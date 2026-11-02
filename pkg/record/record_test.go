package record_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/record"
	"github.com/spore-host/puffball/pkg/results"
	"github.com/spore-host/puffball/pkg/testutil"
)

var update = flag.Bool("update", false, "rewrite the golden records")

const root = "../../testdata"

var recordedAt = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func cepheusCaps(t *testing.T) string {
	t.Helper()
	var g braketsdk.GetDeviceOutput
	testutil.LoadJSON(t, filepath.Join(root, "devices/us-west-1/rigetti_cepheus-1-108q.json"), &g)
	return *g.DeviceCapabilities
}

// fixture is a G0 task: its GetQuantumTask response and its S3 objects,
// plus any objects a test adds (the submission files).
type fixture struct {
	task  *braketsdk.GetQuantumTaskOutput
	dir   string
	extra map[string][]byte
	reads []string
}

func load(t *testing.T, name string) *fixture {
	t.Helper()
	var g braketsdk.GetQuantumTaskOutput
	testutil.LoadJSON(t, filepath.Join(root, "tasks", name, "get.final.json"), &g)
	return &fixture{task: &g, dir: filepath.Join(root, "tasks", name, "s3"), extra: map[string][]byte{}}
}

func (f *fixture) get(_ context.Context, key string) (results.Object, error) {
	f.reads = append(f.reads, key)
	if b, ok := f.extra[key]; ok {
		return results.Object{Body: b, ETag: `"extra"`}, nil
	}
	rel, ok := strings.CutPrefix(key, *f.task.OutputS3Directory+"/")
	if !ok {
		return results.Object{}, results.ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(f.dir, rel))
	if err != nil {
		return results.Object{}, results.ErrNotFound
	}
	return results.Object{Body: b, ETag: `"` + record.Sum(b)[:32] + `"`}, nil
}

// submitted adds what puff run stores before submitting: the action (taken
// from the results' own copy) and the device snapshot.
func (f *fixture) submitted(t *testing.T, caps string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		AdditionalMetadata struct {
			Action json.RawMessage `json:"action"`
		} `json:"additionalMetadata"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	dir := record.SubmissionDir(results.Prefix+f.task.Tags["puffball:run-id"], "cepheus")
	f.extra[dir+record.ActionObject] = r.AdditionalMetadata.Action
	snap, err := record.NewSnapshot(*f.task.DeviceArn, "us-west-1", caps, f.task.CreatedAt.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	f.extra[dir+record.DeviceObject] = snap
}

func build(t *testing.T, f *fixture, caps string) *record.Record {
	t.Helper()
	tb, err := price.Load()
	if err != nil {
		t.Fatal(err)
	}
	r, err := record.Build(context.Background(), record.Inputs{
		Task: f.task, Get: f.get, Prices: tb, PuffVersion: "test",
		Device: func(context.Context) (string, error) { return caps, nil },
		Now:    func() time.Time { return recordedAt },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Toolchain-dependent versions don't belong in a golden file.
	for k := range r.Software {
		if k == "go" || strings.HasPrefix(k, "github.com/") {
			r.Software[k] = "normalized"
		}
	}
	return r
}

func golden(t *testing.T, name string, r *record.Record) []byte {
	t.Helper()
	b, err := record.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "golden", name)
	if *update {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./pkg/record -update)", err)
	}
	if string(b) != string(want) {
		t.Errorf("%s differs from the golden file (go test ./pkg/record -update to accept):\n%s", name, b)
	}
	if p := record.Verify(b); len(p) > 0 {
		t.Errorf("%s doesn't verify: %v", name, p)
	}
	if regexp.MustCompile(`\b\d{12}\b`).Match(b) {
		t.Errorf("%s carries an account-like number", name)
	}
	return b
}

func missing(r *record.Record) map[string]string {
	m := map[string]string{}
	for _, x := range r.Missing {
		m[x.Field] = x.Reason
	}
	return m
}

// A-record on the G0 Cepheus Bell task, submitted the way puff run submits.
func TestRecordCepheusBell(t *testing.T) {
	f := load(t, "cepheus-bell-100")
	caps := cepheusCaps(t)
	f.submitted(t, caps)
	r := build(t, f, caps)
	golden(t, "record_cepheus_bell.json", r)

	if got := r.PhysicalQubits; len(got) != 2 || got[0] != 81 || got[1] != 90 {
		t.Errorf("physical qubits %v, want [81 90] (G0, #4)", got)
	}
	if len(r.CompiledPrograms) != 1 || r.CompiledPrograms[0].Format != "quil" {
		t.Errorf("compiled %+v", r.CompiledPrograms)
	}
	ex := r.Results[0].Executables[0]
	var n int64
	for _, c := range ex.Counts {
		n += c
	}
	if n != 100 || r.Shots.Measured != 100 || r.Shots.Requested != 100 {
		t.Errorf("counts add to %d, measured %d", n, r.Shots.Measured)
	}
	if r.Calibration.AtSubmission == nil || r.Calibration.AtCompletion == nil || r.Calibration.AtRecord != nil {
		t.Fatalf("calibration %+v", r.Calibration)
	}
	for _, c := range []*record.Calibration{r.Calibration.AtSubmission, r.Calibration.AtCompletion} {
		if len(c.Qubits) != 2 || len(c.Pairs) != 1 || c.Pairs["81-90"] == nil || len(c.Benchmarks) == 0 || c.CalibratedTo == nil {
			t.Errorf("calibration subset %+v", c)
		}
	}
	if r.Cost == nil || r.Cost.USD != "$0.3425" || r.Cost.PricesAsOf != "2026-10" {
		t.Errorf("cost %+v", r.Cost)
	}
	if r.Program.Format != "openqasm3" || len(r.Program.Sources) != 1 || r.Program.SubmittedSHA256 == "" {
		t.Errorf("program %+v", r.Program)
	}
	m := missing(r)
	for f := range m {
		if f != "shots.successful" {
			t.Errorf("unexpected missing %s: %s", f, m[f])
		}
	}
	if r.Task.Arn != "arn:aws:braket:us-west-1:<account>:quantum-task/617b7e2b-f77f-433b-8226-6cc55eb8d077" {
		t.Errorf("arn %s", r.Task.Arn)
	}
}

// The G0 program set: nested S3 layout, OpenQASM 3 compiled programs, no
// submission files (it was submitted by the G0 probe, not puff run).
func TestRecordCepheusProgramSet(t *testing.T) {
	f := load(t, "cepheus-progset-2x100")
	r := build(t, f, cepheusCaps(t))
	golden(t, "record_cepheus_progset.json", r)

	if r.Program.Format != "openqasm3-program-set" || len(r.Program.Sources) != 2 || len(r.Results) != 2 {
		t.Errorf("program %+v, %d results", r.Program, len(r.Results))
	}
	if len(r.CompiledPrograms) != 2 || r.CompiledPrograms[0].Format != "openqasm3" {
		t.Errorf("compiled %+v", r.CompiledPrograms)
	}
	if got := r.PhysicalQubits; len(got) != 2 || got[0] != 81 || got[1] != 90 {
		t.Errorf("physical qubits %v", got)
	}
	if r.Shots.Measured != 200 || r.Shots.Successful == nil || *r.Shots.Successful != 200 {
		t.Errorf("shots %+v", r.Shots)
	}
	if r.Cost == nil || r.Cost.USD != "$0.385" {
		t.Errorf("cost %+v", r.Cost)
	}
	m := missing(r)
	for _, f := range []string{"calibration.at_submission", "program.submitted_action_sha256"} {
		if !strings.Contains(m[f], "not stored") {
			t.Errorf("missing %s = %q", f, m[f])
		}
	}
	roles := map[string]int{}
	for _, o := range r.Objects {
		roles[o.Role]++
	}
	if roles["results"] != 3 || roles["program"] != 2 || roles["executable"] != 2 || roles["metadata"] != 1 {
		t.Errorf("objects by role %v", roles)
	}
}

func TestRecordSV1(t *testing.T) {
	f := load(t, "sv1-bell-100")
	r := build(t, f, "")
	b, _ := record.Encode(r)
	if p := record.Verify(b); len(p) > 0 {
		t.Errorf("verify: %v", p)
	}
	m := missing(r)
	for _, f := range []string{"compiled_programs", "physical_qubits", "calibration.at_submission", "calibration.at_completion"} {
		if m[f] == "" {
			t.Errorf("%s not named missing: %v", f, m)
		}
	}
	if r.Cost == nil || r.Cost.USD != "$0.00375" || r.Cost.Minimum {
		t.Errorf("cost %+v", r.Cost)
	}
}

// A device recalibrated after the task ended: the read is kept as
// at_record and at_completion is named missing.
func TestRecordRecalibrated(t *testing.T) {
	f := load(t, "cepheus-bell-100")
	caps := strings.ReplaceAll(cepheusCaps(t), `"timestamp": "2026-10-24T14:2`, `"timestamp": "2026-10-29T14:2`)
	r := build(t, f, caps)
	if r.Calibration.AtCompletion != nil || r.Calibration.AtRecord == nil || !strings.Contains(missing(r)["calibration.at_completion"], "recalibrated") {
		t.Errorf("calibration %+v; missing %v", r.Calibration, missing(r))
	}
}

// Only qubit 81's readout (an instructions[] MEASURE site) recalibrated
// after the end: that alone voids at_completion.
func TestRecordReadoutRecalibrated(t *testing.T) {
	f := load(t, "cepheus-bell-100")
	f.task.EndedAt = ptr(time.Date(2026, 10, 24, 16, 0, 0, 0, time.UTC))
	f.task.CreatedAt = ptr(time.Date(2026, 10, 24, 15, 59, 0, 0, time.UTC))
	r := build(t, f, cepheusCaps(t))
	if r.Calibration.AtCompletion != nil || !strings.Contains(missing(r)["calibration.at_completion"], "recalibrated at 2026-10-24T16:41:52Z") {
		t.Errorf("calibration %+v; missing %v", r.Calibration, missing(r))
	}
	if c := r.Calibration.AtRecord; c == nil || len(c.Instructions["MEASURE"]) != 2 || len(c.Instructions["CZ"]) != 1 {
		t.Errorf("instruction sites not kept: %+v", c)
	}
}

// Two tasks under one run id (the G0 probe did this): the program set must
// not take the Bell task's submitted action as its own.
func TestRecordForeignSubmission(t *testing.T) {
	bell := load(t, "cepheus-bell-100")
	caps := cepheusCaps(t)
	bell.submitted(t, caps)
	ps := load(t, "cepheus-progset-2x100")
	ps.extra = bell.extra
	r := build(t, ps, caps)
	if r.Program.SubmittedSHA256 != "" || !strings.Contains(missing(r)["program.submitted_action_sha256"], "isn't the program the results report") {
		t.Errorf("submitted %q; missing %v", r.Program.SubmittedSHA256, missing(r))
	}
}

// The calibration changed between submission and the record, with
// timestamps still before the task's end (measured live 2026-11-02): when it
// took effect isn't known, so it is at_record, not at_completion.
func TestRecordChangedSinceSubmission(t *testing.T) {
	f := load(t, "cepheus-bell-100")
	caps := cepheusCaps(t)
	f.submitted(t, caps)
	// Qubit 81's properties change; no timestamp moves past the end.
	var doc map[string]any
	if err := json.Unmarshal([]byte(caps), &doc); err != nil {
		t.Fatal(err)
	}
	doc["standardized"].(map[string]any)["oneQubitProperties"].(map[string]any)["81"] = map[string]any{"T1": map[string]any{"value": 0.00003, "unit": "S"}}
	later, _ := json.Marshal(doc)
	r := build(t, f, string(later))
	if r.Calibration.AtSubmission == nil || r.Calibration.AtCompletion != nil || r.Calibration.AtRecord == nil ||
		!strings.Contains(missing(r)["calibration.at_completion"], "changed between submission") {
		t.Errorf("calibration %+v; missing %v", r.Calibration, missing(r))
	}
}

// A snapshot left by an earlier, failed attempt isn't this submission's.
func TestRecordStaleSnapshot(t *testing.T) {
	f := load(t, "cepheus-bell-100")
	caps := cepheusCaps(t)
	f.submitted(t, caps)
	dir := record.SubmissionDir(results.Prefix+f.task.Tags["puffball:run-id"], "cepheus")
	old, _ := record.NewSnapshot(*f.task.DeviceArn, "us-west-1", caps, f.task.CreatedAt.Add(-48*time.Hour))
	f.extra[dir+record.DeviceObject] = old
	r := build(t, f, caps)
	if r.Calibration.AtSubmission != nil || !strings.Contains(missing(r)["calibration.at_submission"], "earlier attempt") {
		t.Errorf("at_submission %+v; missing %v", r.Calibration.AtSubmission, missing(r))
	}
}

// AHS results aren't counted yet: named, not a failed parse. The layout
// (measurements as objects) is amazon-braket-schemas-python's
// AnalogHamiltonianSimulationTaskResult; puff hasn't run AHS live.
func TestRecordAHS(t *testing.T) {
	tb, _ := price.Load()
	created, ended := recordedAt.Add(-time.Hour), recordedAt.Add(-50*time.Minute)
	g := &braketsdk.GetQuantumTaskOutput{
		QuantumTaskArn: ptr("arn:aws:braket:us-east-1:000000000000:quantum-task/11111111-1111-4111-8111-111111111111"),
		DeviceArn:      ptr("arn:aws:braket:us-east-1::device/qpu/quera/Aquila"), Status: types.QuantumTaskStatusCompleted,
		Shots: ptr(int64(10)), CreatedAt: &created, EndedAt: &ended,
		OutputS3Bucket: ptr("amazon-braket-us-east-1-000000000000"), OutputS3Directory: ptr("x/y"),
	}
	res := `{"braketSchemaHeader":{"name":"braket.task_result.analog_hamiltonian_simulation_task_result","version":"1"},` +
		`"measurements":[{"shotMetadata":{"shotStatus":"Success"},"shotResult":{"preSequence":[1],"postSequence":[0]}}],` +
		`"additionalMetadata":{"action":{"braketSchemaHeader":{"name":"braket.ir.ahs.program","version":"1"}}}}`
	r, err := record.Build(context.Background(), record.Inputs{Task: g, Prices: tb, PuffVersion: "test", Now: func() time.Time { return recordedAt },
		Get: func(_ context.Context, key string) (results.Object, error) {
			if key == "x/y/results.json" {
				return results.Object{Body: []byte(res)}, nil
			}
			return results.Object{}, results.ErrNotFound
		},
		Device: func(context.Context) (string, error) { return "{}", nil }})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := record.Encode(r)
	if p := record.Verify(b); len(p) > 0 {
		t.Errorf("verify: %v", p)
	}
	if r.Program.Format != "ahs" || !strings.Contains(missing(r)["results"], "AHS") {
		t.Errorf("format %s, missing %v", r.Program.Format, missing(r))
	}
}

func ptr[T any](v T) *T { return &v }

func TestRecordUnfinished(t *testing.T) {
	f := load(t, "cepheus-bell-100")
	f.task.Status = types.QuantumTaskStatusFailed
	r := build(t, f, cepheusCaps(t))
	m := missing(r)
	if !strings.Contains(m["results"], "FAILED") || r.Cost == nil || r.Cost.USD != "$0.00" || len(f.reads) != 2 {
		t.Errorf("missing %v, cost %+v, reads %v (want only the two submission files)", m, r.Cost, f.reads)
	}
	b, _ := record.Encode(r)
	if p := record.Verify(b); len(p) > 0 {
		t.Errorf("verify: %v", p)
	}
}

func TestRecordDeviceError(t *testing.T) {
	f := load(t, "cepheus-bell-100")
	tb, _ := price.Load()
	r, err := record.Build(context.Background(), record.Inputs{Task: f.task, Get: f.get, Prices: tb, PuffVersion: "test",
		Device: func(context.Context) (string, error) { return "", errors.New("AccessDenied for 123456789012") },
		Now:    func() time.Time { return recordedAt }})
	if err != nil {
		t.Fatal(err)
	}
	if got := missing(r)["calibration.at_completion"]; !strings.Contains(got, "AccessDenied") || strings.Contains(got, "123456789012") {
		t.Errorf("missing %q", got)
	}
	if _, err := record.Build(context.Background(), record.Inputs{}); err == nil {
		t.Error("no task accepted")
	}
}

func TestVerifyRejects(t *testing.T) {
	f := load(t, "cepheus-bell-100")
	caps := cepheusCaps(t)
	f.submitted(t, caps)
	good, _ := record.Encode(build(t, f, caps))
	decode := func(t *testing.T) map[string]any {
		t.Helper()
		dec := json.NewDecoder(strings.NewReader(string(good)))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Baseline: a generic re-encode (keys sorted, HTML escaped, compact)
	// still verifies; the hashes don't depend on the encoding.
	b, _ := json.Marshal(decode(t))
	if p := record.Verify(b); len(p) > 0 {
		t.Fatalf("a re-encoded record doesn't verify: %v", p)
	}
	cal := func(m map[string]any) map[string]any {
		return m["calibration"].(map[string]any)["at_completion"].(map[string]any)
	}
	exec0 := func(m map[string]any) map[string]any {
		return m["results"].([]any)[0].(map[string]any)["executables"].([]any)[0].(map[string]any)
	}
	cases := []struct {
		name, want string
		mut        func(map[string]any)
	}{
		{"wrong schema", "$.schema", func(m map[string]any) { m["schema"] = "puffball.record/v9" }},
		{"unknown field", `unknown field "extra"`, func(m map[string]any) { m["extra"] = 1 }},
		{"no missing", `lacks required "missing"`, func(m map[string]any) { delete(m, "missing") }},
		{"tampered source", "program.sources[0]: sha256", func(m map[string]any) { src(m)["text"] = "OPENQASM 3.0;" }},
		{"tampered compile", "compiled_programs[0]: sha256", func(m map[string]any) { m["compiled_programs"].([]any)[0].(map[string]any)["text"] = "HALT" }},
		{"tampered calibration", "calibration.at_completion: subset_sha256", func(m map[string]any) { cal(m)["qubits"] = map[string]any{} }},
		{"tampered instruction", "calibration.at_completion: subset_sha256", func(m map[string]any) { cal(m)["instructions"] = map[string]any{} }},
		{"account in arn", "$.task.arn", func(m map[string]any) {
			m["task"].(map[string]any)["arn"] = "arn:aws:braket:us-west-1:123456789012:quantum-task/x"
		}},
		{"empty missing reason", "$.missing[0].reason", func(m map[string]any) { m["missing"] = []any{map[string]any{"field": "x", "reason": ""}} }},
		{"bad count", "$.results[0].executables[0].counts.11", func(m map[string]any) { exec0(m)["counts"].(map[string]any)["11"] = "many" }},
		{"counts don't add up", "counts add up to", func(m map[string]any) { exec0(m)["counts"].(map[string]any)["11"] = json.Number("1") }},
		{"unnamed gap", "calibration.at_completion: empty or absent, and not named", func(m map[string]any) { delete(m["calibration"].(map[string]any), "at_completion") }},
		{"unnamed cost", "cost: empty or absent", func(m map[string]any) { m["cost"] = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := decode(t)
			tc.mut(m)
			b, _ := json.Marshal(m)
			p := record.Verify(b)
			if !strings.Contains(strings.Join(p, "\n"), tc.want) {
				t.Errorf("problems %v; want one containing %q", p, tc.want)
			}
		})
	}
	if p := record.Verify([]byte("{")); len(p) == 0 {
		t.Error("broken JSON verified")
	}
}

func src(m map[string]any) map[string]any {
	return m["program"].(map[string]any)["sources"].([]any)[0].(map[string]any)
}

func TestSnapshot(t *testing.T) {
	b, err := record.NewSnapshot("arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q", "us-west-1", `{"a":"<&>"}`, recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `<&>`) {
		t.Errorf("HTML escaped: %s", b)
	}
	s, err := record.ParseSnapshot(b)
	if err != nil || s.Capabilities != `{"a":"<&>"}` || !s.ReadAt.Equal(recordedAt) {
		t.Errorf("%+v %v", s, err)
	}
	if _, err := record.ParseSnapshot([]byte(strings.Replace(string(b), `<&>`, `x`, 1))); err == nil {
		t.Error("tampered snapshot accepted")
	}
	if _, err := record.NewSnapshot("x", "r", "", recordedAt); err == nil {
		t.Error("empty capabilities accepted")
	}
}

func TestSchemaIsJSON(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(record.SchemaJSON, &m); err != nil || m["title"] != record.Schema {
		t.Errorf("schema: %v %v", err, m["title"])
	}
}
