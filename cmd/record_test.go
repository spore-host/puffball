package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/puffball/pkg/record"
	"github.com/spore-host/puffball/pkg/results"
)

// g0Objects serves the G0 tasks' S3 objects (testdata/tasks/<name>/s3) by
// key, from the bucket of the account tasksHarness gives them.
type g0Objects struct{ reads []string }

var g0Dirs = map[string]string{
	"617b7e2b-f77f-433b-8226-6cc55eb8d077": "cepheus-bell-100",
	"a543ab67-6545-4cf0-8bbe-55f55bef1aa6": "cepheus-progset-2x100",
	"d1b61d7d-936d-4574-af57-f962beb72375": "sv1-bell-100",
}

func (o *g0Objects) Read(_ context.Context, _, bucket, key, account string, _ int64) (results.Object, error) {
	o.reads = append(o.reads, key)
	if !strings.HasSuffix(bucket, account) {
		return results.Object{}, errors.New("AccessDenied")
	}
	parts := strings.SplitN(key, "/", 4) // puffball/<run-id>/<task-id>/<rest>
	if len(parts) < 4 || g0Dirs[parts[2]] == "" {
		return results.Object{}, results.ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join("../testdata/tasks", g0Dirs[parts[2]], "s3", parts[3]))
	if err != nil {
		return results.Object{}, results.ErrNotFound
	}
	return results.Object{Body: b, ETag: `"e"`}, nil
}

func recordHarness(t *testing.T) *g0Objects {
	t.Helper()
	tasksHarness(t, nil)
	o := &g0Objects{}
	orig := newReader
	t.Cleanup(func() { newReader = orig })
	newReader = func() results.Reader { return o }
	return o
}

func TestRecordCommand(t *testing.T) {
	recordHarness(t)
	out, errs, code := runPuff(t, "record", g0Bell)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if p := record.Verify([]byte(out)); len(p) > 0 {
		t.Errorf("printed record doesn't verify: %v", p)
	}
	var r record.Record
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.Task.ID != g0Bell || len(r.PhysicalQubits) != 2 || r.Calibration.AtCompletion == nil || r.Cost == nil || r.Cost.USD != "$0.3425" {
		t.Errorf("record %+v", r.Task)
	}
	for _, want := range []string{"record for task " + g0Bell + " on Cepheus-1-108Q", "calibration.at_submission:", "program.submitted_action_sha256:"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errs)
		}
	}
	noAccount(t, out, errs)
}

func TestRecordOutAndVerify(t *testing.T) {
	recordHarness(t)
	path := filepath.Join(t.TempDir(), "bell.record.json")
	if _, errs, code := runPuff(t, "record", g0ProgSet, "--out", path); code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	recordHarness(t)
	if _, errs, code := runPuff(t, "record", g0ProgSet, "--out", path); code != exitUsage || !strings.Contains(errs, "--force") {
		t.Errorf("overwrite without --force: exit %d: %s", code, errs)
	}
	recordHarness(t)
	if _, errs, code := runPuff(t, "record", g0ProgSet, "--out", path, "--force"); code != exitOK {
		t.Errorf("--force: exit %d: %s", code, errs)
	}
	out, errs, code := runPuff(t, "record", "verify", path)
	if code != exitOK || !strings.Contains(out, "verified") {
		t.Errorf("verify: exit %d: %s %s", code, out, errs)
	}
	b, _ := os.ReadFile(path)
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(strings.Replace(string(b), "OPENQASM 3.0;", "OPENQASM 2.0;", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs, code = runPuff(t, "record", "verify", bad, "--json")
	if code != exitError || !strings.Contains(out, `"ok": false`) || !strings.Contains(errs, "sha256 doesn't match") {
		t.Errorf("tampered: exit %d: %s %s", code, out, errs)
	}
	if _, _, code := runPuff(t, "record", "verify", filepath.Join(t.TempDir(), "none.json")); code != exitError {
		t.Errorf("missing file: exit %d", code)
	}
}

func TestRecordUsage(t *testing.T) {
	recordHarness(t)
	for _, args := range [][]string{{"record"}, {"record", "not-a-task"}, {"record", "verify"}} {
		if _, errs, code := runPuff(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d: %s", args, code, errs)
		}
	}
}
