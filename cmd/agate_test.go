package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/results"
	"github.com/spore-host/puffball/pkg/testutil"
)

// agateArgs is the input each billable command needs to reach its gate:
// positional args and flags, without --real. A command that defines --real
// and has no entry here fails TestAGateEveryCommandWithoutReal, so a new
// billable command can't skip A-gate by exiting early on a usage error.
var agateArgs = map[string][]string{
	"puff run": {"../testdata/programs/bell.qasm", "--device", "cepheus", "--shots", "100"},
}

// leafCommands returns every runnable, user-visible command.
func leafCommands(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			if sub.HasSubCommands() {
				walk(sub)
				continue
			}
			out = append(out, sub)
		}
	}
	walk(root)
	return out
}

// resetFlags puts every flag of c (and the root's persistent flags) back to
// its default, so one run's flags don't leak into the next.
func resetFlags(c *cobra.Command) {
	reset := func(f *pflag.Flag) {
		// Set("[]") on a slice flag would make a one-element list.
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	}
	c.Flags().VisitAll(reset)
	rootCmd.PersistentFlags().VisitAll(reset)
}

// TestAGateEveryCommandWithoutReal is A-gate's harness (#8). Every command
// runs against a gate whose creator fails the test on any billable call.
// Commands without --real run once with --json (which also keeps `version`
// off the network). Commands with --real run with their agateArgs three ways
// (no --real; --real --dry-run; --real --budget 0), and each run must open
// the gate: a billable command that exits before its gate would otherwise
// pass for the wrong reason.
func TestAGateEveryCommandWithoutReal(t *testing.T) {
	ensureI18nInitialized()
	opened := 0
	orig := openGate
	t.Cleanup(func() { openGate = orig })
	openGate = func(c *cobra.Command, f gateFlags) (*gate.Gate, error) {
		opened++
		o := gateOptions(c, f)
		o.Creators = testutil.FailingCreators(t)
		o.Tokens = &gate.MemTokens{}
		return gate.New(o)
	}

	// Read-only commands get the G0 fixtures, so the harness never calls AWS.
	origFactory := newFactory
	t.Cleanup(func() { newFactory = origFactory })
	newFactory = func(context.Context) (pbraket.Factory, error) {
		return testutil.DevicesFactory(t, "../testdata/devices"), nil
	}

	// A billable command must not reach STS or S3 before its gate approves:
	// any results-store call fails the test.
	origResults := newResults
	t.Cleanup(func() { newResults = origResults })
	newResults = func() results.Store { return failingResults{t} }

	leaves := leafCommands(rootCmd)
	if len(leaves) == 0 {
		t.Fatal("no commands found; a harness that runs nothing must not pass")
	}
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })
	runOnce := func(c *cobra.Command, args []string) int {
		resetFlags(c)
		var out, errb bytes.Buffer
		rootCmd.SetOut(&out)
		rootCmd.SetErr(&errb)
		code := run(rootCmd, args, &errb)
		t.Logf("%v -> exit %d", args, code)
		return code
	}
	for _, c := range leaves {
		path := strings.Fields(strings.TrimPrefix(c.CommandPath(), rootCmd.Name()+" "))
		if c.Flags().Lookup("real") == nil {
			runOnce(c, append(path, "--json"))
			continue
		}
		fixture, ok := agateArgs[c.CommandPath()]
		if !ok {
			t.Errorf("%s defines --real but has no agateArgs entry; A-gate can't reach its gate without one", c.CommandPath())
			continue
		}
		for _, extra := range [][]string{nil, {"--real", "--dry-run"}, {"--real", "--budget", "0"}} {
			before := opened
			args := append(append(append([]string{}, path...), fixture...), extra...)
			runOnce(c, args)
			if opened == before {
				t.Errorf("%v exited without opening the gate; fix its agateArgs", args)
			}
		}
	}
}

func TestAddGateFlags(t *testing.T) {
	var f gateFlags
	c := &cobra.Command{Use: "x"}
	addGateFlags(c, &f)
	if err := c.Flags().Parse([]string{"--real", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if o := gateOptions(c, f); !o.Real || !o.DryRun || o.Budget != nil {
		t.Errorf("without --budget: %+v (a budget must be nil when not given)", o)
	}
	if err := c.Flags().Parse([]string{"--budget", "0"}); err != nil {
		t.Fatal(err)
	}
	if o := gateOptions(c, f); o.Budget == nil || *o.Budget != 0 {
		t.Errorf("--budget 0 must be a budget of zero, got %v", o.Budget)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if g, err := openGate(c, f); err != nil || g == nil {
		t.Fatalf("openGate: %v", err)
	}
}

// failingResults fails the test on any STS or S3 call.
type failingResults struct{ t *testing.T }

func (f failingResults) Account(context.Context) (string, error) {
	f.t.Error("STS called before the gate approved")
	return "", errors.New("no")
}
func (f failingResults) Exists(context.Context, string, string, string) (bool, error) {
	f.t.Error("S3 called before the gate approved")
	return false, errors.New("no")
}
func (f failingResults) Create(context.Context, string, string) error {
	f.t.Error("S3 CreateBucket called before the gate approved")
	return errors.New("no")
}
