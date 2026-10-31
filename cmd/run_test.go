package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/puffball/pkg/awscfg"
)

const bellQASM = "../testdata/programs/bell.qasm"

// fakeLocalUV points the local rung at a script that prints a fixed result.
func fakeLocalUV(t *testing.T) {
	t.Helper()
	uv := filepath.Join(t.TempDir(), "uv")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "uv 0.0.0-fake"; exit 0; fi
shots=""; prev=""
for a in "$@"; do [ "$prev" = "--shots" ] && shots="$a"; prev="$a"; done
echo "Installed 12 packages" >&2
echo "{\"device\": \"braket_sv (local)\", \"shots\": $shots, \"measured_qubits\": [0, 1], \"counts\": {\"11\": 48, \"00\": 52}, \"simulate_seconds\": 0.007, \"software\": {\"amazon-braket-default-simulator\": \"1.41.1\", \"python\": \"3.14.8\"}}"
`
	if err := os.WriteFile(uv, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := localUV
	localUV = uv
	t.Cleanup(func() { localUV = orig })
}

func TestRunLocal(t *testing.T) {
	fakeLocalUV(t)
	out, errs, code := runPuff(t, "run", bellQASM, "--shots", "100")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"100 shots on the local simulator", "no AWS call, not billable", "00       52     0.520", "11       48     0.480"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(errs, "Installed") {
		t.Error("uv's messages shown without --verbose")
	}
	_, errs, _ = runPuff(t, "run", bellQASM, "--shots", "100", "--verbose")
	if !strings.Contains(errs, "Installed 12 packages") {
		t.Errorf("--verbose did not pass uv's messages on: %q", errs)
	}
}

func TestRunLocalJSON(t *testing.T) {
	fakeLocalUV(t)
	out, errs, code := runPuff(t, "run", bellQASM, "--shots", "100", "--offline", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var v runJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Rung != "local" || v.Billable || v.Result.Counts["00"] != 52 || v.Simulator != "amazon-braket-default-simulator==1.41.1" ||
		!filepath.IsAbs(v.Program) || len(v.ProgramSHA256) != 64 || v.PuffVersion == "" {
		t.Errorf("JSON %+v, %v", v, err)
	}
}

func TestRunUsageErrors(t *testing.T) {
	fakeLocalUV(t)
	for name, args := range map[string][]string{
		"no program":   {"run"},
		"not qasm":     {"run", "prog.json"},
		"zero shots":   {"run", bellQASM, "--shots", "0"},
		"two programs": {"run", bellQASM, bellQASM},
	} {
		if _, errs, code := runPuff(t, args...); code != exitUsage {
			t.Errorf("%s: exit %d (%s)", name, code, errs)
		}
	}
	if _, errs, code := runPuff(t, "run", "missing.qasm"); code != exitError || !strings.Contains(errs, "missing.qasm") {
		t.Errorf("missing file: exit %d %s", code, errs)
	}
}

// TestRunLocalNoAWS is A-offline's command half (#9): the local rung runs with
// AWS configuration that can't resolve and the AWS factory set to fail (see
// TestMain), and still returns counts.
func TestRunLocalNoAWS(t *testing.T) {
	fakeLocalUV(t)
	t.Setenv("AWS_PROFILE", "does-not-exist")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	before := awscfg.Loads()
	out, errs, code := runPuff(t, "run", bellQASM, "--shots", "100")
	if code != exitOK || !strings.Contains(out, "52") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if n := awscfg.Loads() - before; n != 0 {
		t.Errorf("puff run loaded AWS configuration %d times; the local rung must not (A-offline, #9)", n)
	}
}

// TestRunRealLocalSimulator runs the real binary path end to end when uv is here.
func TestRunRealLocalSimulator(t *testing.T) {
	if testing.Short() {
		t.Skip("runs uv and the simulator")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not installed")
	}
	out, errs, code := runPuff(t, "run", bellQASM, "--shots", "200", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var v runJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Result.Counts["00"]+v.Result.Counts["11"] != 200 {
		t.Errorf("Bell: %+v %v", v.Result, err)
	}
}
