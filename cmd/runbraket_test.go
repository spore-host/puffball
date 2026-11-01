package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/pkg/awscfg"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/results"
	"github.com/spore-host/puffball/pkg/testutil"
)

const fakeAccount = "123456789012"

type fakeResults struct {
	exists  bool
	created []string
}

func (f *fakeResults) Account(context.Context) (string, error) { return fakeAccount, nil }
func (f *fakeResults) Exists(context.Context, string, string, string) (bool, error) {
	return f.exists, nil
}
func (f *fakeResults) Create(_ context.Context, region, bucket string) error {
	f.created = append(f.created, region+" "+bucket)
	return nil
}

// braketHarness wires run --device to the G0 fixtures, a recording creator,
// in-memory tokens shared across runs, and a fake results store.
func braketHarness(t *testing.T, bucketExists bool) (*testutil.RecordingCreator, *fakeResults) {
	t.Helper()
	g0Factory(t, nil)
	rc := &testutil.RecordingCreator{}
	tokens := &gate.MemTokens{}
	origGate, origRes := openGate, newResults
	t.Cleanup(func() { openGate, newResults = origGate, origRes })
	openGate = func(c *cobra.Command, f gateFlags) (*gate.Gate, error) {
		o := gateOptions(c, f)
		o.Creators, o.Tokens = rc.Factory(), tokens
		return gate.New(o)
	}
	fr := &fakeResults{exists: bucketExists}
	newResults = func() results.Store { return fr }
	return rc, fr
}

func noAccount(t *testing.T, s ...string) {
	t.Helper()
	for _, x := range s {
		if strings.Contains(x, fakeAccount) {
			t.Errorf("output carries the account ID (law 7):\n%s", x)
		}
	}
}

func TestRunBraketWithoutRealSubmitsNothing(t *testing.T) {
	rc, fr := braketHarness(t, false)
	out, errs, code := runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "100")
	if code != exitOK || len(rc.Calls) != 0 || len(fr.created) != 0 {
		t.Fatalf("exit %d, %d creates, buckets %v: %s", code, len(rc.Calls), fr.created, errs)
	}
	for _, want := range []string{"estimate (prices as of 2026-10): cepheus: $0.30 + 100 x $0.000425 = $0.3425", "Not submitted. Pass --real", "Cepheus-1-108Q in us-west-1"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errs)
		}
	}
	if out != "" {
		t.Errorf("stdout should be empty without --json: %q", out)
	}
	out, _, _ = runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "100", "--json")
	var v braketRun
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Submitted || v.TaskArn != "" || v.Estimate.TotalUSD != "$0.3425" {
		t.Errorf("--json without --real: %+v %v", v, err)
	}
}

func TestRunBraketReal(t *testing.T) {
	rc, fr := braketHarness(t, false)
	out, errs, code := runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "100", "--real",
		"--project", "fmo", "--tag", "owner=me", "--run-id", "run-test1", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(rc.Calls) != 1 {
		t.Fatalf("%d creates, want 1", len(rc.Calls))
	}
	in := rc.Calls[0]
	if aws.ToString(in.DeviceArn) != "arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q" ||
		aws.ToString(in.OutputS3Bucket) != "amazon-braket-us-west-1-"+fakeAccount ||
		aws.ToString(in.OutputS3KeyPrefix) != "puffball/run-test1" || aws.ToInt64(in.Shots) != 100 ||
		!strings.Contains(aws.ToString(in.Action), `"braket.ir.openqasm.program"`) {
		t.Errorf("CreateQuantumTask input: %+v", in)
	}
	want := map[string]string{"puffball:project": "fmo", "puffball:rung": "qpu", "puffball:run-id": "run-test1", "puffball:est-usd": "0.3425", "puffball:prices-as-of": "2026-10", "owner": "me"}
	for k, v := range want {
		if in.Tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, in.Tags[k], v)
		}
	}
	if len(fr.created) != 1 || !strings.Contains(errs, "created the results bucket amazon-braket-us-west-1-<account>") {
		t.Errorf("missing bucket not created and reported: %v %s", fr.created, errs)
	}
	var v braketRun
	if err := json.Unmarshal([]byte(out), &v); err != nil || !v.Submitted || v.TaskID != "fake-1" || v.TaskArn != "arn:aws:braket:us-west-1:<account>:quantum-task/fake-1" || !v.BucketCreated ||
		v.ResultsPrefix != "s3://amazon-braket-us-west-1-<account>/puffball/run-test1/" || len(v.ProgramSHA256) != 64 {
		t.Errorf("JSON %+v %v", v, err)
	}
	noAccount(t, errs, out)

	// The same run id again: the recorded task is returned, nothing created.
	out, errs, code = runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "100", "--real", "--project", "fmo", "--tag", "owner=me", "--run-id", "run-test1")
	if code != exitOK || len(rc.Calls) != 1 || !strings.Contains(out, "already submitted") {
		t.Errorf("retry: exit %d, %d creates: %s %s", code, len(rc.Calls), out, errs)
	}
	noAccount(t, out, errs)
}

func TestRunBraketHumanOutput(t *testing.T) {
	_, _ = braketHarness(t, true)
	out, errs, code := runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "100", "--real", "--run-id", "run-h")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"submitted: task fake-1 on cepheus in us-west-1", "run id:  run-h", "results: s3://amazon-braket-us-west-1-<account>/puffball/run-h/", "puffball:rung=qpu"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(errs, "created the results bucket") {
		t.Error("an existing bucket was reported as created")
	}
	noAccount(t, out, errs)
}

func TestRunBraketSimulatorRegion(t *testing.T) {
	rc, _ := braketHarness(t, true)
	// Only --region decides; clear anything the environment would add.
	for _, k := range []string{"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "SPORE_PROFILE", "SPORE_REGION"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { awscfg.SetFlags("", "") })
	if _, errs, code := runPuff(t, "run", bellQASM, "--device", "sv1", "--shots", "100", "--real", "--region", "us-east-1"); code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if b := aws.ToString(rc.Calls[0].OutputS3Bucket); !strings.HasPrefix(b, "amazon-braket-us-east-1-") {
		t.Errorf("SV1 submitted with bucket %s; want the configured region us-east-1", b)
	}
	if rc.Calls[0].Tags["puffball:rung"] != "sv1" {
		t.Errorf("rung tag %q", rc.Calls[0].Tags["puffball:rung"])
	}
	if _, errs, code := runPuff(t, "run", bellQASM, "--device", "sv1", "--shots", "100", "--real", "--device-region", "eu-west-2"); code != exitOK {
		t.Fatalf("--device-region: exit %d: %s", code, errs)
	}
	if b := aws.ToString(rc.Calls[1].OutputS3Bucket); !strings.HasPrefix(b, "amazon-braket-eu-west-2-") {
		t.Errorf("--device-region eu-west-2 used bucket %s", b)
	}
	// No SV1 in eu-north-1: the first ONLINE region, in order.
	if _, errs, code := runPuff(t, "run", bellQASM, "--device", "sv1", "--shots", "100", "--real", "--region", "eu-north-1"); code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if b := aws.ToString(rc.Calls[2].OutputS3Bucket); !strings.HasPrefix(b, "amazon-braket-eu-west-2-") {
		t.Errorf("fallback region bucket %s; want the first ONLINE region, eu-west-2", b)
	}
}

func TestRunBraketRefusals(t *testing.T) {
	rc, _ := braketHarness(t, true)
	usage := map[string][]string{
		"unknown device":          {"run", bellQASM, "--device", "nope", "--shots", "100"},
		"retired device":          {"run", bellQASM, "--device", "TN1", "--shots", "100"},
		"qasm on analog":          {"run", bellQASM, "--device", "aquila", "--shots", "100"},
		"ahs on gate":             {"run", "../testdata/golden/devices.json", "--device", "cepheus", "--shots", "100"},
		"unsupported file":        {"run", "../CHANGELOG.md", "--device", "cepheus", "--shots", "100"},
		"shots below range":       {"run", bellQASM, "--device", "cepheus", "--shots", "5"},
		"puffball: tag":           {"run", bellQASM, "--device", "cepheus", "--shots", "100", "--tag", "puffball:rung=x"},
		"tag without =":           {"run", bellQASM, "--device", "cepheus", "--shots", "100", "--tag", "owner"},
		"bad device region":       {"run", bellQASM, "--device", "sv1", "--shots", "100", "--device-region", "eu-north-1"},
		"sim budget, no duration": {"run", bellQASM, "--device", "sv1", "--shots", "100", "--budget", "1"},
		"--real without --device": {"run", bellQASM, "--real"},
		"--tag without --device":  {"run", bellQASM, "--tag", "a=b"},
	}
	for name, args := range usage {
		if _, errs, code := runPuff(t, args...); code != exitUsage {
			t.Errorf("%s: exit %d, want 2 (%s)", name, code, errs)
		}
	}
	if _, errs, code := runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "1000", "--real", "--budget", "0.5"); code != exitError || !strings.Contains(errs, "exceeds --budget") {
		t.Errorf("over budget: exit %d %s", code, errs)
	}
	if len(rc.Calls) != 0 {
		t.Errorf("%d tasks created by refused runs", len(rc.Calls))
	}
}

func TestRunBraketDiscoveryFailure(t *testing.T) {
	_, _ = braketHarness(t, true)
	g0Factory(t, func(f *testutil.FakeFactory) { f.Regions["us-west-1"].Err = errors.New("throttled") })
	if _, errs, code := runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "100"); code != exitError || !strings.Contains(errs, "us-west-1") {
		t.Errorf("Cepheus's region down: exit %d %s", code, errs)
	}
}

func TestParseTagsAndTaskID(t *testing.T) {
	got, err := parseTags([]string{"a=1", " b = 2 ", "c="})
	if err != nil || got["a"] != "1" || got["b"] != " 2 " || got["c"] != "" {
		t.Errorf("parseTags = %v %v", got, err)
	}
	if taskID("arn:aws:braket:us-west-1:000000000000:quantum-task/abc-123") != "abc-123" {
		t.Error("taskID")
	}
	if !strings.HasPrefix(newRunID(), "run-") || len(newRunID()) != 12 {
		t.Error("newRunID")
	}
}

// TestRunBraketTokenDirUnwritable: an unwritable token dir stops a --real
// run before the results bucket is created or anything is submitted.
func TestRunBraketTokenDirUnwritable(t *testing.T) {
	g0Factory(t, nil)
	rc := &testutil.RecordingCreator{}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	origGate, origRes := openGate, newResults
	t.Cleanup(func() { openGate, newResults = origGate, origRes })
	openGate = func(c *cobra.Command, f gateFlags) (*gate.Gate, error) {
		o := gateOptions(c, f)
		o.Creators, o.Tokens = rc.Factory(), gate.FileTokens{Dir: filepath.Join(ro, "tokens")}
		return gate.New(o)
	}
	fr := &fakeResults{}
	newResults = func() results.Store { return fr }
	_, errs, code := runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "100", "--real")
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	if code != exitError || len(fr.created) != 0 || len(rc.Calls) != 0 || !strings.Contains(errs, "XDG_STATE_HOME") {
		t.Errorf("exit %d, buckets %v, creates %d: %s", code, fr.created, len(rc.Calls), errs)
	}
}

// TestRunBraketRetryPinsDerivedTags: a retry after a lost response, with a
// different --duration (so a different estimate), resends the first
// attempt's estimate tags and is accepted; changing what the user chose
// (shots) is refused, naming the field.
func TestRunBraketRetryPinsDerivedTags(t *testing.T) {
	rc, _ := braketHarness(t, true)
	rc.LoseResponse = 1
	args := []string{"run", bellQASM, "--device", "sv1", "--shots", "100", "--real", "--run-id", "run-pin", "--device-region", "us-west-1"}
	if _, errs, code := runPuff(t, append(args, "--duration", "10s")...); code != exitError {
		t.Fatalf("first attempt (lost response): exit %d %s", code, errs)
	}
	out, errs, code := runPuff(t, append(args, "--duration", "20s")...)
	if code != exitOK || !strings.Contains(out, "a retry of an earlier attempt") {
		t.Fatalf("retry: exit %d\n%s\n%s", code, out, errs)
	}
	if rc.Created() != 1 || len(rc.Calls) != 2 {
		t.Fatalf("created %d over %d calls", rc.Created(), len(rc.Calls))
	}
	if a, b := rc.Calls[0].Tags["puffball:est-usd"], rc.Calls[1].Tags["puffball:est-usd"]; a != b || a != "0.0125" {
		t.Errorf("est-usd sent as %q then %q; the retry must resend the first", a, b)
	}
	// Once recorded, a run with yet another --duration reports the task's
	// tags as created, not the new estimate.
	out, _, _ = runPuff(t, append(args, "--duration", "40s", "--json")...)
	var v braketRun
	if err := json.Unmarshal([]byte(out), &v); err != nil || !v.AlreadyRan || v.Tags["puffball:est-usd"] != "0.0125" {
		t.Errorf("already submitted: tags %v (want the task's 0.0125) %v", v.Tags, err)
	}
	out, _, _ = runPuff(t, append(args, "--duration", "40s")...)
	if !strings.Contains(out, "the task was created with an estimate of $0.0125") {
		t.Errorf("no note that the shown estimate isn't the task's:\n%s", out)
	}
	// A new key (fresh run) with different shots is fine; the same key isn't.
	rc.LoseResponse = 1
	args2 := []string{"run", bellQASM, "--device", "cepheus", "--shots", "100", "--real", "--run-id", "run-mismatch"}
	_, _, _ = runPuff(t, args2...)
	_, errs, code = runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "200", "--real", "--run-id", "run-mismatch")
	if code != exitError || !strings.Contains(errs, "differs in shots") {
		t.Errorf("same run id, different shots: exit %d %s", code, errs)
	}
}

func TestRunBraketDiscoveryPartial(t *testing.T) {
	_, _ = braketHarness(t, true)
	g0Factory(t, func(f *testutil.FakeFactory) { f.Regions["us-east-1"].Err = errors.New("throttled") })
	// Cepheus is in us-west-1, which answered: proceeds, naming the failure.
	_, errs, code := runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "100")
	if code != exitOK || !strings.Contains(errs, "warning: incomplete") || !strings.Contains(errs, "us-east-1") {
		t.Errorf("unrelated region down: exit %d %s", code, errs)
	}
	// Cepheus still runs when the *configured* region is the one that failed.
	if _, errs, code := runPuff(t, "run", bellQASM, "--device", "cepheus", "--shots", "100", "--region", "us-east-1"); code != exitOK {
		t.Errorf("Cepheus with the configured region down: exit %d %s", code, errs)
	}
	// SV1 with an explicit, healthy --device-region runs despite the failure.
	if _, errs, code := runPuff(t, "run", bellQASM, "--device", "sv1", "--shots", "100", "--device-region", "us-west-1", "--region", "us-east-1"); code != exitOK {
		t.Errorf("SV1 with --device-region us-west-1: exit %d %s", code, errs)
	}
	// SV1 with no --device-region and the configured region down: refused, not
	// silently moved.
	if _, errs, code := runPuff(t, "run", bellQASM, "--device", "sv1", "--shots", "100", "--region", "us-east-1"); code != exitUsage || !strings.Contains(errs, "--device-region") {
		t.Errorf("SV1, configured region down: exit %d %s", code, errs)
	}
	// Asking for SV1 in the region that failed is refused.
	_, errs, code = runPuff(t, "run", bellQASM, "--device", "sv1", "--shots", "100", "--device-region", "us-east-1")
	if code != exitUsage || !strings.Contains(errs, "couldn't be searched") {
		t.Errorf("SV1 in a failed region: exit %d %s", code, errs)
	}
}

func TestCheckTag(t *testing.T) {
	ok := [][2]string{{"owner", "me"}, {"team name", "Q&A"[:1]}, {"path", "a/b:c=d+e-f@g_h.i"}, {"ünïcode", "日本"}, {"k", ""}}
	for _, kv := range ok {
		if err := checkTag(kv[0], kv[1]); err != nil {
			t.Errorf("checkTag(%q, %q) = %v", kv[0], kv[1], err)
		}
	}
	bad := [][2]string{{"", "v"}, {strings.Repeat("k", 129), "v"}, {"k", strings.Repeat("v", 257)}, {"AWS:x", "v"}, {"k", "a;b"}, {"k$", "v"}}
	for _, kv := range bad {
		if err := checkTag(kv[0], kv[1]); err == nil {
			t.Errorf("checkTag(%q, %q) accepted", kv[0], kv[1])
		}
	}
}

// TestRunBraketAHS: an AHS program on the analog device (estimate only; no
// --real, so nothing is spent), and the paradigm checks both ways.
func TestRunBraketAHS(t *testing.T) {
	rc, _ := braketHarness(t, true)
	ahs := "../testdata/programs/ahs_two_atoms.json"
	_, errs, code := runPuff(t, "run", ahs, "--device", "aquila", "--shots", "100")
	if code != exitOK || !strings.Contains(errs, "aquila: $0.30 + 100 x $0.01 = $1.30") {
		t.Errorf("AHS on Aquila: exit %d %s", code, errs)
	}
	if _, errs, code := runPuff(t, "run", ahs, "--device", "cepheus", "--shots", "100"); code != exitUsage || !strings.Contains(errs, "not AHS") {
		t.Errorf("AHS on a gate device: exit %d %s", code, errs)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"setup": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := runPuff(t, "run", bad, "--device", "aquila", "--shots", "100"); code != exitUsage || !strings.Contains(errs, "ahs_register") {
		t.Errorf("malformed AHS: exit %d %s", code, errs)
	}
	if len(rc.Calls) != 0 {
		t.Error("a task was created without --real")
	}
}
