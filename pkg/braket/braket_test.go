package braket_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/testutil"
)

const devicesDir = "../../testdata/devices"

// TestDiscoverFromG0Fixtures replays the G0 capture: five regions from SSM,
// every region searched, the QPUs where G0 found them.
func TestDiscoverFromG0Fixtures(t *testing.T) {
	f := testutil.DevicesFactory(t, devicesDir)
	inv, err := pbraket.Discover(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Err() != nil {
		t.Fatalf("Err() = %v, want complete", inv.Err())
	}
	want := []string{"eu-north-1", "eu-west-2", "us-east-1", "us-west-1", "us-west-2"}
	if got := inv.Regions(); !reflect.DeepEqual(got, want) {
		t.Errorf("Regions() = %v, want %v", got, want)
	}
	found := map[string]string{}
	for r, devs := range inv.Devices {
		for _, d := range devs {
			found[aws.ToString(d.DeviceName)] = r
		}
	}
	for name, region := range map[string]string{"Cepheus-1-108Q": "us-west-1", "Garnet": "eu-north-1", "Aquila": "us-east-1"} {
		if found[name] != region {
			t.Errorf("%s found in %q, want %q", name, found[name], region)
		}
	}
}

func TestDiscoverPartialFailures(t *testing.T) {
	boom := errors.New("throttled")
	cases := []struct {
		name   string
		mutate func(*testutil.FakeFactory)
		failed []string
	}{
		{"region errors", func(f *testutil.FakeFactory) { f.Regions["eu-north-1"].Err = boom }, []string{"eu-north-1"}},
		{"region unreachable", func(f *testutil.FakeFactory) { delete(f.Regions, "us-east-1") }, []string{"us-east-1"}},
		{"mid-pagination error", func(f *testutil.FakeFactory) {
			// us-west-1 has two pages in the fixture; fail on the second.
			f.Regions["us-west-1"].Err, f.Regions["us-west-1"].FailAfter = boom, 1
		}, []string{"us-west-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testutil.DevicesFactory(t, devicesDir)
			tc.mutate(f)
			inv, err := pbraket.Discover(context.Background(), f)
			ie, ok := pbraket.AsIncomplete(err)
			if !ok || inv == nil {
				t.Fatalf("Discover = %v, %v; want the partial inventory and an *IncompleteError", inv, err)
			}
			if inv.Err() != err {
				t.Error("Inventory.Err() differs from the returned error")
			}
			if !reflect.DeepEqual(ie.Regions(), tc.failed) {
				t.Errorf("failed regions = %v, want %v", ie.Regions(), tc.failed)
			}
			for _, r := range tc.failed {
				if _, ok := inv.Devices[r]; ok {
					t.Errorf("failed region %s still has a device list; a partial list must not look complete", r)
				}
				if !strings.Contains(inv.Err().Error(), r) {
					t.Errorf("error %q does not name %s", inv.Err(), r)
				}
			}
			if got := inv.Regions(); len(got) != 5 {
				t.Errorf("Regions() = %v; failed regions must still be listed", got)
			}
		})
	}
}

func TestDiscoverSSMFailureIsAnError(t *testing.T) {
	f := testutil.DevicesFactory(t, devicesDir)
	f.SSM.Err = errors.New("AccessDenied")
	inv, err := pbraket.Discover(context.Background(), f)
	if err == nil || !strings.Contains(err.Error(), "SSM") || inv != nil {
		t.Fatalf("Discover = %v, %v; want nil and an SSM error (no silent fallback to the seed region)", inv, err)
	}
	if _, ok := pbraket.AsIncomplete(err); ok {
		t.Error("an SSM failure must be distinguishable from a per-region failure")
	}
	// Second of two SSM pages fails.
	f.SSM.FailAfter, f.SSM.PageSize = 1, 3
	if _, err := pbraket.Discover(context.Background(), f); err == nil {
		t.Fatal("an SSM failure part-way through the region list was not an error")
	}
	f.SSM.Err = nil
	f.SSM.Regions = append(f.SSM.Regions, "us-west-1", "us-west-1")
	inv, err = pbraket.Discover(context.Background(), f)
	if err != nil || len(inv.SSMRegions) != 5 {
		t.Fatalf("duplicate SSM regions: %v, %v", inv, err)
	}
	f.SSM.Regions = nil
	if _, err := pbraket.Discover(context.Background(), f); err == nil {
		t.Fatal("an empty SSM region list must be an error")
	}
}

// TestIncompleteErrorScrubsAccountIDs: SDK errors carry principal ARNs (law 7).
func TestIncompleteErrorScrubsAccountIDs(t *testing.T) {
	err := &pbraket.IncompleteError{Op: "SearchDevices", Failed: map[string]error{
		"us-west-1": errors.New("AccessDeniedException: User: arn:aws:iam::123456789012:user/alice is not authorized"),
		"us-east-1": errors.New("User agreement has not been accepted for 210987654321. Please visit the console"),
	}}
	if msg := err.Error(); strings.Contains(msg, "123456789012") || strings.Contains(msg, "alice") || strings.Contains(msg, "210987654321") {
		t.Errorf("Error() leaks the account or principal: %s", msg)
	}
}

// TestDiscoverFollowsDeviceARNs: a region named by a device ARN but missing
// from SSM is searched too.
func TestDiscoverFollowsDeviceARNs(t *testing.T) {
	f := &testutil.FakeFactory{
		SeedRegion: "us-west-2",
		SSM:        &testutil.FakeSSM{Regions: []string{"us-west-2"}},
		Regions: map[string]*testutil.FakeBraket{
			"us-west-2": {DevicePages: [][]types.DeviceSummary{{{DeviceArn: aws.String("arn:aws:braket:ap-new-1::device/qpu/x/Y"), DeviceName: aws.String("Y")}}}},
			"ap-new-1":  {DevicePages: [][]types.DeviceSummary{{{DeviceArn: aws.String("arn:aws:braket:ap-new-1::device/qpu/x/Y"), DeviceName: aws.String("Y")}}}},
		},
	}
	inv, err := pbraket.Discover(context.Background(), f)
	if err != nil || inv.Err() != nil {
		t.Fatal(err, inv.Err())
	}
	if !reflect.DeepEqual(inv.Regions(), []string{"ap-new-1", "us-west-2"}) {
		t.Errorf("Regions() = %v", inv.Regions())
	}
}

func TestDiscoverCancelled(t *testing.T) {
	f := testutil.DevicesFactory(t, devicesDir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pbraket.Discover(ctx, f); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery: err = %v, want context.Canceled", err)
	}
}

func TestFanOutCancelledMidway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	res := pbraket.FanOut(ctx, []string{"b", "a"}, func(context.Context, string) (int, error) {
		cancel()
		return 1, nil
	})
	if res[0].Region != "a" || res[1].Region != "b" {
		t.Fatalf("results not sorted by region: %v", res)
	}
	err := pbraket.Incomplete("op", res)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Incomplete = %v, want it to wrap context.Canceled", err)
	}
}

func TestRegionOf(t *testing.T) {
	cases := map[string]string{
		"arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q":           "us-west-1",
		"arn:aws:braket:::device/quantum-simulator/amazon/sv1":                  "",
		"arn:aws:braket:us-west-1:000000000000:quantum-task/617b7e2b-f77f-433b": "us-west-1",
		"not-an-arn": "",
	}
	for arn, want := range cases {
		if got := pbraket.RegionOf(arn); got != want {
			t.Errorf("RegionOf(%q) = %q, want %q", arn, got, want)
		}
	}
}

func task(arn, status string, created time.Time, tags map[string]string) types.QuantumTaskSummary {
	return types.QuantumTaskSummary{QuantumTaskArn: aws.String(arn), Status: types.QuantumTaskStatus(status), CreatedAt: aws.Time(created), Tags: tags}
}

func taskFactory() *testutil.FakeFactory {
	t0 := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	return &testutil.FakeFactory{
		SeedRegion: "us-west-1",
		Regions: map[string]*testutil.FakeBraket{
			"us-west-1": {TaskPage: 1, Tasks: []types.QuantumTaskSummary{
				task("arn:t/1", "COMPLETED", t0, map[string]string{"puffball:project": "g0"}),
				task("arn:t/2", "QUEUED", t0.Add(time.Minute), map[string]string{"puffball:project": "g1"}),
				task("arn:t/3", "RUNNING", t0.Add(-48*time.Hour), nil),
			}},
			"us-east-1": {Tasks: []types.QuantumTaskSummary{
				task("arn:t/4", "QUEUED", t0.Add(2*time.Minute), map[string]string{"puffball:project": "g1", "puffball:rung": "qpu"}),
			}},
		},
	}
}

func arns(ts []pbraket.Task) []string {
	var out []string
	for _, t := range ts {
		out = append(out, aws.ToString(t.Summary.QuantumTaskArn))
	}
	return out
}

func TestSearchTasks(t *testing.T) {
	regions := []string{"us-east-1", "us-west-1"}
	since := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		q    pbraket.TaskQuery
		want []string
	}{
		{"everything, paged one per page", pbraket.TaskQuery{}, []string{"arn:t/3", "arn:t/1", "arn:t/2", "arn:t/4"}},
		{"several statuses, one search each", pbraket.TaskQuery{Statuses: []types.QuantumTaskStatus{"QUEUED", "RUNNING"}}, []string{"arn:t/3", "arn:t/2", "arn:t/4"}},
		{"since", pbraket.TaskQuery{Since: since}, []string{"arn:t/1", "arn:t/2", "arn:t/4"}},
		{"tag value", pbraket.TaskQuery{Tags: map[string]string{"puffball:project": "g1"}}, []string{"arn:t/2", "arn:t/4"}},
		{"tag present, any value", pbraket.TaskQuery{Tags: map[string]string{"puffball:rung": ""}}, []string{"arn:t/4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pbraket.SearchTasks(context.Background(), taskFactory(), regions, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(arns(got), tc.want) {
				t.Errorf("got %v, want %v", arns(got), tc.want)
			}
		})
	}
}

func TestSearchTasksStatusSearchesAreSeparate(t *testing.T) {
	f := taskFactory()
	_, err := pbraket.SearchTasks(context.Background(), f, []string{"us-east-1"}, pbraket.TaskQuery{Statuses: []types.QuantumTaskStatus{"QUEUED", "RUNNING", "CREATED"}})
	if err != nil {
		t.Fatalf("a multi-status query must not send a multi-value filter: %v", err)
	}
	if n := len(f.Regions["us-east-1"].Calls()); n != 3 {
		t.Errorf("%d searches, want one per status (3)", n)
	}
}

// TestSearchTasksLifecycleOrder: per-status searches run in lifecycle order,
// whatever order the caller lists them in, so a task moving forward between
// searches is caught at its later status.
func TestSearchTasksLifecycleOrder(t *testing.T) {
	f := taskFactory()
	_, err := pbraket.SearchTasks(context.Background(), f, []string{"us-east-1"}, pbraket.TaskQuery{
		Statuses: []types.QuantumTaskStatus{"RUNNING", "COMPLETED", "QUEUED", "CREATED", "QUEUED", "MYSTERY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := f.Regions["us-east-1"].Searches()
	want := []string{"CREATED", "QUEUED", "RUNNING", "COMPLETED", "MYSTERY"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("search order = %v, want %v", got, want)
	}
}

func TestSearchTasksPartialFailure(t *testing.T) {
	f := taskFactory()
	f.Regions["us-west-1"].Err, f.Regions["us-west-1"].FailAfter = errors.New("connection reset"), 1
	got, err := pbraket.SearchTasks(context.Background(), f, []string{"us-east-1", "us-west-1", "eu-north-1"}, pbraket.TaskQuery{})
	ie, ok := pbraket.AsIncomplete(err)
	if !ok {
		t.Fatalf("err = %v, want *IncompleteError", err)
	}
	if !reflect.DeepEqual(ie.Regions(), []string{"eu-north-1", "us-west-1"}) {
		t.Errorf("failed = %v", ie.Regions())
	}
	if !reflect.DeepEqual(arns(got), []string{"arn:t/4"}) {
		t.Errorf("tasks = %v; a region that failed part-way must contribute none", arns(got))
	}
}

func TestSearchTasksFromG0Fixture(t *testing.T) {
	var out braket.SearchQuantumTasksOutput
	testutil.LoadJSON(t, "../../testdata/tasks/tags/search-createdAt.json", &out)
	if len(out.QuantumTasks) == 0 {
		t.Fatal("fixture decoded no tasks")
	}
	f := &testutil.FakeFactory{Regions: map[string]*testutil.FakeBraket{"us-west-1": {Tasks: out.QuantumTasks}}}
	got, err := pbraket.SearchTasks(context.Background(), f, []string{"us-west-1"}, pbraket.TaskQuery{
		Statuses: []types.QuantumTaskStatus{types.QuantumTaskStatusCompleted},
		Tags:     map[string]string{"puffball:run-id": "g0c-1a1fdca0"},
	})
	if err != nil || len(got) != len(out.QuantumTasks) {
		t.Fatalf("got %d tasks, err %v; want all %d G0 tasks", len(got), err, len(out.QuantumTasks))
	}
	if got[0].Summary.CreatedAt == nil {
		t.Error("createdAt did not decode from the live response")
	}
}

func TestMatchTags(t *testing.T) {
	tags := map[string]string{"a": "1", "b": "2"}
	cases := []struct {
		want map[string]string
		ok   bool
	}{
		{nil, true}, {map[string]string{"a": "1"}, true}, {map[string]string{"a": ""}, true},
		{map[string]string{"a": "2"}, false}, {map[string]string{"c": ""}, false},
	}
	for _, tc := range cases {
		if got := pbraket.MatchTags(tags, tc.want); got != tc.ok {
			t.Errorf("MatchTags(%v) = %v, want %v", tc.want, got, tc.ok)
		}
	}
}

// TestActionJSONMatchesG0Submission: the program and program-set JSON are
// byte-identical to what G0 submitted and the service accepted (#4).
func TestActionJSONMatchesG0Submission(t *testing.T) {
	bell := "OPENQASM 3.0;\nbit[2] b;\nqubit[2] q;\nh q[0];\ncnot q[0], q[1];\nb[0] = measure q[0];\nb[1] = measure q[1];"
	flipped := "OPENQASM 3.0;\nbit[2] b;\nqubit[2] q;\nh q[0];\ncnot q[0], q[1];\nx q[1];\nb[0] = measure q[0];\nb[1] = measure q[1];"
	got, err := pbraket.OpenQASMAction(pbraket.Program{Source: bell})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"braketSchemaHeader":{"name":"braket.ir.openqasm.program","version":"1"},"source":` + jsonString(bell) + `}`
	if string(got) != want {
		t.Errorf("program JSON\n got %s\nwant %s", got, want)
	}
	set, n, err := pbraket.ProgramSetAction([]pbraket.Program{{Source: bell}, {Source: flipped}})
	if err != nil || n != 2 {
		t.Fatalf("ProgramSetAction: n=%d err=%v", n, err)
	}
	wantSet := `{"braketSchemaHeader":{"name":"braket.ir.openqasm.program_set","version":"1"},"programs":[` +
		`{"braketSchemaHeader":{"name":"braket.ir.openqasm.program","version":"1"},"source":` + jsonString(bell) + `},` +
		`{"braketSchemaHeader":{"name":"braket.ir.openqasm.program","version":"1"},"source":` + jsonString(flipped) + `}]}`
	if string(set) != wantSet {
		t.Errorf("program set JSON\n got %s\nwant %s", set, wantSet)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestProgramSetExecutables(t *testing.T) {
	src := "OPENQASM 3.0; input float a; qubit q; rx(a) q;"
	cases := []struct {
		name    string
		progs   []pbraket.Program
		n       int
		wantErr string
	}{
		{"no inputs is one each", []pbraket.Program{{Source: src}, {Source: src}}, 2, ""},
		{"list inputs", []pbraket.Program{{Source: src, Inputs: map[string]any{"a": []any{0.0, 1.0, 2.0}}}, {Source: src}}, 4, ""},
		{"empty set", nil, 0, "no programs"},
		{"no source", []pbraket.Program{{Source: " "}}, 0, "no source"},
		{"scalar input", []pbraket.Program{{Source: src, Inputs: map[string]any{"a": 1.0}}}, 0, "not a list"},
		{"uneven inputs", []pbraket.Program{{Source: src, Inputs: map[string]any{"a": []any{1.0}, "b": []any{1.0, 2.0}}}}, 0, "different lengths"},
		{"empty list", []pbraket.Program{{Source: src, Inputs: map[string]any{"a": []any{}}}}, 0, "empty"},
		{"empty input name", []pbraket.Program{{Source: src, Inputs: map[string]any{"": []any{1.0}}}}, 0, "empty name"},
		{"typed slice is a list", []pbraket.Program{{Source: src, Inputs: map[string]any{"a": []float64{1, 2}}}}, 2, ""},
		{"bit-string list", []pbraket.Program{{Source: src, Inputs: map[string]any{"a": []string{"01", "10", "11"}}}}, 3, ""},
		{"non-binary string", []pbraket.Program{{Source: src, Inputs: map[string]any{"a": []any{"abc"}}}}, 0, "not a bit string"},
		{"bool", []pbraket.Program{{Source: src, Inputs: map[string]any{"a": []any{true}}}}, 0, "not a number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, n, err := pbraket.ProgramSetAction(tc.progs)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || n != tc.n {
				t.Fatalf("n = %d, err = %v; want %d", n, err, tc.n)
			}
		})
	}
	if _, err := pbraket.OpenQASMAction(pbraket.Program{}); err == nil {
		t.Error("OpenQASMAction accepted an empty program")
	}
	for name, v := range map[string]any{"int": 3, "float": 0.5, "bits": "0101", "json number": json.Number("1e-3")} {
		if _, err := pbraket.OpenQASMAction(pbraket.Program{Source: src, Inputs: map[string]any{"a": v}}); err != nil {
			t.Errorf("%s input rejected: %v", name, err)
		}
	}
	for name, v := range map[string]any{"string": "abc", "bool": false, "nil": nil, "map": map[string]any{}} {
		if _, err := pbraket.OpenQASMAction(pbraket.Program{Source: src, Inputs: map[string]any{"a": v}}); err == nil {
			t.Errorf("%s input accepted", name)
		}
	}
}

func TestActionJSONIsNotHTMLEscaped(t *testing.T) {
	src := "OPENQASM 3.0; bit b; qubit q; if (b < 1) { x q; } b = measure q;"
	got, err := pbraket.OpenQASMAction(pbraket.Program{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "b < 1") || strings.HasSuffix(string(got), "\n") {
		t.Errorf("source escaped or trailing newline: %s", got)
	}
}

const ahsProgram = `{
 "setup": {"ahs_register": {"sites": [["0.0", "0.0"], ["0.0000055", "0.0"]], "filling": [1, 1]}},
 "hamiltonian": {"drivingFields": [], "localDetuning": []}
}`

func TestAHSAction(t *testing.T) {
	got, err := pbraket.AHSAction([]byte(ahsProgram))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"braketSchemaHeader":{"name":"braket.ir.ahs.program","version":"1"}`) {
		t.Errorf("header not set: %s", got)
	}
	if !strings.Contains(string(got), `"0.0000055"`) {
		t.Errorf("site coordinate changed: %s", got)
	}
	// Numeric coordinates keep their exact text.
	num := strings.Replace(ahsProgram, `"0.0000055"`, `5.5e-6`, 1)
	if got, err := pbraket.AHSAction([]byte(num)); err != nil || !strings.Contains(string(got), `5.5e-6`) {
		t.Errorf("numeric coordinate not preserved verbatim: %s %v", got, err)
	}
	alias := strings.Replace(ahsProgram, "localDetuning", "shiftingFields", 1)
	if _, err := pbraket.AHSAction([]byte(alias)); err != nil {
		t.Errorf("shiftingFields alias rejected: %v", err)
	}

	bad := map[string]string{
		"not json":      `[1,2`,
		"trailing data": ahsProgram + `{}`,
		"wrong header":  `{"braketSchemaHeader":{"name":"braket.ir.openqasm.program","version":"1"},` + ahsProgram[1:],
		"no sites":      strings.Replace(ahsProgram, `"sites"`, `"x"`, 1),
		"no filling":    strings.Replace(ahsProgram, `"filling"`, `"x"`, 1),
		"no driving":    strings.Replace(ahsProgram, `"drivingFields"`, `"x"`, 1),
		"no detuning":   strings.Replace(ahsProgram, `"localDetuning"`, `"x"`, 1),
	}
	for name, in := range bad {
		if _, err := pbraket.AHSAction([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestReadInterfaceHasNoBillableMethod guards law 1 at the type level: the
// read interface must not grow a Create method.
func TestReadInterfaceHasNoBillableMethod(t *testing.T) {
	api := reflect.TypeOf((*pbraket.API)(nil)).Elem()
	for i := 0; i < api.NumMethod(); i++ {
		if strings.HasPrefix(api.Method(i).Name, "Create") {
			t.Errorf("braket.API has billable method %s; billable calls belong to Creator, held by pkg/gate", api.Method(i).Name)
		}
	}
}
