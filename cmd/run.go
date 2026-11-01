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
	"time"

	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/pkg/local"
)

var runFlags struct {
	shots        int64
	offline      bool
	device       string
	deviceRegion string
	duration     time.Duration
	project      string
	runID        string
	tags         []string
	gate         gateFlags
}

// localUV is the uv binary the local rung uses; "" finds it on PATH. Tests
// point it at a fake.
var localUV = ""

var runCmd = &cobra.Command{
	Use:   "run <file>",
	Short: "Run a program on the local simulator, or on Amazon Braket with --device",
	Long: `Run a program.

Without --device: the local rung. An OpenQASM 3 (.qasm) program runs on the
Braket default simulator on this machine: no AWS call, no credentials, not
billable. It runs the pinned amazon-braket-default-simulator on Python 3.14
through uv (fetched once, then cached). --offline uses only uv's cache.

With --device: Amazon Braket. The device is found in the live service, the
run is priced, and the estimate is printed. Nothing is submitted without
--real. With --real it is submitted to the device's region, with results in
that region's default bucket (amazon-braket-<region>-<account>, created on a
--real run if it doesn't exist, and reported), under puffball/<run-id>/.
Every task is tagged puffball:project, puffball:rung, puffball:run-id and
puffball:est-usd. Repeating a run id retries the same request; it never
creates a second task.

SV1 and DM1 bill run time, which isn't known in advance: their estimate is the
minimum, and --budget needs --duration as an upper bound.`,
	Example: `  puff run bell.qasm --shots 1000
  puff run bell.qasm --device cepheus --shots 100            # print the estimate, submit nothing
  puff run bell.qasm --device cepheus --shots 100 --real
  puff run bell.qasm --device sv1 --shots 100 --real --project fmo --tag owner=me`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		program := args[0]
		if runFlags.shots < 1 {
			return usageErrorf("--shots must be at least 1")
		}
		if runFlags.device == "" {
			for _, f := range []string{"real", "dry-run", "budget", "device-region", "duration", "project", "run-id", "tag"} {
				if cmd.Flags().Changed(f) {
					return usageErrorf("--%s applies only with --device (the local simulator is not billable)", f)
				}
			}
		}
		if ext := strings.ToLower(filepath.Ext(program)); runFlags.device == "" && ext != ".qasm" {
			return usageErrorf("%s: the local simulator runs OpenQASM 3 (.qasm) programs", program)
		}
		src, err := os.ReadFile(program)
		if err != nil {
			return fmt.Errorf("program: %w", err)
		}
		if runFlags.device != "" {
			s := sha256.Sum256(src)
			return runOnBraket(cmd, program, src, hex.EncodeToString(s[:]))
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
	runCmd.Flags().BoolVar(&runFlags.offline, "offline", false, "Local: use only uv's cache; never the network")
	runCmd.Flags().StringVar(&runFlags.device, "device", "", "Run on this Amazon Braket device (alias, name or ARN) instead of locally")
	runCmd.Flags().StringVar(&runFlags.deviceRegion, "device-region", "", "For a simulator offered in several regions, submit in this one")
	runCmd.Flags().DurationVar(&runFlags.duration, "duration", 0, "SV1/DM1: expected billed duration, for the estimate and --budget")
	runCmd.Flags().StringVar(&runFlags.project, "project", "default", "Value of the puffball:project tag")
	runCmd.Flags().StringVar(&runFlags.runID, "run-id", "", "Run id (default: a new one); repeat one to retry without creating a second task")
	runCmd.Flags().StringArrayVar(&runFlags.tags, "tag", nil, "Extra task tag key=value (repeatable)")
	addGateFlags(runCmd, &runFlags.gate)
	runFlags.gate.pinned = pinnedTags
	rootCmd.AddCommand(runCmd)
}
