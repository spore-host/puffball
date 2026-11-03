package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/testutil"
)

// budgetHarness points newFactory at the G0 device fixtures and
// newLimiters at a FakeLimiter changing them.
func budgetHarness(t *testing.T, mut func(*testutil.FakeFactory)) (*testutil.FakeFactory, *testutil.FakeLimiter) {
	t.Helper()
	f := testutil.DevicesFactory(t, "../testdata/devices")
	if mut != nil {
		mut(f)
	}
	fl := &testutil.FakeLimiter{F: f}
	origF, origL := newFactory, newLimiters
	t.Cleanup(func() { newFactory, newLimiters = origF, origL })
	newFactory = func(context.Context) (pbraket.Factory, error) { return f, nil }
	newLimiters = func() gate.Limiters { return fl.Factory() }
	return f, fl
}

func TestBudgetSetLsRm(t *testing.T) {
	f, fl := budgetHarness(t, nil)
	out, errs, code := runPuff(t, "budget", "set", "cepheus", "--usd", "1", "--project", "puffball-g1")
	if code != exitOK || !strings.Contains(out, "Cepheus-1-108Q  us-west-1  created  $1") {
		t.Fatalf("set: exit %d:\n%s\n%s", code, out, errs)
	}
	if l := f.Regions["us-west-1"].Limits; len(l) != 1 || l[0].Tags["puffball:project"] != "puffball-g1" {
		t.Fatalf("service state %+v", l)
	}
	out, errs, code = runPuff(t, "budget", "ls")
	if code != exitOK || !strings.Contains(out, "Cepheus-1-108Q") || !strings.Contains(out, "yes (puffball-g1)") || !strings.Contains(out, "Not a bill") {
		t.Errorf("ls: exit %d:\n%s\n%s", code, out, errs)
	}
	noAccount(t, out, errs)
	out, _, code = runPuff(t, "budget", "ls", "--json")
	var got struct {
		Limits []struct {
			Device    string `json:"device"`
			Remaining string `json:"remaining_usd"`
		} `json:"limits"`
		Complete bool `json:"complete"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != exitOK || !got.Complete || len(got.Limits) != 1 || got.Limits[0].Remaining != "1.00" {
		t.Errorf("ls --json: exit %d %+v %v\n%s", code, got, err, out)
	}
	if out, errs, code := runPuff(t, "budget", "rm", "cepheus"); code != exitOK || !strings.Contains(out, "deleted") || len(f.Regions["us-west-1"].Limits) != 0 {
		t.Errorf("rm: exit %d:\n%s\n%s", code, out, errs)
	}
	if len(fl.Calls) != 2 {
		t.Errorf("calls %v", fl.Calls)
	}
}

func TestBudgetUsage(t *testing.T) {
	for _, args := range [][]string{
		{"budget", "set", "cepheus"},
		{"budget", "set", "cepheus", "--usd", "lots"},
		{"budget", "set", "--usd", "1"},
		{"budget", "set", "sv1", "--usd", "1"},
		{"budget", "set", "cepheus", "--usd", "1", "--until", "soon"},
		{"budget", "set", "cepheus", "--usd", "1", "--until", "2020-01-01"},
		{"budget", "set", "cepheus", "--usd", "1", "--from", "2030-01-01"},
		{"budget", "set", "cepheus", "--usd", "1", "--project", ""},
		{"budget", "rm"},
		{"budget", "set", "--all-qpus", "--include-others", "--usd", "1"},
		{"budget", "rm", "--all-qpus", "--include-others"},
		{"budget", "rm", "--all-qpus", "cepheus"},
		{"budget", "ls", "x"},
	} {
		_, fl := budgetHarness(t, nil)
		if _, errs, code := runPuff(t, args...); code != exitUsage || len(fl.Calls) != 0 {
			t.Errorf("%v: exit %d, calls %v: %s", args, code, fl.Calls, errs)
		}
	}
}

func TestBudgetUntil(t *testing.T) {
	f, _ := budgetHarness(t, nil)
	fixedNow(t, mustTime("2026-11-02T00:00:00Z"))
	if _, errs, code := runPuff(t, "budget", "set", "cepheus", "--usd", "1", "--until", "2026-12-31"); code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	// A date-only --until runs through that day.
	p := f.Regions["us-west-1"].Limits[0].TimePeriod
	if p == nil || aws.ToTime(p.EndAt) != mustTime("2027-01-01T00:00:00Z") || aws.ToTime(p.StartAt) != mustTime("2026-11-02T00:00:00Z") {
		t.Errorf("period %+v", p)
	}
	out, _, _ := runPuff(t, "budget", "ls")
	if !strings.Contains(out, "2026-11-02 00:00Z to 2027-01-01 00:00Z") {
		t.Errorf("ls period:\n%s", out)
	}
}

func TestBudgetKeepsOthers(t *testing.T) {
	_, fl := budgetHarness(t, func(f *testutil.FakeFactory) {
		f.Regions["us-west-1"].Limits = []types.SpendingLimitSummary{{SpendingLimitArn: aws.String("arn:aws:braket:us-west-1:" + fakeAccount + ":spending-limit/x"),
			DeviceArn: aws.String("arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q"), SpendingLimit: aws.String("10"), TotalSpend: aws.String("0"), QueuedSpend: aws.String("0")}}
	})
	out, errs, code := runPuff(t, "budget", "rm", "cepheus")
	if code != exitOK || !strings.Contains(out, "kept (ownership unknown") || len(fl.Calls) != 0 {
		t.Errorf("exit %d, calls %v:\n%s\n%s", code, fl.Calls, out, errs)
	}
	noAccount(t, out, errs)
}

func TestBudgetFailures(t *testing.T) {
	// A limiter error is reported per device and exits 1.
	_, fl := budgetHarness(t, nil)
	fl.Err = errors.New("AccessDenied for " + fakeAccount)
	out, errs, code := runPuff(t, "budget", "set", "cepheus", "--usd", "1", "--json")
	if code != exitError || !strings.Contains(out, `"action": "failed"`) {
		t.Errorf("exit %d:\n%s\n%s", code, out, errs)
	}
	noAccount(t, out, errs)
	// --all-qpus over a partial device list would skip QPUs: refused.
	budgetHarness(t, func(f *testutil.FakeFactory) { f.Regions["eu-north-1"].Err = errors.New("injected failure") })
	if _, errs, code := runPuff(t, "budget", "set", "--all-qpus", "--usd", "1"); code != exitError || !strings.Contains(errs, "--all-qpus needs every region") {
		t.Errorf("--all-qpus partial: exit %d: %s", code, errs)
	}
	// ls with a region down names it, on stdout and stderr, and exits 1.
	budgetHarness(t, func(f *testutil.FakeFactory) { f.Regions["eu-north-1"].Err = errors.New("injected failure") })
	if out, errs, code := runPuff(t, "budget", "ls"); code != exitError || !strings.Contains(errs, "eu-north-1") || !strings.Contains(out, "INCOMPLETE: eu-north-1") {
		t.Errorf("ls region down: exit %d:\n%s\n%s", code, out, errs)
	}
	// A device in a region that couldn't be searched: exit 1 naming it, not 2.
	budgetHarness(t, func(f *testutil.FakeFactory) { f.Regions["eu-north-1"].Err = errors.New("injected failure") })
	if _, errs, code := runPuff(t, "budget", "set", "garnet", "--usd", "1"); code != exitError || !strings.Contains(errs, "eu-north-1") {
		t.Errorf("set garnet, eu-north-1 down: exit %d: %s", code, errs)
	}
	// rm --all-qpus needs every region.
	budgetHarness(t, func(f *testutil.FakeFactory) { f.Regions["eu-north-1"].Err = errors.New("injected failure") })
	if _, errs, code := runPuff(t, "budget", "rm", "--all-qpus"); code != exitError || !strings.Contains(errs, "needs every region") {
		t.Errorf("rm --all-qpus partial: exit %d: %s", code, errs)
	}
}

func TestBudgetAllQPUs(t *testing.T) {
	f, _ := budgetHarness(t, nil)
	out, errs, code := runPuff(t, "budget", "set", "--all-qpus", "--usd", "2")
	if code != exitOK || strings.Count(out, "created") < 3 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errs)
	}
	for region, b := range f.Regions {
		for _, l := range b.Limits {
			if !strings.Contains(aws.ToString(l.DeviceArn), ":"+region+"::device/qpu/") {
				t.Errorf("limit for %s created in %s", aws.ToString(l.DeviceArn), region)
			}
		}
	}
	if _, _, code := runPuff(t, "budget", "rm", "--all-qpus"); code != exitOK {
		t.Errorf("rm --all-qpus: exit %d", code)
	}
	for _, b := range f.Regions {
		if len(b.Limits) != 0 {
			t.Errorf("limits left: %+v", b.Limits)
		}
	}
}
