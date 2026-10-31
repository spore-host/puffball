package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/internal/scrub"
	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/devices"
	"github.com/spore-host/puffball/pkg/price"
)

// newFactory builds the read-only Braket client factory. Tests replace it
// with one over the G0 fixtures.
var newFactory = pbraket.NewFactory

var devicesFlags struct {
	paradigm string
	all      bool
}

// estimateShots is the shot count the device list prices, per task.
const estimateShots = 1000

var devicesCmd = &cobra.Command{
	Use:   "devices",
	Short: "List every Amazon Braket device in every region",
	Long: `List every Amazon Braket device in every region, from the service:
status, qubits, paradigm, queue depth, execution window, calibration age, and
the estimated cost of one 1,000-shot task (SV1 and DM1: the per-minute rate).

Regions come from the public SSM Braket region list; each is searched. A
region that fails is named, and the command exits 1. Retired devices are
listed with --all, with their replacement where one is known.

Calibration age is from the device's capabilities; IQM and QuEra report no
calibration time, shown as "none" rather than a substitute.`,
	Example: `  puff devices
  puff devices --paradigm analog
  puff devices --all --json`,
	Args: usageArgs(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch devicesFlags.paradigm {
		case "", devices.ParadigmGate, devices.ParadigmAnalog:
		default:
			return usageErrorf("--paradigm must be %s or %s", devices.ParadigmGate, devices.ParadigmAnalog)
		}
		tb, err := price.Load()
		if err != nil {
			return err
		}
		aliases := devices.Aliases{}
		for _, d := range tb.Devices {
			aliases[d.Alias] = d.Arn
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		f, err := newFactory(ctx)
		if err != nil {
			return err
		}
		l, err := devices.Collect(ctx, f, aliases)
		if l == nil {
			return err
		}
		var shown []devices.Device
		for _, d := range l.Devices {
			// A retired device has no paradigm (it isn't read with GetDevice),
			// so --paradigm leaves retired devices out.
			if (devicesFlags.all || d.Status != "RETIRED") && (devicesFlags.paradigm == "" || d.Paradigm == devicesFlags.paradigm) {
				shown = append(shown, d)
			}
		}
		now := clockNow()
		if jsonOutput {
			if werr := writeDevicesJSON(cmd.OutOrStdout(), shown, tb, l, now); werr != nil {
				return werr
			}
		} else {
			writeDevicesTable(cmd.OutOrStdout(), shown, tb, now)
		}
		if w := tb.StaleWarning(now); w != "" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
		}
		// The list is printed either way; a failed region still fails the
		// command (law 3).
		return err
	},
}

// cost is what the list says about a device's price: an estimate for a QPU,
// the per-minute rate for a simulator, or why there is neither. It is never
// silently blank (law 2).
type cost struct {
	cell         string
	estimate     *price.Estimate
	perMinuteUSD string
	err          string
}

func costOf(tb *price.Table, d devices.Device) cost {
	p, err := tb.Lookup(d.Arn)
	if err != nil {
		return cost{cell: "unpriced", err: scrub.Error(err).Error()}
	}
	if p.Simulator() {
		return cost{cell: "$" + p.PerMinuteUSD + "/min", perMinuteUSD: "$" + p.PerMinuteUSD}
	}
	e, err := tb.Estimate(p, price.Request{Programs: 1, Shots: estimateShots})
	if err != nil {
		return cost{cell: "unpriced", err: scrub.Error(err).Error()}
	}
	return cost{cell: e.TotalUSD, estimate: &e}
}

func windowCell(d devices.Device, now time.Time) string {
	s := d.WindowAt(now)
	switch {
	case !s.Known:
		return "?"
	case s.Open:
		return "open"
	case s.Next.IsZero():
		return "none in 7d"
	case s.Next.Sub(now) < 24*time.Hour:
		return "opens " + s.Next.Format("15:04") + "Z"
	default:
		return "opens " + s.Next.Format("Mon 15:04") + "Z"
	}
}

func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// calibrationCell shows the calibration age; when the data spans more than a
// day (Rigetti benchmarks), the newest-to-oldest range.
func calibrationCell(d devices.Device, now time.Time) string {
	c := d.Calibration
	switch {
	case d.Type == "SIMULATOR":
		return "-"
	case c.At == nil:
		return "none"
	case c.Newest != nil && c.Newest.Sub(*c.At) > 24*time.Hour:
		return strings.TrimSuffix(age(now.Sub(*c.Newest)), " ago") + "-" + age(now.Sub(*c.At))
	default:
		return age(now.Sub(*c.At))
	}
}

func writeDevicesTable(w io.Writer, ds []devices.Device, tb *price.Table, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ALIAS\tNAME\tREGION\tSTATUS\tPARADIGM\tQUBITS\tQUEUE\tWINDOW\tCALIBRATED\tESTIMATE")
	for _, d := range ds {
		alias := d.Alias
		if alias == "" {
			alias = "-"
		}
		if d.Status == "RETIRED" {
			repl := "no known replacement"
			if d.ReplacedBy != "" {
				repl = "replaced by " + shortName(d.ReplacedBy)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\tRETIRED\t\t\t\t\t\t%s\n", alias, d.Name, strings.Join(d.Regions, ","), repl)
			continue
		}
		queue := "?"
		if n, ok := d.QueueSize("Normal"); ok {
			queue = fmt.Sprint(n)
		}
		cost := costOf(tb, d).cell
		region := strings.Join(d.Regions, ",")
		if len(d.Regions) > 2 {
			region = fmt.Sprintf("%d regions", len(d.Regions))
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			alias, d.Name, region, d.Status, d.Paradigm, d.Qubits, queue, windowCell(d, now), calibrationCell(d, now), cost)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\nESTIMATE: one task of %d shots (simulators: the per-minute rate), prices as of %s. An estimate, not a bill.\n", estimateShots, tb.AsOf)
	_, _ = fmt.Fprintln(w, "QUEUE: normal-priority quantum tasks waiting. WINDOW and CALIBRATED: UTC, from the device's capabilities.")
}

func shortName(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

// devicesJSON is the --json shape of `puff devices`.
type devicesJSON struct {
	Devices       []deviceJSON      `json:"devices"`
	PricesAsOf    string            `json:"prices_as_of"`
	EstimateShots int               `json:"estimate_shots"`
	RegionsFailed map[string]string `json:"regions_failed"`
	Complete      bool              `json:"complete"`
	Warnings      []string          `json:"warnings"`
}

type deviceJSON struct {
	devices.Device
	WindowOpen *bool           `json:"window_open,omitempty"`
	NextWindow string          `json:"next_window,omitempty"`
	Estimate   *price.Estimate `json:"estimate,omitempty"`
	// PerMinuteUSD is a simulator's rate, as of prices_as_of.
	PerMinuteUSD string `json:"per_minute_usd,omitempty"`
	// EstimateError says why a device has neither, e.g. unpriced.
	EstimateError string `json:"estimate_error,omitempty"`
}

func writeDevicesJSON(w io.Writer, ds []devices.Device, tb *price.Table, l *devices.List, now time.Time) error {
	out := devicesJSON{Devices: []deviceJSON{}, PricesAsOf: tb.AsOf, EstimateShots: estimateShots, RegionsFailed: map[string]string{}, Complete: l.Err == nil, Warnings: []string{}}
	if ie, ok := pbraket.AsIncomplete(l.Err); ok {
		for r, e := range ie.Failed {
			out.RegionsFailed[r] = scrub.Error(e).Error()
		}
	}
	if s := tb.StaleWarning(now); s != "" {
		out.Warnings = append(out.Warnings, s)
	}
	for _, d := range ds {
		dj := deviceJSON{Device: d}
		if d.Status != "RETIRED" {
			if s := d.WindowAt(now); s.Known {
				open := s.Open
				dj.WindowOpen = &open
				if !s.Open && !s.Next.IsZero() {
					dj.NextWindow = s.Next.Format(time.RFC3339)
				}
			}
			c := costOf(tb, d)
			dj.Estimate, dj.PerMinuteUSD, dj.EstimateError = c.estimate, c.perMinuteUSD, c.err
		}
		out.Devices = append(out.Devices, dj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

func init() {
	devicesCmd.Flags().StringVar(&devicesFlags.paradigm, "paradigm", "", "Only gate or analog devices")
	devicesCmd.Flags().BoolVar(&devicesFlags.all, "all", false, "Include retired devices")
	rootCmd.AddCommand(devicesCmd)
}
