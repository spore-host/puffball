package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

func table(t *testing.T) *Table {
	t.Helper()
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func dev(t *testing.T, tb *Table, alias string) Device {
	t.Helper()
	d, err := tb.Lookup(alias)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// g0Products reads the Price List pages captured in G0(b) (#3).
func g0Products(t *testing.T) []Product {
	t.Helper()
	pages, _ := filepath.Glob("../../testdata/pricing/get-products.*.json")
	sort.Strings(pages)
	if len(pages) == 0 {
		t.Fatal("no Price List fixtures")
	}
	var entries []string
	for _, p := range pages {
		b, err := os.ReadFile(p) //nolint:gosec // fixtures
		if err != nil {
			t.Fatal(err)
		}
		var page struct{ PriceList []string }
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, page.PriceList...)
	}
	ps, err := ParseProducts(entries)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// TestTableMatchesG0PriceList: every price in the embedded table equals the
// Price List captured in G0 (#3), unit included.
func TestTableMatchesG0PriceList(t *testing.T) {
	diffs, err := table(t).Drift(g0Products(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diffs {
		t.Errorf("drift: %s", d)
	}
}

func TestDriftDetects(t *testing.T) {
	tb := table(t)
	ps := g0Products(t)
	var changed []Product
	for _, p := range ps {
		switch p.UsageType {
		case "USW1-Task-Shot-Rigetti-Cepheus-1-108Q":
			p.USD = "0.0005000000"
		case "USE1-Completed-Task-ExecutionDuration-SV1":
			p.USD = "0.0900000000"
		case "EUN1-Task-IQM-Garnet":
			continue // delisted
		}
		changed = append(changed, p)
	}
	diffs, err := tb.Drift(changed)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range diffs {
		got[d.UsageType] = d.PriceList
	}
	want := map[string]string{
		"USW1-Task-Shot-Rigetti-Cepheus-1-108Q":     "$0.0005 per Quantum-Shot",
		"USE1-Completed-Task-ExecutionDuration-SV1": "$0.09 per minutes",
		"EUN1-Task-IQM-Garnet":                      "missing",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("diffs = %v, want %v", got, want)
	}
	if _, err := tb.Drift([]Product{{UsageType: "USW1-Task-Rigetti-Cepheus-1-108Q", Unit: "Quantum-Task", USD: "abc"}}); err == nil {
		t.Error("an unparseable Price List price was accepted")
	}
}

// TestQPUArithmeticIsExact is A-price's QPU half (#10): exactly per-task +
// shots x per-shot, for every QPU in the table.
func TestQPUArithmeticIsExact(t *testing.T) {
	tb := table(t)
	for _, d := range tb.Devices {
		if d.Simulator() {
			continue
		}
		for _, shots := range []int64{d.ShotsRange[0], min(100, d.ShotsRange[1]), d.ShotsRange[1]} {
			e, err := tb.Estimate(d, Request{Programs: 1, Shots: shots})
			if err != nil {
				t.Fatal(err)
			}
			if want := tb.perTask + Nano(shots)*d.perShot; e.Total != want {
				t.Errorf("%s %d shots: %s, want %s", d.Alias, shots, e.Total, want)
			}
		}
	}
}

func TestEstimates(t *testing.T) {
	tb := table(t)
	cases := []struct {
		name, dev  string
		req        Request
		total      string
		billed     int
		arithmetic string
	}{
		{"G0 Cepheus Bell", "cepheus", Request{Programs: 1, Shots: 100}, "$0.3425", 1, "$0.30 + 100 x $0.000425 = $0.3425"},
		{"G1 GHZ-3", "cepheus", Request{Programs: 1, Shots: 1000}, "$0.725", 1, ""},
		{"A-budget task", "cepheus", Request{Programs: 1, Shots: 5000}, "$2.425", 1, ""},
		{"G0 program set", "cepheus", Request{Programs: 2, Shots: 100, Pack: 2}, "$0.385", 1, "1 program sets x $0.30 + 2 programs x 100 shots x $0.000425 = $0.385"},
		{"VQE unpacked", "cepheus", Request{Programs: 500, Shots: 1000}, "$362.50", 500, "500 tasks x ($0.30 + 1000 x $0.000425) = $362.50"},
		{"VQE packed by 5", "cepheus", Request{Programs: 500, Shots: 1000, Pack: 5}, "$242.50", 100, ""},
		{"uneven packing", "cepheus", Request{Programs: 7, Shots: 10, Pack: 3}, "$0.92975", 3, ""},
		{"Forte 1000", "forte", Request{Programs: 1, Shots: 1000}, "$80.30", 1, ""},
		{"Aquila", "aquila", Request{Programs: 1, Shots: 100}, "$1.30", 1, ""},
		{"SV1 minimum, duration unknown", "sv1", Request{Programs: 1, Shots: 100}, "$0.00375", 1, ""},
		{"SV1 G0: 2 ms billed as 3 s", "sv1", Request{Programs: 1, Shots: 100, Duration: 2 * time.Millisecond}, "$0.00375", 1, "2ms billed as the 3 s minimum x $0.075/min = $0.00375"},
		{"SV1 10 s", "sv1", Request{Programs: 1, Shots: 100, Duration: 10 * time.Second}, "$0.0125", 1, "10s x $0.075/min = $0.0125"},
		{"SV1 rounds up to the ms", "sv1", Request{Programs: 1, Duration: 3*time.Second + 1}, "$0.00375125", 1, ""},
		{"DM1 4 tasks", "dm1", Request{Programs: 4, Duration: time.Minute}, "$0.30", 4, "4 tasks x 1m0s x $0.075/min = $0.30"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := tb.Estimate(dev(t, tb, tc.dev), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if e.TotalUSD != tc.total || e.BilledTasks != tc.billed || e.Kind != "estimate" || e.PricesAsOf != "2026-10" {
				t.Errorf("got %s, %d tasks, kind %q, as of %q; want %s, %d", e.TotalUSD, e.BilledTasks, e.Kind, e.PricesAsOf, tc.total, tc.billed)
			}
			if tc.arithmetic != "" && e.Arithmetic != tc.arithmetic {
				t.Errorf("arithmetic %q\n          want %q", e.Arithmetic, tc.arithmetic)
			}
		})
	}
	e, _ := tb.Estimate(dev(t, tb, "sv1"), Request{Programs: 1})
	if !e.Minimum || !strings.Contains(e.Arithmetic, "at least") {
		t.Errorf("unknown duration must be labelled a minimum: %+v", e)
	}
}

func TestEstimateRefuses(t *testing.T) {
	tb := table(t)
	cases := []struct {
		dev  string
		req  Request
		want string
	}{
		{"cepheus", Request{Programs: 0, Shots: 10}, "at least one program"},
		{"cepheus", Request{Programs: 1, Shots: 9}, "10 to 50000 shots"},
		{"cepheus", Request{Programs: 1, Shots: 50001}, "10 to 50000 shots"},
		{"forte", Request{Programs: 1, Shots: 99}, "100 to 5000 shots"},
		{"sv1", Request{Programs: 1, Shots: -1}, "0 to 100000 shots"},
		{"cepheus", Request{Programs: 1, Shots: 10, Duration: time.Second}, "only to simulators"},
		{"sv1", Request{Programs: 2, Pack: 2}, "does not run program sets"},
		{"aquila", Request{Programs: 2, Shots: 1, Pack: 2}, "does not run program sets"},
		{"cepheus", Request{Programs: 200, Shots: 10, Pack: 101}, "limit of 100"},
		{"cepheus", Request{Programs: 2, Shots: 10, Pack: -1}, "negative"},
		{"ibex", Request{Programs: 20, Shots: 1000, Pack: 20}, "at most 10000"},
		{"cepheus", Request{Programs: 100, Shots: 50000, Pack: 100}, "at most 200000"},
		{"sv1", Request{Programs: 1, Duration: -time.Second}, "negative"},
		{"sv1", Request{Programs: 1, Duration: 31 * 24 * time.Hour}, "30 days"},
		// Overflow is an error, never a wrong figure (review of #36).
		{"forte", Request{Programs: 1_000_000, Shots: 5000}, ""}, // $400,300,000: large, representable
		{"sv1", Request{Programs: math.MaxInt64 / 1000, Duration: 29 * 24 * time.Hour}, "too large"},
		{"cepheus", Request{Programs: math.MaxInt64 / 10, Shots: 50000}, "too large"},
	}
	for _, tc := range cases {
		e, err := tb.Estimate(dev(t, tb, tc.dev), tc.req)
		if tc.want == "" {
			// Large but representable: must be exact and positive.
			if err != nil || e.TotalUSD != "$400300000.00" {
				t.Errorf("%s %+v: %v, %v; want an exact positive estimate", tc.dev, tc.req, e.TotalUSD, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %+v: err = %v, want %q", tc.dev, tc.req, err, tc.want)
		}
	}
	// The reviewer's failing case: a Forte estimate printed as negative.
	if _, err := tb.Estimate(dev(t, tb, "forte"), Request{Programs: 1, Shots: 200_000_000_000}); err == nil {
		t.Error("200e9 Forte shots accepted")
	}
}

// TestSimulatorRoundsPerTask: each task is rounded up on its own, then
// multiplied, so N tasks are never under-estimated.
func TestSimulatorRoundsPerTask(t *testing.T) {
	tb := table(t)
	one, _ := tb.Estimate(dev(t, tb, "sv1"), Request{Programs: 1, Duration: 3*time.Second + time.Millisecond/2})
	seven, _ := tb.Estimate(dev(t, tb, "sv1"), Request{Programs: 7, Duration: 3*time.Second + time.Millisecond/2})
	if seven.Total != 7*one.Total {
		t.Errorf("7 tasks = %s, want 7 x %s", seven.Total, one.Total)
	}
}

func TestLookup(t *testing.T) {
	tb := table(t)
	for _, s := range []string{"cepheus", "CEPHEUS", "Rigetti Cepheus-1-108Q", "arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q"} {
		if d, err := tb.Lookup(s); err != nil || d.Alias != "cepheus" {
			t.Errorf("Lookup(%q) = %v, %v", s, d.Alias, err)
		}
	}
	_, err := tb.Lookup("ankaa")
	if !errors.Is(err, ErrUnknownDevice) || !strings.Contains(err.Error(), "never a guess") || !strings.Contains(err.Error(), "cepheus") {
		t.Errorf("unknown device: %v", err)
	}
	if got := tb.Aliases(); len(got) != 9 || got[0] != "aquila" {
		t.Errorf("Aliases() = %v", got)
	}
}

func TestParseUSD(t *testing.T) {
	ok := map[string]Nano{"0.30": 300_000_000, "0.000425": 425_000, "0.0004250000": 425_000, "7000": 7_000_000_000_000, "0.075": 75_000_000, "1": 1e9}
	for s, want := range ok {
		if got, err := ParseUSD(s); err != nil || got != want {
			t.Errorf("ParseUSD(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
	for _, s := range []string{"", "-1", "+1", "abc", "0.0000000001", "1.2.3", "0.x", "99999999999", "1e3", "1x", "1 2", "0x10", " 1", "1.", ".5", "1.-5", "1.000000000000000000001"} {
		if _, err := ParseUSD(s); err == nil {
			t.Errorf("ParseUSD(%q) accepted", s)
		}
	}
}

func TestNanoString(t *testing.T) {
	cases := map[Nano]string{300_000_000: "$0.30", 342_500_000: "$0.3425", 3_750_000: "$0.00375", 0: "$0.00", 362_500_000_000: "$362.50", -300_000_000: "-$0.30", 1: "$0.000000001"}
	for n, want := range cases {
		if got := n.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", n, got, want)
		}
	}
	if Nano(342_500_000).USD() != 0.3425 {
		t.Error("USD()")
	}
}

func TestStaleness(t *testing.T) {
	tb := table(t)
	if w := tb.StaleWarning(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)); w != "" {
		t.Errorf("30 days old warned: %s", w)
	}
	late := time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	if w := tb.StaleWarning(late); !tb.Stale(late) || !strings.Contains(w, "75 days old") {
		t.Errorf("75 days old: %q", w)
	}
}

// mutate returns the embedded table with one change applied to its JSON.
func mutate(t *testing.T, f func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(embedded, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func device(m map[string]any, alias string) map[string]any {
	for _, d := range m["devices"].([]any) {
		if dm := d.(map[string]any); dm["alias"] == alias {
			return dm
		}
	}
	panic("no device " + alias)
}

func TestParseRejectsBadTables(t *testing.T) {
	bad := map[string]func(map[string]any){
		"unknown field":   func(m map[string]any) { m["asof"] = 1 },
		"bad as_of":       func(m map[string]any) { m["as_of"] = "October" },
		"bad per-task":    func(m map[string]any) { m["per_task_usd"] = "x" },
		"duplicate alias": func(m map[string]any) { device(m, "garnet")["alias"] = "cepheus" },
		"both prices":     func(m map[string]any) { device(m, "aquila")["per_minute_usd"] = "1" },
		"neither price":   func(m map[string]any) { delete(device(m, "aquila"), "per_shot_usd") },
		"bad shot price":  func(m map[string]any) { device(m, "aquila")["per_shot_usd"] = "1e-2" },
		"bad minute":      func(m map[string]any) { device(m, "sv1")["per_minute_usd"] = "x" },
		"sim no minimum":  func(m map[string]any) { delete(device(m, "sv1"), "minimum_seconds") },
		"sim no regions":  func(m map[string]any) { device(m, "sv1")["usage"] = map[string]any{} },
		"sim bad region": func(m map[string]any) {
			device(m, "sv1")["usage"] = map[string]any{"minutes": []any{"XXX9-Completed-Task-ExecutionDuration-SV1"}}
		},
		"qpu no usage": func(m map[string]any) { device(m, "aquila")["usage"] = map[string]any{} },
		"usage wrong region": func(m map[string]any) {
			device(m, "garnet")["usage"] = map[string]any{"task": "USW1-Task-IQM-Garnet", "shot": "EUN1-Task-Shot-IQM-Garnet"}
		},
		"no shots range": func(m map[string]any) { delete(device(m, "cepheus"), "shots_range") },
		"inverted range": func(m map[string]any) { device(m, "cepheus")["shots_range"] = []any{100, 10} },
	}
	for name, f := range bad {
		if _, err := Parse(mutate(t, f)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte("{")); err == nil {
		t.Error("not JSON: accepted")
	}
	if _, err := Parse(mutate(t, func(map[string]any) {})); err != nil {
		t.Errorf("unchanged table rejected: %v", err)
	}
}

// TestDriftReportsEachSimulatorRegion: one delisted region is a diff (law 3).
func TestDriftReportsEachSimulatorRegion(t *testing.T) {
	var kept []Product
	for _, p := range g0Products(t) {
		if p.UsageType != "USW2-Completed-Task-ExecutionDuration-SV1" {
			kept = append(kept, p)
		}
	}
	diffs, err := table(t).Drift(kept)
	if err != nil || len(diffs) != 1 || diffs[0].UsageType != "USW2-Completed-Task-ExecutionDuration-SV1" || diffs[0].PriceList != "missing" {
		t.Errorf("diffs = %v, %v; want USW2 SV1 missing", diffs, err)
	}
}

type fakeProducts struct {
	pages [][]string
	err   error
}

func (f *fakeProducts) GetProducts(_ context.Context, in *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	if in.ServiceCode == nil || *in.ServiceCode != ServiceCode {
		return nil, errors.New("wrong service code")
	}
	i := 0
	if in.NextToken != nil {
		_, _ = fmt.Sscanf(*in.NextToken, "%d", &i)
	}
	out := &pricing.GetProductsOutput{PriceList: f.pages[i]}
	if i+1 < len(f.pages) {
		tok := fmt.Sprint(i + 1)
		out.NextToken = &tok
	}
	return out, nil
}

func TestFetchProducts(t *testing.T) {
	pages, _ := filepath.Glob("../../testdata/pricing/get-products.*.json")
	sort.Strings(pages)
	f := &fakeProducts{}
	for _, p := range pages {
		b, _ := os.ReadFile(p) //nolint:gosec // fixtures
		var page struct{ PriceList []string }
		_ = json.Unmarshal(b, &page)
		f.pages = append(f.pages, page.PriceList)
	}
	ps, err := FetchProducts(context.Background(), f)
	if err != nil || len(ps) == 0 {
		t.Fatalf("FetchProducts: %d, %v", len(ps), err)
	}
	if diffs, _ := table(t).Drift(ps); len(diffs) != 0 {
		t.Errorf("drift through FetchProducts: %v", diffs)
	}
	if _, err := FetchProducts(context.Background(), &fakeProducts{err: errors.New("throttled")}); err == nil {
		t.Error("API error not returned")
	}
	if _, err := ParseProducts([]string{"{"}); err == nil {
		t.Error("bad entry accepted")
	}
	if d := (Diff{Device: "x", UsageType: "u", Table: "a", PriceList: "b"}).String(); d != "x u: table a, Price List b" {
		t.Errorf("Diff.String() = %q", d)
	}
}

// TestDriftReportsNewSimulatorRegion: a simulator region the Price List has
// and the table doesn't is a diff, not silently ignored.
func TestDriftReportsNewSimulatorRegion(t *testing.T) {
	ps := append(g0Products(t), Product{UsageType: "EUN1-Completed-Task-ExecutionDuration-SV1", Unit: "minutes", USD: "0.0750000000"})
	diffs, err := table(t).Drift(ps)
	if err != nil || len(diffs) != 1 || diffs[0].UsageType != "EUN1-Completed-Task-ExecutionDuration-SV1" || diffs[0].Table != "not in the table" {
		t.Errorf("diffs = %v, %v", diffs, err)
	}
}

// TestSimulatorRegionsMatchG0Devices: the table's SV1/DM1 regions are the
// regions G0 found them ONLINE in (testdata/devices, #2).
func TestSimulatorRegionsMatchG0Devices(t *testing.T) {
	inverse := map[string]string{}
	for pre, r := range usagePrefix {
		inverse[r] = pre
	}
	for _, alias := range []string{"sv1", "dm1"} {
		files, _ := filepath.Glob("../../testdata/devices/*/amazon-braket_" + alias + ".json")
		var want []string
		for _, f := range files {
			region := filepath.Base(filepath.Dir(f))
			want = append(want, inverse[region]+"-Completed-Task-ExecutionDuration-"+strings.ToUpper(alias))
		}
		sort.Strings(want)
		got := append([]string(nil), dev(t, table(t), alias).Usage.Minutes...)
		sort.Strings(got)
		if len(want) == 0 || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s regions: table %v, G0 devices %v", alias, got, want)
		}
	}
}

func TestCheckedArithmetic(t *testing.T) {
	if _, err := mul(math.MaxInt64/2, 3); !errors.Is(err, errOverflow) {
		t.Errorf("mul overflow: %v", err)
	}
	if _, err := mul(-1, 2); err == nil {
		t.Error("mul accepted a negative")
	}
	if v, err := mul(0, math.MaxInt64); err != nil || v != 0 {
		t.Errorf("mul(0, max) = %d, %v", v, err)
	}
	if _, err := add(math.MaxInt64, 1); !errors.Is(err, errOverflow) {
		t.Errorf("add overflow: %v", err)
	}
	// Extreme --tasks with --pack: an overflow error, not a misleading one.
	tb := table(t)
	if _, err := tb.Estimate(dev(t, tb, "garnet"), Request{Programs: math.MaxInt, Shots: 1, Pack: 2}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("extreme packed tasks: %v", err)
	}
	// Fees alone overflow; shots alone overflow; the sum overflows.
	big := Device{Alias: "x", ShotsRange: [2]int64{1, math.MaxInt64}, perShot: 1}
	tb2 := &Table{perTask: Nano(math.MaxInt64 / 2)}
	for name, req := range map[string]Request{
		"fees": {Programs: 3, Shots: 1},
		"sum":  {Programs: 1, Shots: math.MaxInt64/2 + 2},
	} {
		if _, err := tb2.qpuOnly(big, req); !errors.Is(err, errOverflow) {
			t.Errorf("%s: %v", name, err)
		}
	}
	shotsHeavy := Device{Alias: "y", ShotsRange: [2]int64{1, math.MaxInt64}, perShot: Nano(math.MaxInt64 / 2)}
	if _, err := (&Table{perTask: 1}).qpuOnly(shotsHeavy, Request{Programs: 1, Shots: 3}); !errors.Is(err, errOverflow) {
		t.Errorf("shots: %v", err)
	}
}

// qpuOnly runs the QPU path on a hand-built device and table.
func (t *Table) qpuOnly(d Device, req Request) (Estimate, error) {
	e := Estimate{Programs: req.Programs, Shots: req.Shots}
	var err error
	if e.TotalShots, err = mul(int64(req.Programs), req.Shots); err != nil {
		return e, err
	}
	return e, t.qpu(d, req, &e)
}
