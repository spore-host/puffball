package cmd

import (
	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/pkg/gate"
)

// gateFlags are the flags every billable command takes.
type gateFlags struct {
	real   bool
	dryRun bool
	budget float64
}

// addGateFlags registers --real, --dry-run and --budget on a billable command.
func addGateFlags(c *cobra.Command, f *gateFlags) {
	c.Flags().BoolVar(&f.real, "real", false, "Spend money: submit to Amazon Braket (default is to print the estimate and stop)")
	c.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the estimate and stop, even with --real")
	c.Flags().Float64Var(&f.budget, "budget", 0, "Refuse the run if its estimate exceeds this many US dollars (0 refuses any spend)")
}

// gateOptions turns a command's gate flags into gate.Options. --budget counts
// only when given, so --budget 0 means "spend nothing", not "no budget".
func gateOptions(c *cobra.Command, f gateFlags) gate.Options {
	o := gate.Options{Real: f.real, DryRun: f.dryRun, Out: c.ErrOrStderr()}
	if c.Flags().Changed("budget") {
		b := f.budget
		o.Budget = &b
	}
	return o
}

// openGate builds the gate for a billable command, with the real AWS clients.
// It is a variable so the A-gate test (#8) can swap in a creator that fails on
// any billable call.
var openGate = func(c *cobra.Command, f gateFlags) (*gate.Gate, error) {
	dir, err := gate.DefaultTokenDir()
	if err != nil {
		return nil, err
	}
	o := gateOptions(c, f)
	o.Tokens = gate.FileTokens{Dir: dir}
	return gate.New(o)
}
