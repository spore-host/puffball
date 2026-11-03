package budget_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/budget"
	"github.com/spore-host/puffball/pkg/devices"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/testutil"
)

const cepheus = "arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q"

func fixtures(t *testing.T) (*testutil.FakeFactory, *devices.List, devices.Aliases) {
	t.Helper()
	f := testutil.DevicesFactory(t, "../../testdata/devices")
	tb, err := price.Load()
	if err != nil {
		t.Fatal(err)
	}
	aliases := devices.Aliases{}
	for _, d := range tb.Devices {
		aliases[d.Alias] = d.Arn
	}
	l, err := devices.Collect(context.Background(), f, aliases)
	if err != nil {
		t.Fatal(err)
	}
	return f, l, aliases
}

func TestTargets(t *testing.T) {
	_, l, aliases := fixtures(t)
	ts, err := budget.Targets(l, aliases, []string{"cepheus", "Cepheus-1-108Q"}, false)
	if err != nil || len(ts) != 1 || ts[0].Arn != cepheus || ts[0].Region != "us-west-1" {
		t.Fatalf("%+v %v", ts, err)
	}
	if _, err := budget.Targets(l, aliases, []string{"sv1"}, false); !errors.Is(err, budget.ErrNotQPU) {
		t.Errorf("sv1: %v", err)
	}
	if _, err := budget.Targets(l, aliases, []string{"nope"}, false); err == nil {
		t.Error("unknown device accepted")
	}
	all, err := budget.Targets(l, aliases, nil, true)
	if err != nil || len(all) < 3 {
		t.Fatalf("--all-qpus: %d %v", len(all), err)
	}
	for _, x := range all {
		if strings.Contains(x.Arn, "quantum-simulator") || x.Region == "" {
			t.Errorf("--all-qpus target %+v", x)
		}
		for _, d := range l.Devices {
			if d.Arn == x.Arn && d.Status == "RETIRED" {
				t.Errorf("retired %s targeted", d.Name)
			}
		}
	}
}

func TestCheckUSDAndPeriod(t *testing.T) {
	for s, ok := range map[string]bool{"1": true, "0": true, "2.50": true, "1.5": true, "-1": false, "1.234": false, "1e3": false, "": false, "$1": false} {
		if (budget.CheckUSD(s) == nil) != ok {
			t.Errorf("CheckUSD(%q) ok=%v", s, !ok)
		}
	}
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if p, err := budget.Period(time.Time{}, time.Time{}, now); p != nil || err != nil {
		t.Errorf("no period: %v %v", p, err)
	}
	p, err := budget.Period(time.Time{}, now.Add(24*time.Hour), now)
	if err != nil || !aws.ToTime(p.StartAt).Equal(now) {
		t.Errorf("until only: %+v %v", p, err)
	}
	for name, c := range map[string][2]time.Time{
		"from without until": {now, {}},
		"until in the past":  {{}, now.Add(-time.Hour)},
		"until before from":  {now.Add(48 * time.Hour), now.Add(24 * time.Hour)},
	} {
		if _, err := budget.Period(c[0], c[1], now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSetUpdateRemove(t *testing.T) {
	f, l, aliases := fixtures(t)
	ts, _ := budget.Targets(l, aliases, []string{"cepheus"}, false)
	fl := &testutil.FakeLimiter{F: f}
	ctx := context.Background()

	cs := budget.Set(ctx, f, fl.Factory(), ts, "1", nil, "g1", false)
	if len(cs) != 1 || cs[0].Action != "created" || cs[0].After != "1" {
		t.Fatalf("create: %+v", cs)
	}
	lim := f.Regions["us-west-1"].Limits
	if len(lim) != 1 || aws.ToString(lim[0].SpendingLimit) != "1" || lim[0].Tags[budget.Tag] != "g1" {
		t.Fatalf("service state %+v", lim)
	}
	if cs := budget.Set(ctx, f, fl.Factory(), ts, "1.00", nil, "g1", false); cs[0].Action != "unchanged" {
		t.Errorf("same amount: %+v", cs)
	}
	if cs := budget.Set(ctx, f, fl.Factory(), ts, "2.50", nil, "g1", false); cs[0].Action != "updated" || cs[0].Before != "1" || cs[0].After != "2.50" {
		t.Errorf("update: %+v", cs)
	}
	// Below current + queued spend: Braket rejects it, and it is reported.
	f.Regions["us-west-1"].Limits[0].TotalSpend = aws.String("2.00")
	if cs := budget.Set(ctx, f, fl.Factory(), ts, "1", nil, "g1", false); !budget.Failed(cs) || !strings.Contains(cs[0].Error, "current spend plus queued spend") {
		t.Errorf("update below spend: %+v", cs)
	}
	views := budget.Views([]pbraket.Limit{{Region: "us-west-1", Summary: f.Regions["us-west-1"].Limits[0]}})
	if v := views[0]; v.LimitUSD != "2.50" || v.SpentUSD != "2.00" || v.Remaining != "$0.50" || !v.ByPuff || v.Project != "g1" || len(v.Missing) != 0 {
		t.Errorf("view %+v", v)
	}
	if cs := budget.Remove(ctx, f, fl.Factory(), ts, false); cs[0].Action != "deleted" || len(f.Regions["us-west-1"].Limits) != 0 {
		t.Errorf("remove: %+v", cs)
	}
	if cs := budget.Remove(ctx, f, fl.Factory(), ts, false); cs[0].Action != "none" {
		t.Errorf("remove again: %+v", cs)
	}
}

// A limit someone else made is theirs: kept unless --include-others.
func TestOthersKept(t *testing.T) {
	f, l, aliases := fixtures(t)
	ts, _ := budget.Targets(l, aliases, []string{"cepheus"}, false)
	f.Regions["us-west-1"].Limits = []types.SpendingLimitSummary{{SpendingLimitArn: aws.String("arn:aws:braket:us-west-1:000000000000:spending-limit/theirs"),
		DeviceArn: aws.String(cepheus), SpendingLimit: aws.String("10"), TotalSpend: aws.String("0"), QueuedSpend: aws.String("0")}}
	fl := &testutil.FakeLimiter{F: f}
	ctx := context.Background()
	if cs := budget.Set(ctx, f, fl.Factory(), ts, "1", nil, "g1", false); cs[0].Action != "kept" || !cs[0].NotPuffs || len(fl.Calls) != 0 {
		t.Errorf("set over another's limit: %+v, calls %v", cs, fl.Calls)
	}
	if cs := budget.Remove(ctx, f, fl.Factory(), ts, false); cs[0].Action != "kept" || len(fl.Calls) != 0 {
		t.Errorf("rm another's limit: %+v", cs)
	}
	if cs := budget.Set(ctx, f, fl.Factory(), ts, "1", nil, "g1", true); cs[0].Action != "updated" || !cs[0].NotPuffs {
		t.Errorf("--include-others set: %+v", cs)
	}
	if cs := budget.Remove(ctx, f, fl.Factory(), ts, true); cs[0].Action != "deleted" {
		t.Errorf("--include-others rm: %+v", cs)
	}
}

func TestSetErrors(t *testing.T) {
	f, l, aliases := fixtures(t)
	ts, _ := budget.Targets(l, aliases, []string{"cepheus", "garnet"}, false)
	f.Regions["eu-north-1"].Err = errors.New("injected for 123456789012")
	fl := &testutil.FakeLimiter{F: f}
	cs := budget.Set(context.Background(), f, fl.Factory(), ts, "1", nil, "g1", false)
	byName := map[string]budget.Change{}
	for _, c := range cs {
		byName[c.Name] = c
	}
	if byName["Garnet"].Action != "failed" || strings.Contains(byName["Garnet"].Error, "123456789012") || byName["Cepheus-1-108Q"].Action != "created" {
		t.Errorf("one region failing must not stop the other: %+v", cs)
	}
	fl.Err = errors.New("AccessDenied")
	f.Regions["eu-north-1"].Err = nil
	if cs := budget.Remove(context.Background(), f, fl.Factory(), ts, false); !budget.Failed(cs) {
		t.Errorf("delete error not reported: %+v", cs)
	}
	var ls gate.Limiters = func(context.Context, string) (pbraket.Limiter, error) { return nil, errors.New("no client") }
	if cs := budget.Set(context.Background(), f, ls, ts[:1], "3", nil, "g1", false); !budget.Failed(cs) {
		t.Errorf("client error not reported: %+v", cs)
	}
}

func TestViewsMissing(t *testing.T) {
	v := budget.Views([]pbraket.Limit{{Region: "us-west-1", Summary: types.SpendingLimitSummary{DeviceArn: aws.String(cepheus), SpendingLimit: aws.String("1")}}})[0]
	if v.Remaining != "" || strings.Join(v.Missing, ",") != "queued_usd,remaining_usd,spent_usd" || v.ByPuff {
		t.Errorf("%+v", v)
	}
}

func TestSearchLimitsContract(t *testing.T) {
	mk := func(t *testing.T) *testutil.FakeFactory {
		f := testutil.DevicesFactory(t, "../../testdata/devices")
		f.Regions["us-west-1"].Limits = []types.SpendingLimitSummary{{DeviceArn: aws.String(cepheus), SpendingLimit: aws.String("1")}}
		f.Regions["eu-north-1"].Limits = []types.SpendingLimitSummary{
			{DeviceArn: aws.String("arn:aws:braket:eu-north-1::device/qpu/iqm/Garnet"), SpendingLimit: aws.String("1")},
			{DeviceArn: aws.String("arn:aws:braket:eu-north-1::device/qpu/iqm/Emerald"), SpendingLimit: aws.String("1")},
		}
		return f
	}
	op := func(ctx context.Context, f *testutil.FakeFactory) ([]string, error) {
		ls, err := pbraket.SearchLimits(ctx, f, []string{"eu-north-1", "eu-west-2", "us-east-1", "us-west-1", "us-west-2"})
		var got []string
		for _, l := range ls {
			got = append(got, l.Region)
		}
		return got, err
	}
	testutil.AssertIncompleteContract(t, "eu-north-1", mk, op, "SearchSpendingLimits")
}
