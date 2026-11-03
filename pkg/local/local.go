// Package local is puff's local rung: an OpenQASM 3 program run on the Braket
// default simulator through uv. It makes no AWS call and looks up no
// credentials (A-offline, #9): it imports no AWS package; the simulator runs
// in a fresh temp directory under python -I, with AWS_*, PYTHON* and
// VIRTUAL_ENV removed, the AWS config files pointed at the null device and
// instance metadata off; and the runner refuses to report if an AWS SDK
// loaded.
//
// uv runs with --isolated --no-config, so a project, workspace or uv.toml
// around the user doesn't change the run. It still searches the directories
// above the temp directory for a virtual environment when it looks for an
// interpreter (measured with a broken ~/.venv); a temp directory under such a
// directory makes uv fail, loudly.
package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spore-host/puffball/python"
)

// SimulatorPackage, SimulatorVersion and PythonVersion are the pinned
// simulator and interpreter, the versions measured in G0(d) (#5). Python is
// pinned because uv otherwise follows any .python-version file above the
// working directory (measured: a ~/.python-version of 3.12.2 was picked up,
// and numba has no cp312 wheel in the cache).
const (
	SimulatorPackage = "amazon-braket-default-simulator"
	SimulatorVersion = "1.41.1"
	PythonVersion    = "3.14"
)

// offlineMisses are uv's messages when --offline can't find what it needs
// (uv 0.13.6, measured 2026-10-31): the simulator not in the cache ("hint:
// Packages were unavailable because the network was disabled. ..."), and
// Python 3.14 not installed ("hint: A managed Python download is available
// for Python 3.14, but uv is set to offline mode").
var offlineMisses = []string{"network was disabled", "uv is set to offline mode"}

// Options configure a run.
type Options struct {
	// UV is the uv binary; "" finds it on PATH.
	UV string
	// Offline passes --offline to uv: use only the uv cache, never the
	// network. The pinned simulator must have been fetched once.
	Offline bool
	// Stderr receives uv's own messages (downloads, resolution). Nil
	// discards them; on failure they are included in the error.
	Stderr io.Writer
}

// Result is what the runner printed.
type Result struct {
	Device          string           `json:"device"`
	Shots           int64            `json:"shots"`
	MeasuredQubits  []int            `json:"measured_qubits"`
	Counts          map[string]int64 `json:"counts"`
	SimulateSeconds float64          `json:"simulate_seconds"`
	// Software is every package in the simulator's closure, Python, and uv.
	Software map[string]string `json:"software"`
	// Missing names what the run could not record (law 6).
	Missing []string `json:"missing"`
}

// ErrNoUV means uv isn't installed.
var ErrNoUV = errors.New("uv is not installed: the local simulator runs through uv (https://docs.astral.sh/uv/getting-started/installation/)")

// FindUV returns the uv binary to use.
func FindUV(o Options) (string, error) {
	if o.UV != "" {
		return o.UV, nil
	}
	p, err := exec.LookPath("uv")
	if err != nil {
		return "", ErrNoUV
	}
	return p, nil
}

// Command returns the uv arguments that run script on program. python -I
// ignores PYTHON* variables and the user's site directory, so nothing outside
// the uv environment can be imported.
func Command(script, program string, shots int64, offline bool) []string {
	args := []string{"run", "--isolated", "--no-config", "--no-project", "--python", PythonVersion}
	if offline {
		args = append(args, "--offline")
	}
	return append(args, "--with", SimulatorPackage+"=="+SimulatorVersion,
		"python", "-I", script, program, "--shots", fmt.Sprint(shots))
}

// Env returns env with every AWS_*, PYTHON* and VIRTUAL_ENV variable removed,
// the AWS config files pointed at the null device, and instance metadata off,
// so nothing in the simulator's process can find credentials, a profile or a
// region (A-offline, #9; the same settings G0(d) measured with, #5).
func Env(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "AWS_") && !strings.HasPrefix(kv, "PYTHON") && !strings.HasPrefix(kv, "VIRTUAL_ENV=") {
			out = append(out, kv)
		}
	}
	return append(out, "AWS_CONFIG_FILE="+os.DevNull, "AWS_SHARED_CREDENTIALS_FILE="+os.DevNull, "AWS_EC2_METADATA_DISABLED=true")
}

// bitString is a measured outcome: one 0 or 1 per measured qubit.
var bitString = regexp.MustCompile(`^[01]+$`)

// check holds a result to what was asked for: the shots measured, counts that
// add up to them, and outcomes with one bit per measured qubit.
func (r Result) check(shots int64) error {
	if r.Shots != shots {
		return fmt.Errorf("local simulator measured %d shots, want %d", r.Shots, shots)
	}
	var sum int64
	for k, n := range r.Counts {
		if !bitString.MatchString(k) || len(k) != len(r.MeasuredQubits) {
			return fmt.Errorf("local simulator outcome %q is not %d bits", k, len(r.MeasuredQubits))
		}
		if n < 0 {
			return fmt.Errorf("local simulator outcome %q has a negative count", k)
		}
		sum += n
	}
	if sum != shots {
		return fmt.Errorf("local simulator counts add up to %d, want %d", sum, shots)
	}
	return nil
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// uvVersion returns `uv --version`'s output, or "" if it can't be read.
func uvVersion(ctx context.Context, uv string) string {
	// uv is exec.LookPath("uv") or a test's fake; no user input names it.
	out, err := exec.CommandContext(ctx, uv, "--version").Output() // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- uv's path, not user input
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Run runs an OpenQASM 3 file on the local simulator.
func Run(ctx context.Context, program string, shots int64, o Options) (Result, error) {
	src, err := os.ReadFile(program)
	if err != nil {
		return Result{}, fmt.Errorf("program: %w", err)
	}
	return RunSource(ctx, src, shots, o)
}

// RunSource runs OpenQASM 3 source on the local simulator. The simulator
// reads a copy written for it, so what ran is exactly src (a caller can hash
// src and know it is what ran).
func RunSource(ctx context.Context, src []byte, shots int64, o Options) (Result, error) {
	if shots < 1 {
		return Result{}, errors.New("the local simulator needs at least one shot")
	}
	uv, err := FindUV(o)
	if err != nil {
		return Result{}, err
	}
	dir, err := os.MkdirTemp("", "puff-local-")
	if err != nil {
		return Result{}, fmt.Errorf("local runner: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	script := filepath.Join(dir, "puff_local.py")
	if err := os.WriteFile(script, python.Runner, 0o600); err != nil {
		return Result{}, fmt.Errorf("local runner: %w", err)
	}
	prog := filepath.Join(dir, "program.qasm")
	if err := os.WriteFile(prog, src, 0o600); err != nil {
		return Result{}, fmt.Errorf("local runner: %w", err)
	}

	var stdout, stderr bytes.Buffer
	// uv is exec.LookPath("uv") or a test's fake; the arguments are puff's own
	// temp-file paths and an integer. The user's program reaches the
	// simulator only as file contents, never as an argument.
	cmd := exec.CommandContext(ctx, uv, Command(script, prog, shots, o.Offline)...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- see above
	cmd.Dir = dir                                                                   // nothing in the user's directory is imported
	cmd.Env = Env(os.Environ())
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// On cancel, stop uv and the Python it started, and don't wait forever
	// for pipes a straggler holds.
	ownProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	if o.Stderr != nil {
		_, _ = o.Stderr.Write(stderr.Bytes())
	}
	if runErr != nil && ctx.Err() != nil {
		return Result{}, fmt.Errorf("local simulator interrupted: %w", ctx.Err())
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if o.Offline && containsAny(msg, offlineMisses) {
			msg += fmt.Sprintf("\n(--offline uses only uv's cache: run once without --offline to fetch %s==%s for Python %s)", SimulatorPackage, SimulatorVersion, PythonVersion)
		}
		return Result{}, fmt.Errorf("local simulator: %w\n%s", runErr, msg)
	}
	var r Result
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return Result{}, fmt.Errorf("local simulator printed something other than its JSON result: %w\n%s", err, strings.TrimSpace(stdout.String()))
	}
	if err := r.check(shots); err != nil {
		return Result{}, err
	}
	if r.Software == nil {
		r.Software = map[string]string{}
	}
	if v := uvVersion(ctx, uv); v != "" {
		r.Software["uv"] = v
	} else {
		r.Missing = append(r.Missing, "software.uv")
	}
	return r, nil
}

// Outcomes returns the counts' bit strings, most frequent first, ties by bit
// string.
func (r Result) Outcomes() []string {
	out := make([]string, 0, len(r.Counts))
	for k := range r.Counts {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if r.Counts[out[i]] != r.Counts[out[j]] {
			return r.Counts[out[i]] > r.Counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// Cached reports whether the pinned simulator and Python are in uv's cache:
// it imports the simulator through uv with --offline, so it never touches the
// network, and returns the version that loaded. A cold run needs the network
// (#5); this is how puff doctor tells.
func Cached(ctx context.Context, o Options) (string, error) {
	uv, err := FindUV(o)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "puff-cached-")
	if err != nil {
		return "", fmt.Errorf("uv cache check: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	args := []string{"run", "--isolated", "--no-config", "--no-project", "--python", PythonVersion, "--offline",
		"--with", SimulatorPackage + "==" + SimulatorVersion, "python", "-I", "-c",
		"import importlib.metadata as m, braket.default_simulator; print(m.version('" + SimulatorPackage + "'))"}
	var stdout, stderr bytes.Buffer
	// uv is exec.LookPath("uv") or a test's fake; the arguments are constants.
	cmd := exec.CommandContext(ctx, uv, args...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- uv's path and constant arguments
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, Env(os.Environ()), &stdout, &stderr
	ownProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if containsAny(msg, offlineMisses) {
			return "", fmt.Errorf("%s==%s for Python %s isn't in uv's cache: %s", SimulatorPackage, SimulatorVersion, PythonVersion, lastLine(msg))
		}
		return "", fmt.Errorf("uv: %w: %s", err, lastLine(msg))
	}
	v := strings.TrimSpace(stdout.String())
	if v != SimulatorVersion {
		return "", fmt.Errorf("uv loaded %s %q, want %s", SimulatorPackage, v, SimulatorVersion)
	}
	return v, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
