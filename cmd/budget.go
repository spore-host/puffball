package cmd

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/budget"
	"github.com/spore-host/puffball/pkg/devices"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/price"
)

// newLimiters is the spending-limit client factory; tests replace it.
var newLimiters = gate.AWSLimiters

var budgetFlags struct {
	usd     string
	until   string
	from    string
	allQPUs bool
	project string
	others  bool
}

var budgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Set, list and remove Amazon Braket spending limits on QPUs",
	Long: `Braket spending limits, wrapped: puff sets, lists and removes them, and
Braket enforces them. A limit is per QPU, in the QPU's region. Braket
checks each new task's estimated cost against the remaining budget (limit
minus current spend minus queued spend), and rejects a task that doesn't
fit, or any task outside the limit's --from/--until period. Only tasks
submitted while a limit exists count against it.

Spending limits cover QPU tasks only: not simulators, notebooks, Hybrid Job
instances or Braket Direct reservations (Amazon Braket Developer Guide).

Setting or removing a limit isn't billed. puff leaves a limit it didn't
create alone unless --include-others is given.`,
}

func parseWhen(flag, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, usageErrorf("%s %q: want a date (2026-12-31, UTC midnight) or an RFC 3339 time", flag, s)
}

// budgetTargets resolves the command's devices, from the live device list.
func budgetTargets(cmd *cobra.Command, args []string) ([]budget.Target, pbraket.Factory, error) {
	if len(args) == 0 && !budgetFlags.allQPUs {
		return nil, nil, usageErrorf("name a QPU (alias, name or ARN), or pass --all-qpus")
	}
	ctx := ctxOf(cmd)
	tb, err := price.Load()
	if err != nil {
		return nil, nil, err
	}
	aliases := devices.Aliases{}
	for _, d := range tb.Devices {
		aliases[d.Alias] = d.Arn
	}
	f, err := newFactory(ctx)
	if err != nil {
		return nil, nil, err
	}
	l, collectErr := devices.Collect(ctx, f, aliases)
	if l == nil {
		return nil, nil, collectErr
	}
	if collectErr != nil {
		if budgetFlags.allQPUs {
			// --all-qpus over a partial list would silently skip QPUs (law 3).
			return nil, nil, fmt.Errorf("--all-qpus needs every region: %w", collectErr)
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", collectErr.Error())
	}
	ts, err := budget.Targets(l, aliases, args, budgetFlags.allQPUs)
	if err != nil {
		return nil, nil, usageError{err}
	}
	return ts, f, nil
}

func writeChanges(cmd *cobra.Command, cs []budget.Change) error {
	if jsonOutput {
		if cs == nil {
			cs = []budget.Change{}
		}
		if err := writeJSON(cmd.OutOrStdout(), map[string]any{"changes": cs}); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "DEVICE\tREGION\tACTION\tLIMIT")
		for _, c := range cs {
			limit := ""
			switch {
			case c.Before != "" && c.After != "" && c.Before != c.After:
				limit = "$" + c.Before + " -> $" + c.After
			case c.After != "":
				limit = "$" + c.After
			case c.Before != "":
				limit = "$" + c.Before
			}
			action := c.Action
			if c.NotPuffs && c.Action == "kept" {
				action = "kept (not created by puff; --include-others to change it)"
			}
			if c.Error != "" {
				action += ": " + display(c.Error)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, c.Region, action, limit)
		}
		_ = tw.Flush()
	}
	if budget.Failed(cs) {
		return fmt.Errorf("some spending limits weren't changed (see above)")
	}
	return nil
}

var budgetSetCmd = &cobra.Command{
	Use:   "set [<device>...] --usd <amount>",
	Short: "Set a spending limit on QPUs, creating or updating it",
	Example: `  puff budget set cepheus --usd 1
  puff budget set --all-qpus --usd 5 --until 2026-12-31`,
	Args: usageArgs(cobra.ArbitraryArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		if budgetFlags.usd == "" {
			return usageErrorf("--usd is required")
		}
		if err := budget.CheckUSD(budgetFlags.usd); err != nil {
			return usageErrorf("--usd %v", err)
		}
		if err := checkTag(budget.Tag, budgetFlags.project); err != nil || budgetFlags.project == "" {
			return usageErrorf("--project: want a non-empty tag value")
		}
		from, err := parseWhen("--from", budgetFlags.from)
		if err != nil {
			return err
		}
		until, err := parseWhen("--until", budgetFlags.until)
		if err != nil {
			return err
		}
		period, err := budget.Period(from, until, clockNow())
		if err != nil {
			return usageError{err}
		}
		ts, f, err := budgetTargets(cmd, args)
		if err != nil {
			return err
		}
		return writeChanges(cmd, budget.Set(ctxOf(cmd), f, newLimiters(), ts, budgetFlags.usd, period, budgetFlags.project, budgetFlags.others))
	},
}

var budgetRmCmd = &cobra.Command{
	Use:     "rm [<device>...]",
	Short:   "Remove the spending limit from QPUs",
	Example: `  puff budget rm cepheus`,
	Args:    usageArgs(cobra.ArbitraryArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		ts, f, err := budgetTargets(cmd, args)
		if err != nil {
			return err
		}
		return writeChanges(cmd, budget.Remove(ctxOf(cmd), f, newLimiters(), ts, budgetFlags.others))
	},
}

var budgetLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List spending limits in every Braket region",
	Long: `List every spending limit in every Braket region: the limit, Braket's
current and queued spend against it, the remaining budget, and its period.
The spend figures are Braket's spending-limit accounting (the estimated cost
of tasks submitted while the limit exists), not a bill. A region that fails
is named, and the command exits 1 after listing the rest.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := ctxOf(cmd)
		f, err := newFactory(ctx)
		if err != nil {
			return err
		}
		inv, derr := pbraket.Discover(ctx, f)
		if inv == nil {
			return derr
		}
		ls, serr := pbraket.SearchLimits(ctx, f, inv.Regions())
		listErr := mergeIncomplete("list spending limits", derr, serr)
		views := budget.Views(ls)
		if jsonOutput {
			if err := writeJSON(cmd.OutOrStdout(), map[string]any{"limits": views, "complete": listErr == nil, "regions_failed": failedRegions(listErr),
				"note": "spent and queued are Braket's spending-limit accounting: its estimated cost of tasks submitted while the limit exists, not a bill"}); err != nil {
				return err
			}
			return listErr
		}
		writeLimits(cmd.OutOrStdout(), views)
		if len(views) == 0 {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "no spending limits")
		}
		return listErr
	},
}

func writeLimits(w io.Writer, vs []budget.View) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "DEVICE\tREGION\tLIMIT\tSPENT\tQUEUED\tREMAINING\tPERIOD\tBY PUFF")
	for _, v := range vs {
		period := "always"
		if v.From != nil || v.Until != nil {
			period = when(v.From) + " to " + when(v.Until)
		}
		by := "no"
		if v.ByPuff {
			by = "yes (" + dash(v.Project) + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t$%s\t$%s\t$%s\t%s\t%s\t%s\n", v.Device, v.Region, v.LimitUSD, v.SpentUSD, v.QueuedUSD, dash(v.Remaining), period, by)
	}
	_ = tw.Flush()
	if len(vs) > 0 {
		_, _ = fmt.Fprintln(w, "\nSPENT and QUEUED are Braket's spending-limit accounting: its estimated cost of tasks submitted while the limit exists. Not a bill.")
		for _, v := range vs {
			if len(v.Missing) > 0 {
				_, _ = fmt.Fprintf(w, "%s: the service didn't report %s\n", v.Device, strings.Join(v.Missing, ", "))
			}
		}
	}
}

func init() {
	for _, c := range []*cobra.Command{budgetSetCmd, budgetRmCmd} {
		c.Flags().BoolVar(&budgetFlags.allQPUs, "all-qpus", false, "Every QPU that isn't RETIRED, in every region")
		c.Flags().BoolVar(&budgetFlags.others, "include-others", false, "Also change limits puff didn't create")
	}
	budgetSetCmd.Flags().StringVar(&budgetFlags.usd, "usd", "", "The limit in US dollars (e.g. 1 or 2.50; 0 blocks every task)")
	budgetSetCmd.Flags().StringVar(&budgetFlags.until, "until", "", "End of the limit's period (date or RFC 3339); outside it Braket rejects every task")
	budgetSetCmd.Flags().StringVar(&budgetFlags.from, "from", "", "Start of the period (default now; needs --until)")
	budgetSetCmd.Flags().StringVar(&budgetFlags.project, "project", "default", "puffball:project tag on limits puff creates")
	budgetCmd.AddCommand(budgetSetCmd, budgetLsCmd, budgetRmCmd)
	rootCmd.AddCommand(budgetCmd)
}
