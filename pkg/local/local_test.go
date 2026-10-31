package local

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const bell = "../../testdata/programs/bell.qasm"

// fakeUV writes a shell script standing in for uv. It records its arguments
// and environment, then prints body (with SHOTS replaced by the --shots value).
func fakeUV(t *testing.T, body string, exit int) (uv, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	uv = filepath.Join(dir, "uv")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "uv 0.0.0-fake"; exit 0; fi
printf '%s\n' "$@" > "` + record + `.args"
env > "` + record + `.env"
shots=""; prev=""
for a in "$@"; do [ "$prev" = "--shots" ] && shots="$a"; prev="$a"; done
echo "uv says hello" >&2
cat <<JSON | sed "s/SHOTS/$shots/"
` + body + `
JSON
exit ` + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(uv, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return uv, record
}

const goodBody = `{"device": "braket_sv (local)", "shots": SHOTS, "measured_qubits": [0, 1], "counts": {"00": 52, "11": 48}, "simulate_seconds": 0.007, "software": {"amazon-braket-default-simulator": "1.41.1", "python": "3.14.8"}, "missing": ["rng_seed"]}`

func TestRunWithFakeUV(t *testing.T) {
	uv, record := fakeUV(t, goodBody, 0)
	t.Setenv("AWS_PROFILE", "aws")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("PYTHONPATH", "/tmp/shadow")
	var stderr bytes.Buffer
	r, err := Run(context.Background(), bell, 100, Options{UV: uv, Offline: true, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if r.Shots != 100 || r.Counts["00"] != 52 || r.Software["amazon-braket-default-simulator"] != "1.41.1" {
		t.Errorf("result %+v", r)
	}
	if got := r.Outcomes(); !reflect.DeepEqual(got, []string{"00", "11"}) {
		t.Errorf("Outcomes() = %v", got)
	}
	if !strings.Contains(stderr.String(), "uv says hello") {
		t.Error("uv's stderr was not passed on")
	}
	args, _ := os.ReadFile(record + ".args")
	if r.Software["uv"] != "uv 0.0.0-fake" || len(r.Missing) != 1 {
		t.Errorf("software %v missing %v", r.Software, r.Missing)
	}
	for _, want := range []string{"run", "--isolated", "--no-config", "--no-project", "--python", "3.14", "--offline", "--with", "amazon-braket-default-simulator==1.41.1", "python", "-I", "--shots", "100"} {
		if !strings.Contains(string(args), want+"\n") {
			t.Errorf("uv args lack %q:\n%s", want, args)
		}
	}
	env, _ := os.ReadFile(record + ".env")
	for _, bad := range []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "PYTHONPATH"} {
		if strings.Contains(string(env), bad+"=") {
			t.Errorf("the simulator saw %s (A-offline, #9)", bad)
		}
	}
	for _, want := range []string{"AWS_CONFIG_FILE=/dev/null", "AWS_SHARED_CREDENTIALS_FILE=/dev/null", "AWS_EC2_METADATA_DISABLED=true"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("the simulator's environment lacks %s", want)
		}
	}
}

func TestRunFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := Run(ctx, bell, 0, Options{UV: "/bin/true"}); err == nil {
		t.Error("0 shots accepted")
	}
	if _, err := Run(ctx, "nope.qasm", 10, Options{UV: "/bin/true"}); err == nil {
		t.Error("missing program accepted")
	}
	uv, _ := fakeUV(t, "Failed to fetch: network disabled in offline mode", 2)
	_, err := Run(ctx, bell, 10, Options{UV: uv, Offline: true})
	if err == nil || !strings.Contains(err.Error(), "uv says hello") {
		t.Errorf("uv failure lost its stderr: %v", err)
	}
	uv, _ = fakeUV(t, "not json", 0)
	if _, err := Run(ctx, bell, 10, Options{UV: uv}); err == nil || !strings.Contains(err.Error(), "other than its JSON") {
		t.Errorf("non-JSON output: %v", err)
	}
	for name, body := range map[string]string{
		"measured shots differ": `{"shots": 7, "measured_qubits": [0], "counts": {"0": 7}}`,
		"empty counts":          `{"shots": SHOTS, "measured_qubits": [0], "counts": {}}`,
		"counts don't add up":   `{"shots": SHOTS, "measured_qubits": [0], "counts": {"0": 3}}`,
		"not a bit string":      `{"shots": SHOTS, "measured_qubits": [0], "counts": {"x": 10}}`,
		"wrong width":           `{"shots": SHOTS, "measured_qubits": [0, 1], "counts": {"0": 10}}`,
		"negative count":        `{"shots": SHOTS, "measured_qubits": [0], "counts": {"0": 20, "1": -10}}`,
	} {
		uv, _ := fakeUV(t, body, 0)
		if _, err := Run(ctx, bell, 10, Options{UV: uv}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOfflineHint(t *testing.T) {
	dir := t.TempDir()
	uv := filepath.Join(dir, "uv")
	// uv 0.13.6's real message for an --offline cache miss (measured).
	msg := "error: No solution found when resolving `--with` dependencies\n" +
		"  cause: Because amazon-braket-default-simulator was not found in the cache and you require amazon-braket-default-simulator==1.41.1, we can conclude that your requirements are unsatisfiable.\n\n" +
		"hint: Packages were unavailable because the network was disabled. When the network is disabled, registry packages may only be read from the cache."
	if err := os.WriteFile(uv, []byte("#!/bin/sh\ncat >&2 <<'EOF'\n"+msg+"\nEOF\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), bell, 10, Options{UV: uv, Offline: true})
	if err == nil || !strings.Contains(err.Error(), "run once without --offline") {
		t.Errorf("offline cache miss: %v", err)
	}
	// uv 0.13.6's message when Python 3.14 isn't installed (measured).
	noPython := filepath.Join(t.TempDir(), "uv")
	if err := os.WriteFile(noPython, []byte("#!/bin/sh\necho 'error: No interpreter found for Python 3.14 in virtual environments or managed installations' >&2\necho 'hint: A managed Python download is available for Python 3.14, but uv is set to offline mode' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), bell, 10, Options{UV: noPython, Offline: true}); err == nil || !strings.Contains(err.Error(), "run once without --offline") {
		t.Errorf("offline, no Python 3.14: %v", err)
	}
	// Online, the same failure gets no --offline hint.
	if _, err := Run(context.Background(), bell, 10, Options{UV: uv}); err == nil || strings.Contains(err.Error(), "run once without --offline") {
		t.Errorf("online failure: %v", err)
	}
}

func TestFindUV(t *testing.T) {
	if p, err := FindUV(Options{UV: "/x/uv"}); err != nil || p != "/x/uv" {
		t.Errorf("explicit UV: %s %v", p, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := FindUV(Options{}); !errors.Is(err, ErrNoUV) {
		t.Errorf("no uv on PATH: %v", err)
	}
}

func TestEnvAndCommand(t *testing.T) {
	got := Env([]string{"HOME=/h", "AWS_PROFILE=aws", "AWS_REGION=us-west-1", "PATH=/bin", "UV_CACHE_DIR=/c", "PYTHONPATH=/x", "PYTHONHOME=/y", "VIRTUAL_ENV=/v"})
	want := []string{"HOME=/h", "PATH=/bin", "UV_CACHE_DIR=/c", "AWS_CONFIG_FILE=" + os.DevNull, "AWS_SHARED_CREDENTIALS_FILE=" + os.DevNull, "AWS_EC2_METADATA_DISABLED=true"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Env = %v", got)
	}
	if c := Command("s.py", "p.qasm", 5, false); strings.Contains(strings.Join(c, " "), "--offline") {
		t.Errorf("online command has --offline: %v", c)
	}
}

// TestNoAWSDependency is A-offline's static half (#9): the local rung's
// package graph contains nothing from the AWS SDK or puff's AWS packages.
func TestNoAWSDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v; this check must run, not pass by skipping", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "aws-sdk-go") || strings.HasSuffix(dep, "/pkg/awscfg") || strings.HasSuffix(dep, "/pkg/braket") || strings.HasSuffix(dep, "/pkg/gate") {
			t.Errorf("pkg/local depends on %s; the local rung must not reach AWS", dep)
		}
	}
}

// TestRunRealSimulator runs the real uv and simulator when they are here.
func TestRunRealSimulator(t *testing.T) {
	if testing.Short() {
		t.Skip("runs uv and the simulator")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not installed")
	}
	r, err := Run(context.Background(), bell, 200, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Counts["00"]+r.Counts["11"] != 200 || r.Software["amazon-braket-default-simulator"] != SimulatorVersion || !strings.HasPrefix(r.Software["python"], PythonVersion) || r.Software["numpy"] == "" || r.Software["uv"] == "" {
		t.Errorf("Bell on the local simulator: %+v", r)
	}
}

func TestCancelStopsTheSimulator(t *testing.T) {
	uv := filepath.Join(t.TempDir(), "uv")
	if err := os.WriteFile(uv, []byte("#!/bin/sh\n[ \"$1\" = --version ] && exit 0\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Run(ctx, bell, 10, Options{UV: uv}); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("a cancelled run: %v; want an interrupted error", err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("cancel took %v; the process group was not stopped", d)
	}
}

// TestRunSourceRunsExactlyTheBytes: the simulator reads a copy of src, not the
// caller's file, so a hash of src is a hash of what ran.
func TestRunSourceRunsExactlyTheBytes(t *testing.T) {
	uv, record := fakeUV(t, goodBody, 0)
	src := []byte("OPENQASM 3.0;\n// exact bytes\n")
	if _, err := RunSource(context.Background(), src, 100, Options{UV: uv}); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(record + ".args")
	if !strings.Contains(string(args), "program.qasm") {
		t.Errorf("the simulator was not given puff's copy: %s", args)
	}
}

// TestUVVersionMissing: if uv --version fails, the result says so in
// missing[] rather than dropping it (law 6).
func TestUVVersionMissing(t *testing.T) {
	uv := filepath.Join(t.TempDir(), "uv")
	body := strings.ReplaceAll(goodBody, "SHOTS", "100")
	script := "#!/bin/sh\n[ \"$1\" = --version ] && exit 1\ncat <<'JSON'\n" + body + "\nJSON\n"
	if err := os.WriteFile(uv, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), bell, 100, Options{UV: uv})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Software["uv"]; ok || !strings.Contains(strings.Join(r.Missing, ","), "software.uv") {
		t.Errorf("software %v missing %v; want software.uv named as missing", r.Software, r.Missing)
	}
}

func TestOutcomesOrder(t *testing.T) {
	r := Result{Counts: map[string]int64{"10": 5, "01": 5, "11": 9}}
	if got := r.Outcomes(); !reflect.DeepEqual(got, []string{"11", "01", "10"}) {
		t.Errorf("Outcomes() = %v; want most frequent first, ties by bit string", got)
	}
}
