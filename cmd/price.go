package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/pkg/price"
)

var priceFlags struct {
	shots    int64
	tasks    int
	pack     int
	duration time.Duration
}

// clockNow is the clock for staleness, windows and calibration ages; tests replace it.
var clockNow = time.Now

var priceCmd = &cobra.Command{
	Use:   "price <device>",
	Short: "Print the estimated cost of a run before anything runs",
	Long: `Print the pricing-page arithmetic for running programs on a device.

The figure is an estimate (the price table's arithmetic, as of its date). It is
not a bill: billing arrives later and can differ. Nothing is submitted and no
AWS call is made.

QPUs:       $0.30 per task + shots x the device's per-shot price.
--pack K:   on gate-based QPUs, packs up to K programs into one program set,
            which pays one per-task fee.
SV1, DM1:   per minute of billed duration, with a 3 s minimum per task. The
            duration isn't known before a run; without --duration the
            estimate is the minimum.`,
	Example: `  puff price cepheus --shots 1000
  puff price cepheus --shots 1000 --tasks 500 --pack 5
  puff price sv1 --duration 10s`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		tb, err := price.Load()
		if err != nil {
			return err
		}
		d, err := tb.Lookup(args[0])
		if err != nil {
			return usageError{err}
		}
		if !cmd.Flags().Changed("shots") && !d.Simulator() {
			return usageErrorf("--shots is required for %s", d.Alias)
		}
		e, err := tb.Estimate(d, price.Request{Programs: priceFlags.tasks, Shots: priceFlags.shots, Pack: priceFlags.pack, Duration: priceFlags.duration})
		if err != nil {
			return usageError{err}
		}
		warning := tb.StaleWarning(clockNow())
		if jsonOutput {
			return writePriceJSON(cmd.OutOrStdout(), d, e, warning)
		}
		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "%s (%s)\n", d.Name, d.Alias)
		_, _ = fmt.Fprintf(out, "estimate (prices as of %s): %s\n", e.PricesAsOf, e.Arithmetic)
		_, _ = fmt.Fprintf(out, "This is an estimate, not a bill; nothing was submitted.\n")
		if warning != "" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", warning)
		}
		return nil
	},
}

// priceJSON is the --json shape of `puff price`.
type priceJSON struct {
	price.Estimate
	Name     string   `json:"device_name"`
	Warnings []string `json:"warnings"`
}

func writePriceJSON(w io.Writer, d price.Device, e price.Estimate, warning string) error {
	p := priceJSON{Estimate: e, Name: d.Name, Warnings: []string{}}
	if warning != "" {
		p.Warnings = append(p.Warnings, warning)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

func init() {
	priceCmd.Flags().Int64Var(&priceFlags.shots, "shots", 0, "Shots per program")
	priceCmd.Flags().IntVar(&priceFlags.tasks, "tasks", 1, "Number of programs (each a task, unless --pack)")
	priceCmd.Flags().IntVar(&priceFlags.pack, "pack", 0, "Pack up to this many programs into each program set (gate-based QPUs)")
	priceCmd.Flags().DurationVar(&priceFlags.duration, "duration", 0, "Expected billed duration per task, for SV1 and DM1 (e.g. 10s)")
	rootCmd.AddCommand(priceCmd)
}
