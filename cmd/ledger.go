package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/internal/scrub"
	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/ledger"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/results"
)

// newReader reads results.json objects; tests replace it.
var newReader = func() results.Reader { return results.AWS{} }

var ledgerFlags struct {
	by      string
	since   time.Duration
	tasks   bool
	noCache bool
}

// ledgerNotes are printed under every ledger: what its figures are and
// aren't.
var ledgerNotes = []string{
	"Estimates are the pricing-page arithmetic, not a bill. Billed figures come from Cost Explorer, which puff doesn't call yet (#16).",
	"Pending (created, queued, running) and if-billed (cancelled, failed) tasks are kept out of the total. The pricing page doesn't say whether cancelled or failed tasks are billed.",
	"Simulator tasks are billed for their duration, read from each task's results.json in S3; \"at least\" marks a minimum charge where none was read.",
	"Not included: Hybrid Jobs instances, notebook instances, Braket Direct reservations, S3 storage.",
}

var ledgerCmd = &cobra.Command{
	Use:   "ledger",
	Short: "Estimate spend on quantum tasks in every Braket region",
	Long: `Estimate what quantum tasks cost, from every Braket region: all tasks, not
only puff's. The ledger is rebuilt from the service each time. Each task is
priced from its metadata with the embedded price table (QPU: per-task fee plus
shots x per-shot, one fee per program set; SV1 and DM1: the billed duration in
the task's results.json, with the 3 s minimum).

Completed tasks make the total. Pending tasks and cancelled or failed ones
are shown apart and never added in. A task on a device the price table
doesn't price is named, with no figure, and the command exits 1. So does a
region that couldn't be searched, after the rest is shown.

The only local state is a cache of simulator durations already read from S3
($XDG_STATE_HOME/puffball/ledger); deleting it loses nothing.`,
	Example: `  puff ledger
  puff ledger --by device --since 168h
  puff ledger --by day --tasks --json`,
	Args: usageArgs(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := ctxOf(cmd)
		if ledgerFlags.since < 0 {
			return usageErrorf("--since must not be negative")
		}
		valid := false
		for _, b := range ledger.Bys {
			valid = valid || b == ledgerFlags.by
		}
		if !valid {
			return usageErrorf("--by %q: want one of %s", ledgerFlags.by, strings.Join(ledger.Bys, ", "))
		}
		tb, err := price.Load()
		if err != nil {
			return err
		}
		q := pbraket.TaskQuery{}
		since := ""
		if ledgerFlags.since > 0 {
			q.Since = clockNow().Add(-ledgerFlags.since)
			since = q.Since.UTC().Format(time.RFC3339)
		}
		f, err := newFactory(ctx)
		if err != nil {
			return err
		}
		tasks, listErr := listTasks(ctx, f, q)
		listErr = scrub.Error(listErr)
		if tasks == nil {
			return listErr
		}
		o := ledger.Options{Prices: tb, Reader: newReader(), Account: func(ctx context.Context) (string, error) { return newResults().Account(ctx) }}
		if !ledgerFlags.noCache {
			if p, err := ledger.DefaultCachePath(); err == nil {
				o.Cache = &ledger.FileCache{Path: p}
			}
		}
		l, err := ledger.Build(ctx, tasks, o)
		if err != nil {
			return err
		}
		if o.Cache != nil && o.Cache.Err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: the ledger cache isn't working (%v); durations are read from S3 each time\n", o.Cache.Err)
		}
		groups, total, err := l.Group(ledgerFlags.by)
		if err != nil {
			return err
		}
		failed := failedRegions(listErr)
		if jsonOutput {
			out := struct {
				Kind          string            `json:"kind"`
				PricesAsOf    string            `json:"prices_as_of"`
				Since         string            `json:"since,omitempty"`
				By            string            `json:"by"`
				Groups        []ledger.Group    `json:"groups"`
				Total         ledger.Group      `json:"total"`
				Tasks         []ledger.Line     `json:"tasks"`
				Unpriced      []string          `json:"unpriced_devices"`
				Complete      bool              `json:"complete"`
				RegionsFailed map[string]string `json:"regions_failed"`
				Notes         []string          `json:"notes"`
			}{price.Kind, l.PricesAsOf, since, ledgerFlags.by, groups, total, l.Lines, l.Unpriced(), listErr == nil, failed, ledgerNotes}
			for i := range out.Tasks {
				out.Tasks[i].Note = display(out.Tasks[i].Note)
			}
			if err := writeJSON(cmd.OutOrStdout(), out); err != nil {
				return err
			}
		} else {
			writeLedger(cmd.OutOrStdout(), l, groups, total, since, failed)
		}
		return ledgerErr(l, listErr)
	},
}

// ledgerErr is the exit status: a region that couldn't be searched, or a
// task with no price (law 2), is an error after the rest is shown.
func ledgerErr(l *ledger.Ledger, listErr error) error {
	var unpriced error
	if u := l.Unpriced(); len(u) > 0 {
		unpriced = fmt.Errorf("tasks with no estimate, never a guess (prices as of %s): %s", l.PricesAsOf, strings.Join(u, "; "))
	}
	return errors.Join(listErr, unpriced)
}

func money(s ledger.Sum) string {
	if s.Tasks == 0 {
		return "-"
	}
	if s.AtLeast {
		return "at least " + s.USD
	}
	return s.USD
}

func writeLedger(w io.Writer, l *ledger.Ledger, groups []ledger.Group, total ledger.Group, since string, failed map[string]string) {
	scope := "all tasks"
	if since != "" {
		scope = "tasks created since " + since
	}
	_, _ = fmt.Fprintf(w, "ESTIMATE by %s, %s (prices as of %s). An estimate, not a bill.\n", ledgerFlags.by, scope, l.PricesAsOf)
	if len(failed) > 0 {
		regions := make([]string, 0, len(failed))
		for r := range failed {
			regions = append(regions, r)
		}
		sort.Strings(regions)
		_, _ = fmt.Fprintf(w, "INCOMPLETE: %s couldn't be searched; the figures cover the other regions only.\n", strings.Join(regions, ", "))
		total.Key = "TOTAL (incomplete)"
	}
	_, _ = fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "%s\tTASKS\tSHOTS\tESTIMATE\tPENDING\tIF BILLED\tUNPRICED\n", strings.ToUpper(ledgerFlags.by))
	row := func(g ledger.Group) {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n", g.Key, g.Tasks(), g.Shots(), money(g.Completed), money(g.Pending), money(g.IfBilled), count(g.Unpriced))
	}
	for _, g := range groups {
		row(g)
	}
	if total.Key == "total" {
		total.Key = "TOTAL"
	}
	row(total)
	_ = tw.Flush()
	if ledgerFlags.tasks && len(l.Lines) > 0 {
		_, _ = fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "TASK\tREGION\tDEVICE\tSTATUS\tSHOTS\tSTANDING\tESTIMATE\tARITHMETIC")
		for _, ln := range l.Lines {
			est, arith := "-", display(ln.Note)
			if ln.Estimate != nil {
				est, arith = ln.Estimate.USD, ln.Estimate.Arithmetic
				if ln.Note != "" {
					arith += " (" + display(ln.Note) + ")"
				}
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", ln.TaskID, ln.Region, ln.Device, ln.Status, ln.Shots, ln.Standing, est, arith)
		}
		_ = tw.Flush()
	}
	_, _ = fmt.Fprintln(w)
	for _, n := range ledgerNotes {
		_, _ = fmt.Fprintln(w, "- "+n)
	}
}

func count(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n)
}

func init() {
	ledgerCmd.Flags().StringVar(&ledgerFlags.by, "by", "project", "Group by project, device, rung or day (UTC creation date); untagged tasks group as \"untagged\"")
	ledgerCmd.Flags().DurationVar(&ledgerFlags.since, "since", 30*24*time.Hour, "Only tasks created within this long (0: all)")
	ledgerCmd.Flags().BoolVar(&ledgerFlags.tasks, "tasks", false, "Also list each task with its arithmetic")
	ledgerCmd.Flags().BoolVar(&ledgerFlags.noCache, "no-cache", false, "Neither read nor write the local cache of simulator durations")
	rootCmd.AddCommand(ledgerCmd)
}
