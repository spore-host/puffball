package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestRoot builds a root wired like rootCmd (silenced errors, flag errors
// marked as usage errors) with one subcommand whose RunE returns err.
func newTestRoot(t *testing.T, err error) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "puff", SilenceErrors: true, SilenceUsage: true}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return flagError{err} })
	root.AddCommand(&cobra.Command{
		Use:  "do",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return err },
	})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	return root
}

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name     string
		cmdErr   error
		args     []string
		wantCode int
		wantErr  string
	}{
		{"success", nil, []string{"do"}, exitOK, ""},
		{"runtime error", errors.New("boom"), []string{"do"}, exitError, "boom"},
		{"usage error from a command", usageErrorf("need --device"), []string{"do"}, exitUsage, "need --device"},
		{"unknown flag", nil, []string{"do", "--nope"}, exitUsage, "unknown flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got := run(newTestRoot(t, tc.cmdErr), tc.args, &stderr)
			if got != tc.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", got, tc.wantCode, stderr.String())
			}
			if tc.wantErr != "" && !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantErr)
			}
			if tc.wantErr == "" && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty on success", stderr.String())
			}
		})
	}
}

func TestUsageErrorUnwraps(t *testing.T) {
	inner := errors.New("inner")
	err := usageError{inner}
	if !errors.Is(err, inner) {
		t.Error("usageError does not unwrap to its cause")
	}
	if !errors.Is(flagError{inner}, inner) {
		t.Error("flagError does not unwrap to its cause")
	}
}

func TestRootHasNoUnhiddenStubs(t *testing.T) {
	ensureI18nInitialized()
	for _, c := range rootCmd.Commands() {
		if c.Short == "" && !c.Hidden {
			t.Errorf("command %q has no Short description", c.Name())
		}
	}
}
