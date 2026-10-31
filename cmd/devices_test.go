package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/devices"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/testutil"
)

// g0Factory points newFactory at the G0 device fixtures; mut can break them.
func g0Factory(t *testing.T, mut func(*testutil.FakeFactory)) {
	t.Helper()
	orig := newFactory
	t.Cleanup(func() { newFactory = orig })
	newFactory = func(context.Context) (pbraket.Factory, error) {
		f := testutil.DevicesFactory(t, "../testdata/devices")
		if mut != nil {
			mut(f)
		}
		return f, nil
	}
}

// g0Time is shortly after the G0 capture, inside Cepheus's 16:00 window.
var g0Time = time.Date(2026, 10, 24, 17, 30, 0, 0, time.UTC)

func TestDevicesTable(t *testing.T) {
	g0Factory(t, nil)
	fixedNow(t, g0Time)
	out, errs, code := runPuff(t, "devices")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"us-west-1",
		"ONLINE",
		"107",
		"$0.725",
		"$0.075/min",
		"ESTIMATE: one task of 1000 shots",
		"prices as of 2026-10. An estimate, not a bill.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "RETIRED") {
		t.Error("retired devices shown without --all")
	}
	cep := lineWith(out, "Cepheus-1-108Q")
	// Calibration spans the newest (3h) to the oldest (T1/T2, 40d) benchmark.
	for _, want := range []string{"cepheus ", "open", "3h-40d ago"} {
		if !strings.Contains(cep, want) {
			t.Errorf("Cepheus row lacks %q: %s", want, cep)
		}
	}
	if garnet := lineWith(out, "Garnet"); !strings.Contains(garnet, "none") {
		t.Errorf("Garnet's missing calibration not shown as none: %s", garnet)
	}
	if aquila := lineWith(out, "Aquila"); !strings.Contains(aquila, "470") || !strings.Contains(aquila, "analog") {
		t.Errorf("Aquila row: %s", aquila)
	}
}

func lineWith(out, s string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	return ""
}

func TestDevicesAllAndParadigm(t *testing.T) {
	g0Factory(t, nil)
	fixedNow(t, g0Time)
	out, _, _ := runPuff(t, "devices", "--all")
	if tn1 := lineWith(out, "TN1"); !strings.Contains(tn1, "RETIRED") || !strings.Contains(tn1, "replaced by sv1") {
		t.Errorf("TN1 with --all: %q", tn1)
	}
	if a := lineWith(out, "Ankaa-3"); !strings.Contains(a, "no known replacement") {
		t.Errorf("Ankaa-3: %q", a)
	}
	out, _, _ = runPuff(t, "devices", "--paradigm", "analog")
	if !strings.Contains(out, "Aquila") || strings.Contains(out, "Cepheus") {
		t.Errorf("--paradigm analog:\n%s", out)
	}
	if _, _, code := runPuff(t, "devices", "--paradigm", "photonic"); code != exitUsage {
		t.Errorf("bad --paradigm exit %d", code)
	}
}

func TestDevicesJSONGolden(t *testing.T) {
	g0Factory(t, nil)
	fixedNow(t, g0Time)
	out, errs, code := runPuff(t, "devices", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	path := filepath.Join("..", "testdata", "golden", "devices.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // golden file
	if err != nil {
		t.Fatalf("%v (UPDATE_GOLDEN=1 to create)", err)
	}
	if out != string(want) {
		t.Error("devices --json differs from testdata/golden/devices.json; rerun with UPDATE_GOLDEN=1 if intended")
	}
	var v devicesJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil || !v.Complete || len(v.RegionsFailed) != 0 {
		t.Errorf("JSON: complete=%v failed=%v err=%v", v.Complete, v.RegionsFailed, err)
	}
}

// TestDevicesRegionFailure: the list is printed, the failed region is named,
// and the command exits 1 (law 3).
func TestDevicesRegionFailure(t *testing.T) {
	g0Factory(t, func(f *testutil.FakeFactory) {
		f.Regions["eu-north-1"].Err = errors.New("AccessDenied for arn:aws:iam::123456789012:user/alice")
	})
	fixedNow(t, g0Time)
	out, errs, code := runPuff(t, "devices")
	if code != exitError || !strings.Contains(errs, "eu-north-1") || !strings.Contains(out, "Cepheus") {
		t.Fatalf("exit %d, stderr %q, stdout has Cepheus: %v", code, errs, strings.Contains(out, "Cepheus"))
	}
	if strings.Contains(errs, "123456789012") || strings.Contains(errs, "alice") {
		t.Errorf("stderr leaks an account or principal: %s", errs)
	}
	out, _, code = runPuff(t, "devices", "--json")
	var v devicesJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != exitError || v.Complete || v.RegionsFailed["eu-north-1"] == "" {
		t.Errorf("JSON on failure: code %d complete=%v failed=%v err=%v", code, v.Complete, v.RegionsFailed, err)
	}
	if strings.Contains(out, "123456789012") {
		t.Error("JSON leaks an account ID")
	}
}

func TestDevicesFactoryErrors(t *testing.T) {
	orig := newFactory
	t.Cleanup(func() { newFactory = orig })
	newFactory = func(context.Context) (pbraket.Factory, error) { return nil, errors.New("no region") }
	if _, errs, code := runPuff(t, "devices"); code != exitError || !strings.Contains(errs, "no region") {
		t.Errorf("factory error: %d %s", code, errs)
	}
	g0Factory(t, func(f *testutil.FakeFactory) { f.SSM.Err = errors.New("ssm down") })
	if _, errs, code := runPuff(t, "devices"); code != exitError || !strings.Contains(errs, "SSM") {
		t.Errorf("SSM failure: %d %s", code, errs)
	}
}

func TestDevicesAllJSONGolden(t *testing.T) {
	g0Factory(t, nil)
	fixedNow(t, g0Time)
	out, errs, code := runPuff(t, "devices", "--all", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	path := filepath.Join("..", "testdata", "golden", "devices_all.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // golden file
	if err != nil || out != string(want) {
		t.Fatalf("devices --all --json differs from %s (UPDATE_GOLDEN=1 if intended): %v", path, err)
	}
	var v devicesJSON
	_ = json.Unmarshal([]byte(out), &v)
	for _, d := range v.Devices {
		switch {
		case d.Status == "RETIRED" && (d.Qubits != 0 || d.Calibration.Missing == ""):
			t.Errorf("retired %s: %+v", d.Name, d.Device)
		case d.Status != "RETIRED" && d.Type == "SIMULATOR" && d.PerMinuteUSD != "$0.075":
			t.Errorf("simulator %s has no per-minute rate in JSON", d.Name)
		case d.Status != "RETIRED" && d.Type == "QPU" && d.Estimate == nil && d.EstimateError == "":
			t.Errorf("QPU %s has neither an estimate nor a reason", d.Name)
		}
	}
	if strings.Contains(out, `"qubits": 0`) {
		t.Error("retired devices print qubits 0")
	}
}

func TestCostOfUnpriced(t *testing.T) {
	tb, _ := price.Load()
	c := costOf(tb, devices.Device{Arn: "arn:aws:braket:us-west-1::device/qpu/rigetti/Ankaa-3"})
	if c.cell != "unpriced" || !strings.Contains(c.err, "never a guess") {
		t.Errorf("unpriced: %+v", c)
	}
}

func TestWindowCell(t *testing.T) {
	cep := devices.Device{Windows: []devices.Window{{Day: "Everyday", Start: "16:00:00", End: "23:59:59"}}}
	mon := devices.Device{Windows: []devices.Window{{Day: "Monday", Start: "08:00:00", End: "09:00:00"}}}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) // a Saturday
	cases := map[string]string{
		windowCell(cep, now):                  "opens 16:00Z",
		windowCell(mon, now):                  "opens Mon 08:00Z",
		windowCell(devices.Device{}, now):     "?",
		windowCell(cep, now.Add(5*time.Hour)): "open",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("windowCell = %q, want %q", got, want)
		}
	}
}

func TestCells(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Minute: "30m ago", 3 * time.Hour: "3h ago", 72 * time.Hour: "3d ago"} {
		if got := age(d); got != want {
			t.Errorf("age(%v) = %q", d, got)
		}
	}
	if shortName("arn:aws:braket:::device/quantum-simulator/amazon/sv1") != "sv1" {
		t.Error("shortName")
	}
}
