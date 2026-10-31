package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/pkg/local"
)

var runFlags struct {
	shots   int64
	offline bool
}

// localUV is the uv binary the local rung uses; "" finds it on PATH. Tests
// point it at a fake.
var localUV = ""

var runCmd = &cobra.Command{
	Use:   "run <file.qasm>",
	Short: "Run an OpenQASM 3 program on the local simulator",
	Long: `Run an OpenQASM 3 program on the Braket default simulator, on this machine.

This is the local rung: no AWS call, no credentials, not billable. It runs
the pinned amazon-braket-default-simulator on Python 3.14 through uv
(fetched once, then cached). With --offline, uv uses only its cache and
never the network.

Running on Amazon Braket (--device, --real) arrives with #39.`,
	Example: `  puff run bell.qasm --shots 1000
  puff run bell.qasm --shots 1000 --offline --json`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		program := args[0]
		if ext := strings.ToLower(filepath.Ext(program)); ext != ".qasm" {
			return usageErrorf("%s: the local simulator runs OpenQASM 3 (.qasm) programs", program)
		}
		if runFlags.shots < 1 {
			return usageErrorf("--shots must be at least 1")
		}
		src, err := os.ReadFile(program)
		if err != nil {
			return fmt.Errorf("program: %w", err)
		}
		abs, err := filepath.Abs(program)
		if err != nil {
			return fmt.Errorf("program: %w", err)
		}
		// The bytes hashed are the bytes run: RunSource hands the simulator
		// a copy of src.
		sum := sha256.Sum256(src)
		var uvLog bytes.Buffer
		r, err := local.RunSource(cmd.Context(), src, runFlags.shots, local.Options{UV: localUV, Offline: runFlags.offline, Stderr: &uvLog})
		if verbose && uvLog.Len() > 0 {
			_, _ = cmd.ErrOrStderr().Write(uvLog.Bytes())
		}
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeRunJSON(cmd, runJSON{Rung: "local", Program: abs, ProgramSHA256: hex.EncodeToString(sum[:]), Billable: false,
				Simulator: local.SimulatorPackage + "==" + local.SimulatorVersion, Result: r})
		}
		ver, ok := r.Software[local.SimulatorPackage]
		if !ok {
			ver = "?"
		}
		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "%s: %d shots on the local simulator (%s %s): no AWS call, not billable\n\n",
			program, r.Shots, local.SimulatorPackage, ver)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "OUTCOME\tCOUNT\tFRACTION")
		for _, o := range r.Outcomes() {
			_, _ = fmt.Fprintf(tw, "%s\t%d\t%.3f\n", o, r.Counts[o], float64(r.Counts[o])/float64(r.Shots))
		}
		_ = tw.Flush()
		return nil
	},
}

// runJSON is the --json shape of `puff run` on the local rung.
type runJSON struct {
	Rung          string       `json:"rung"`
	Program       string       `json:"program"`        // absolute path
	ProgramSHA256 string       `json:"program_sha256"` // of the file as run
	Billable      bool         `json:"billable"`
	Simulator     string       `json:"simulator"`
	PuffVersion   string       `json:"puff_version"`
	Result        local.Result `json:"result"`
}

func writeRunJSON(cmd *cobra.Command, v runJSON) error {
	v.PuffVersion = Version
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

func init() {
	runCmd.Flags().Int64Var(&runFlags.shots, "shots", 1000, "Shots")
	runCmd.Flags().BoolVar(&runFlags.offline, "offline", false, "Use only uv's cache; never the network")
	rootCmd.AddCommand(runCmd)
}
