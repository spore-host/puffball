package cmd

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestExecuteExitCodes runs Execute in a child process (it calls os.Exit) and
// checks the exit code and that the signal context reached the command.
func TestExecuteExitCodes(t *testing.T) {
	if os.Getenv("PUFF_EXECUTE_HELPER") == "1" {
		os.Args = append([]string{"puff"}, strings.Fields(os.Getenv("PUFF_ARGS"))...)
		Execute()
		return
	}
	for args, want := range map[string]int{"version --json": 0, "bogus": 2, "price ankaa --shots 100": 2} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExecuteExitCodes$")
		cmd.Env = append(os.Environ(), "PUFF_EXECUTE_HELPER=1", "PUFF_ARGS="+args)
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != want {
			t.Errorf("puff %s: exit %d, want %d", args, code, want)
		}
	}
}
