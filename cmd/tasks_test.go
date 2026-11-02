package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/results"
	"github.com/spore-host/puffball/pkg/testutil"
)

// The G0 tasks (testdata/tasks) were scrubbed to account 000000000000;
// tasksHarness gives them fakeAccount, so a leak of it is detectable.
const (
	g0Account  = fakeAccount
	g0Bell     = "617b7e2b-f77f-433b-8226-6cc55eb8d077"
	g0ProgSet  = "a543ab67-6545-4cf0-8bbe-55f55bef1aa6"
	g0BellARN  = "arn:aws:braket:us-west-1:" + g0Account + ":quantum-task/" + g0Bell
	afterG0    = "2026-10-31T00:00:00Z"
	missingID  = "00000000-0000-4000-8000-000000000000"
	unknownARN = "arn:aws:braket:us-west-1:" + g0Account + ":quantum-task/" + missingID
)

type g0Results struct{ failingResults }

func (g0Results) Account(context.Context) (string, error) { return g0Account, nil }

// withAccount rewrites the fixture tasks' account (in ARNs and bucket names)
// from the scrubbed placeholder to acct.
func withAccount(f *testutil.FakeFactory, acct string) {
	re := func(p *string) *string {
		if p == nil {
			return nil
		}
		return aws.String(strings.ReplaceAll(*p, "000000000000", acct))
	}
	for _, b := range f.Regions {
		for i := range b.Tasks {
			b.Tasks[i].QuantumTaskArn = re(b.Tasks[i].QuantumTaskArn)
			b.Tasks[i].OutputS3Bucket = re(b.Tasks[i].OutputS3Bucket)
		}
		detail := map[string]*braketsdk.GetQuantumTaskOutput{}
		for arn, g := range b.TaskDetail {
			g.QuantumTaskArn, g.OutputS3Bucket = re(g.QuantumTaskArn), re(g.OutputS3Bucket)
			detail[*re(&arn)] = g
		}
		b.TaskDetail = detail
	}
}

// tasksHarness points newFactory at the G0 task fixtures (mut can change
// them), newResults at the fixture account, and fails on any cancel unless
// the test sets one up.
func tasksHarness(t *testing.T, mut func(*testutil.FakeFactory)) *testutil.FakeFactory {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, afterG0)
	fixedNow(t, at)
	f := testutil.TasksFactory(t, "../testdata")
	withAccount(f, g0Account)
	if mut != nil {
		mut(f)
	}
	origF, origR, origC := newFactory, newResults, newCancellers
	t.Cleanup(func() { newFactory, newResults, newCancellers = origF, origR, origC })
	newFactory = func(context.Context) (pbraket.Factory, error) { return f, nil }
	newResults = func() results.Store { return g0Results{failingResults{t}} }
	newCancellers = func() gate.Cancellers {
		return func(context.Context, string) (pbraket.Canceller, error) {
			t.Error("cancel client built")
			return nil, errors.New("no cancel in this test")
		}
	}
	return f
}

// setStatus makes the G0 Bell task look unfinished.
func setStatus(f *testutil.FakeFactory, st types.QuantumTaskStatus, pos string) {
	g := f.Regions["us-west-1"].TaskDetail[g0BellARN]
	g.Status, g.EndedAt = st, nil
	g.QueueInfo.Position = aws.String(pos)
}

func TestLs(t *testing.T) {
	tasksHarness(t, nil)
	out, errs, code := runPuff(t, "ls")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"TASK", g0Bell, g0ProgSet, "Cepheus-1-108Q", "sv1", "COMPLETED", "puffball-g0", "g0c-1a1fdca0", "us-west-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}
	noAccount(t, out, errs)
}

func TestLsFilters(t *testing.T) {
	cases := []struct {
		name string
		args []string
		in   []string
		out  []string
	}{
		{"device", []string{"--device", "SV1"}, []string{"d1b61d7d"}, []string{g0Bell}},
		{"since", []string{"--since", "96h"}, []string{g0Bell}, []string{"d1b61d7d"}},
		{"project", []string{"--project", "nope"}, nil, []string{g0Bell}},
		{"state", []string{"--state", "queued"}, nil, []string{g0Bell}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tasksHarness(t, nil)
			out, errs, code := runPuff(t, append([]string{"ls"}, tc.args...)...)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, errs)
			}
			for _, s := range tc.in {
				if !strings.Contains(out, s) {
					t.Errorf("lacks %s:\n%s", s, out)
				}
			}
			for _, s := range tc.out {
				if strings.Contains(out, s) {
					t.Errorf("has %s:\n%s", s, out)
				}
			}
		})
	}
}

func TestLsUsage(t *testing.T) {
	for _, args := range [][]string{{"ls", "--state", "done"}, {"ls", "--since", "-1h"}, {"ls", "x"}} {
		tasksHarness(t, nil)
		if _, errs, code := runPuff(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want 2: %s", args, code, errs)
		}
	}
}

func TestLsJSON(t *testing.T) {
	tasksHarness(t, nil)
	out, errs, code := runPuff(t, "ls", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var got struct {
		Tasks         []taskView        `json:"tasks"`
		Complete      bool              `json:"complete"`
		RegionsFailed map[string]string `json:"regions_failed"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Complete || len(got.Tasks) != 3 || len(got.RegionsFailed) != 0 {
		t.Fatalf("got %+v", got)
	}
	if a := got.Tasks[0].TaskArn; !strings.Contains(a, ":<account>:") {
		t.Errorf("task_arn %q does not hide the account", a)
	}
	noAccount(t, out, errs)
}

// ls is a multi-region read, so it keeps #47's contract: a region it can't
// cover is named in an *IncompleteError, the rest is still listed, exit 1.
func TestLsIncompleteContract(t *testing.T) {
	tasksHarness(t, nil)
	mk := func(t *testing.T) *testutil.FakeFactory { return testutil.TasksFactory(t, "../testdata") }
	ls := func(ctx context.Context, f *testutil.FakeFactory) ([]string, error) {
		tasks, err := listTasks(ctx, f, pbraket.TaskQuery{})
		var got []string
		for _, tk := range tasks {
			got = append(got, tk.Region)
		}
		return got, err
	}
	// us-west-1 holds the G0 tasks: it can fail in discovery or mid-way
	// through the task search. eu-north-1 has devices but no tasks, so it
	// fails in discovery, and us-west-1's tasks must still be listed.
	t.Run("us-west-1", func(t *testing.T) {
		testutil.AssertIncompleteContract(t, "us-west-1", mk, ls, "SearchDevices", "SearchQuantumTasks")
	})
	t.Run("eu-north-1", func(t *testing.T) {
		testutil.AssertIncompleteContract(t, "eu-north-1", mk, ls, "SearchDevices")
	})
}

func TestLsRegionDown(t *testing.T) {
	tasksHarness(t, func(f *testutil.FakeFactory) { f.Regions["eu-north-1"].Err = errors.New("injected failure") })
	out, errs, code := runPuff(t, "ls", "--json")
	if code != exitError {
		t.Fatalf("exit %d, want 1: %s", code, errs)
	}
	if !strings.Contains(errs, "eu-north-1") || !strings.Contains(out, g0Bell) || !strings.Contains(out, `"complete": false`) || !strings.Contains(out, `"eu-north-1"`) {
		t.Errorf("a failed region must be named and the rest listed:\nstdout %s\nstderr %s", out, errs)
	}
}

func TestGet(t *testing.T) {
	for _, ref := range []string{g0Bell, g0BellARN} {
		tasksHarness(t, nil)
		out, errs, code := runPuff(t, "get", ref)
		if code != exitOK {
			t.Fatalf("%s: exit %d: %s", ref, code, errs)
		}
		for _, want := range []string{g0Bell, "COMPLETED", "Cepheus-1-108Q", "puffball-g0", "s3://amazon-braket-us-west-1-<account>/"} {
			if !strings.Contains(out, want) {
				t.Errorf("get lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "queue ") {
			t.Errorf("queue position shown for a finished task:\n%s", out)
		}
		noAccount(t, out, errs)
	}
}

func TestGetProgramSetJSON(t *testing.T) {
	tasksHarness(t, nil)
	out, errs, code := runPuff(t, "get", g0ProgSet, "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var v taskView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if v.Programs != 2 || v.Region != "us-west-1" || strings.Contains(out, g0Account) {
		t.Errorf("got %+v", v)
	}
}

func TestGetErrors(t *testing.T) {
	cases := []struct {
		ref  string
		code int
		want string
	}{
		{"not-a-task", exitUsage, "not a task ID"},
		{"arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q", exitUsage, "not a quantum task ARN"},
		{"arn:aws:braket:us-west-1:" + g0Account + ":quantum-task/not-a-uuid", exitUsage, "not a quantum task ARN"},
		{"arn:quantum-task/" + g0Bell, exitUsage, "not a quantum task ARN"},
		{missingID, exitError, missingID},
		{unknownARN, exitError, "invalid"},
	}
	for _, tc := range cases {
		tasksHarness(t, nil)
		_, errs, code := runPuff(t, "get", tc.ref)
		if code != tc.code || !strings.Contains(errs, tc.want) {
			t.Errorf("get %s: exit %d, stderr %q; want %d and %q", tc.ref, code, errs, tc.code, tc.want)
		}
	}
}

// waitSteps replaces waitSleep with one that advances the clock by the
// interval and the task through steps, one per poll.
func waitSteps(t *testing.T, f *testutil.FakeFactory, steps ...types.QuantumTaskStatus) *int {
	t.Helper()
	n := 0
	now := clockNow()
	orig := waitSleep
	t.Cleanup(func() { waitSleep = orig })
	fixedNow(t, now)
	waitSleep = func(ctx context.Context, d time.Duration) error {
		now = now.Add(d)
		clockNow = func() time.Time { return now }
		if n < len(steps) {
			setStatus(f, steps[n], "None")
		}
		n++
		return ctx.Err()
	}
	return &n
}

func TestWait(t *testing.T) {
	cases := []struct {
		name  string
		steps []types.QuantumTaskStatus
		code  int
		want  string
	}{
		{"completes", []types.QuantumTaskStatus{types.QuantumTaskStatusRunning, types.QuantumTaskStatusCompleted}, exitOK, "RUNNING"},
		{"fails", []types.QuantumTaskStatus{types.QuantumTaskStatusFailed}, exitError, "ended FAILED"},
		{"cancelled", []types.QuantumTaskStatus{types.QuantumTaskStatusCancelled}, exitError, "ended CANCELLED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tasksHarness(t, func(f *testutil.FakeFactory) { setStatus(f, types.QuantumTaskStatusQueued, "3") })
			waitSteps(t, f, tc.steps...)
			_, errs, code := runPuff(t, "wait", g0Bell)
			if code != tc.code || !strings.Contains(errs, tc.want) || !strings.Contains(errs, "QUEUED, queue position 3") {
				t.Errorf("exit %d (want %d), stderr:\n%s", code, tc.code, errs)
			}
		})
	}
}

func TestWaitTimeout(t *testing.T) {
	f := tasksHarness(t, func(f *testutil.FakeFactory) { setStatus(f, types.QuantumTaskStatusQueued, "3") })
	n := waitSteps(t, f)
	_, errs, code := runPuff(t, "wait", g0Bell, "--timeout", "35s", "--interval", "10s")
	// Polls at 0, 10, 20, 30 and 35s: the last sleep is cut to the deadline.
	if code != exitError || !strings.Contains(errs, "still QUEUED after 35s") || *n != 4 || !clockNow().Equal(mustTime(afterG0).Add(35*time.Second)) {
		t.Errorf("exit %d, polls %d: %s", code, *n, errs)
	}
}

func TestWaitInterrupted(t *testing.T) {
	f := tasksHarness(t, func(f *testutil.FakeFactory) { setStatus(f, types.QuantumTaskStatusQueued, "3") })
	waitSteps(t, f)
	orig := waitSleep
	waitSleep = func(context.Context, time.Duration) error { return context.Canceled }
	t.Cleanup(func() { waitSleep = orig })
	_, errs, code := runPuff(t, "wait", g0Bell)
	if code != exitError || !strings.Contains(errs, "not cancelled") {
		t.Errorf("exit %d: %s", code, errs)
	}
}

func TestWaitUsage(t *testing.T) {
	tasksHarness(t, nil)
	if _, errs, code := runPuff(t, "wait", g0Bell, "--interval", "0s"); code != exitUsage {
		t.Errorf("exit %d: %s", code, errs)
	}
}

func TestCancel(t *testing.T) {
	tasksHarness(t, func(f *testutil.FakeFactory) { setStatus(f, types.QuantumTaskStatusQueued, "3") })
	fc := &testutil.FakeCanceller{Status: types.CancellationStatusCancelling}
	newCancellers = func() gate.Cancellers { return fc.Factory() }
	out, errs, code := runPuff(t, "cancel", g0Bell)
	if code != exitOK || len(fc.Calls) != 1 || !strings.Contains(out, "CANCELLING") || !strings.Contains(errs, "cancelling task "+g0Bell) {
		t.Fatalf("exit %d, %d calls:\n%s\n%s", code, len(fc.Calls), out, errs)
	}
	if got := aws.ToString(fc.Calls[0].QuantumTaskArn); got != g0BellARN {
		t.Errorf("cancelled %s", got)
	}
}

func TestCancelFinishedTask(t *testing.T) {
	tasksHarness(t, nil) // the cancel client fails the test if built
	out, errs, code := runPuff(t, "cancel", g0Bell, "--json")
	if code != exitOK || !strings.Contains(errs, "nothing to cancel") || !strings.Contains(out, `"cancelled": false`) {
		t.Errorf("exit %d:\n%s\n%s", code, out, errs)
	}
}

func TestCancelError(t *testing.T) {
	tasksHarness(t, func(f *testutil.FakeFactory) { setStatus(f, types.QuantumTaskStatusQueued, "3") })
	fc := &testutil.FakeCanceller{Err: errors.New("denied for 123456789012")}
	newCancellers = func() gate.Cancellers { return fc.Factory() }
	_, errs, code := runPuff(t, "cancel", g0Bell)
	if code != exitError || !strings.Contains(errs, "denied") {
		t.Errorf("exit %d: %s", code, errs)
	}
	noAccount(t, errs)
}

func mustTime(s string) time.Time {
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return at
}

// A task not found while discovery couldn't search a region is reported as
// incomplete, naming the region, not as "not found" (law 3).
func TestGetNotFoundWithRegionDown(t *testing.T) {
	tasksHarness(t, func(f *testutil.FakeFactory) {
		b := f.Regions["eu-north-1"]
		b.Err, b.FailOn = errors.New("injected failure"), "SearchDevices"
	})
	_, errs, code := runPuff(t, "get", missingID)
	if code != exitError || !strings.Contains(errs, "eu-north-1") {
		t.Errorf("exit %d: %s", code, errs)
	}
	// Found despite the failure: the failure doesn't matter.
	tasksHarness(t, func(f *testutil.FakeFactory) {
		b := f.Regions["eu-north-1"]
		b.Err, b.FailOn = errors.New("injected failure"), "SearchDevices"
	})
	if _, errs, code := runPuff(t, "get", g0Bell); code != exitOK {
		t.Errorf("exit %d: %s", code, errs)
	}
}

func TestWaitSurvivesTransientErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fails int
		code  int
	}{{"one blip", 1, exitOK}, {"gives up", maxPollFailures, exitError}} {
		t.Run(tc.name, func(t *testing.T) {
			f := tasksHarness(t, func(f *testutil.FakeFactory) { setStatus(f, types.QuantumTaskStatusQueued, "3") })
			b := f.Regions["us-west-1"]
			polls := 0
			waitSteps(t, f)
			step := waitSleep
			waitSleep = func(ctx context.Context, d time.Duration) error {
				_ = step(ctx, d)
				polls++
				if polls <= tc.fails {
					b.Err, b.FailOn = errors.New("ThrottlingException for "+g0Account), "GetQuantumTask"
				} else {
					b.Err = nil
					setStatus(f, types.QuantumTaskStatusCompleted, "None")
				}
				return nil
			}
			_, errs, code := runPuff(t, "wait", g0Bell, "--json")
			if code != tc.code || !strings.Contains(errs, "polling "+g0Bell+" failed (1 of 3)") {
				t.Errorf("exit %d (want %d): %s", code, tc.code, errs)
			}
			noAccount(t, errs)
		})
	}
}

func TestWaitJSONOnTimeout(t *testing.T) {
	f := tasksHarness(t, func(f *testutil.FakeFactory) { setStatus(f, types.QuantumTaskStatusQueued, "3") })
	waitSteps(t, f)
	out, errs, code := runPuff(t, "wait", g0Bell, "--json", "--timeout", "5s")
	var v taskView
	if code != exitError || json.Unmarshal([]byte(out), &v) != nil || v.Status != "QUEUED" {
		t.Errorf("exit %d, stdout %q: %s", code, out, errs)
	}
}

// raceCanceller finishes the task, then fails the cancel, as the service
// would for a task that completed between puff's read and its cancel.
type raceCanceller struct{ f *testutil.FakeFactory }

func (r raceCanceller) CancelQuantumTask(context.Context, *braketsdk.CancelQuantumTaskInput, ...func(*braketsdk.Options)) (*braketsdk.CancelQuantumTaskOutput, error) {
	setStatus(r.f, types.QuantumTaskStatusCompleted, "None")
	return nil, errors.New("ValidationException: task is in a terminal state")
}

func TestCancelRace(t *testing.T) {
	f := tasksHarness(t, func(f *testutil.FakeFactory) { setStatus(f, types.QuantumTaskStatusQueued, "3") })
	newCancellers = func() gate.Cancellers {
		return func(context.Context, string) (pbraket.Canceller, error) { return raceCanceller{f}, nil }
	}
	out, errs, code := runPuff(t, "cancel", g0Bell, "--json")
	if code != exitOK || !strings.Contains(errs, "finished (COMPLETED) before it could be cancelled") || !strings.Contains(out, `"cancelled": false`) {
		t.Errorf("exit %d:\n%s\n%s", code, out, errs)
	}
}
