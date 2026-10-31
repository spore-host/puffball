package devices_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/devices"
	"github.com/spore-host/puffball/pkg/testutil"
)

const fixtures = "../../testdata/devices"

func load(t *testing.T, file string) devices.Device {
	t.Helper()
	var g braket.GetDeviceOutput
	testutil.LoadJSON(t, filepath.Join(fixtures, file), &g)
	d, err := devices.Parse(&g, filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ts(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

// TestParseG0Fixtures checks every field against the GetDevice responses
// captured in G0 (#2).
func TestParseG0Fixtures(t *testing.T) {
	cases := []struct {
		file     string
		qubits   int
		paradigm string
		calibAt  *time.Time
		calibSrc string
		programs bool
		firstWin devices.Window
		normalQ  int
	}{
		// Oldest benchmark: the T1/T2 sites (FreeInversionRecovery,
		// FreeInductionDecay) are 40 days older than the rest.
		{"us-west-1/rigetti_cepheus-1-108q.json", 107, "gate", ts("2026-09-14T14:24:06Z"), "provider.specs", true, devices.Window{Day: "Everyday", Start: "00:00:00", End: "01:59:59"}, 0},
		{"eu-north-1/iqm_garnet.json", 20, "gate", nil, "", true, devices.Window{Day: "Weekdays", Start: "00:00:00", End: "01:29:59"}, 0},
		{"eu-north-1/aqt_ibex-q1.json", 12, "gate", ts("2026-10-20T13:05:10Z"), "standardized.updatedAt", true, devices.Window{Day: "Monday", Start: "11:00:00", End: "15:00:00"}, 2},
		{"us-east-1/ionq_forte-1.json", 36, "gate", ts("2026-10-23T00:00:00Z"), "standardized.updatedAt", true, devices.Window{Day: "Sunday", Start: "00:00:00", End: "17:59:59"}, 0},
		{"us-east-1/quera_aquila.json", 256, "analog", nil, "", false, devices.Window{Day: "Friday", Start: "04:00:00", End: "11:59:59"}, 470},
		{"us-west-1/amazon-braket_sv1.json", 34, "gate", nil, "", false, devices.Window{Day: "Everyday", Start: "00:00:00", End: "23:59:59"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			d := load(t, tc.file)
			if d.Qubits != tc.qubits || d.Paradigm != tc.paradigm || (d.ProgramSet != nil) != tc.programs {
				t.Errorf("qubits %d paradigm %s program sets %v; want %d %s %v", d.Qubits, d.Paradigm, d.ProgramSet != nil, tc.qubits, tc.paradigm, tc.programs)
			}
			if d.Windows[0] != tc.firstWin {
				t.Errorf("first window %+v, want %+v", d.Windows[0], tc.firstWin)
			}
			if n, ok := d.QueueSize("Normal"); !ok || n != tc.normalQ {
				t.Errorf("normal queue %d %v, want %d", n, ok, tc.normalQ)
			}
			c := d.Calibration
			switch {
			case tc.calibAt == nil && (c.At != nil || c.Missing == ""):
				t.Errorf("calibration %+v: want none, with the reason named (law 6)", c)
			case tc.calibAt != nil && (c.At == nil || !c.At.Equal(*tc.calibAt) || !strings.HasPrefix(c.Source, tc.calibSrc)):
				t.Errorf("calibration %+v, want %v from %s", c, tc.calibAt, tc.calibSrc)
			}
		})
	}
	cep := load(t, "us-west-1/rigetti_cepheus-1-108q.json")
	if n := cep.Calibration.Newest; n == nil || !n.Equal(*ts("2026-10-24T14:23:23Z")) {
		t.Errorf("Cepheus newest calibration %v", n)
	}
	if ibex := load(t, "eu-north-1/aqt_ibex-q1.json"); ibex.Calibration.Newest != nil {
		t.Error("a single timestamp must not set Newest")
	}
	if !reflect.DeepEqual(cep.NativeGates, []string{"rx", "rz", "cz", "barrier"}) || !reflect.DeepEqual(cep.ShotsRange, []int64{10, 50000}) {
		t.Errorf("Cepheus gates %v shots %v", cep.NativeGates, cep.ShotsRange)
	}
	if ps := cep.ProgramSet; ps.MaxExecutables != 100 || ps.MaxTotalShots != 200000 {
		t.Errorf("Cepheus program set %+v", ps)
	}
	if forte := load(t, "us-east-1/ionq_forte-1.json"); forte.ProgramSet.MinTotalShots != 100 {
		t.Errorf("Forte min total shots %d", forte.ProgramSet.MinTotalShots)
	}
	if n, ok := load(t, "us-west-1/amazon-braket_sv1.json").QueueSize(""); !ok || n != 2 {
		t.Errorf("SV1 jobs queue %d %v", n, ok)
	}
}

// TestParseEveryFixture: every captured device parses.
func TestParseEveryFixture(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(fixtures, "*", "*.json"))
	n := 0
	for _, f := range files {
		if strings.Contains(f, "search-devices") {
			continue
		}
		rel, _ := filepath.Rel(fixtures, f)
		d := load(t, rel)
		if d.Paradigm == "" || d.Qubits == 0 || len(d.Windows) == 0 || len(d.Queues) != 3 {
			t.Errorf("%s: incomplete parse %+v", rel, d)
		}
		n++
	}
	if n < 10 {
		t.Fatalf("parsed %d fixtures; expected every G0 device", n)
	}
}

func TestParseRejectsBadCapabilities(t *testing.T) {
	g := &braket.GetDeviceOutput{DeviceName: aws.String("x"), DeviceCapabilities: aws.String("{")}
	if _, err := devices.Parse(g, "r"); err == nil {
		t.Error("bad JSON accepted")
	}
	g.DeviceCapabilities = aws.String(`{"action":{"braket.ir.openqasm.program_set":"nope"}}`)
	if _, err := devices.Parse(g, "r"); err == nil {
		t.Error("bad program_set accepted")
	}
}

var aliases = devices.Aliases{
	"cepheus": "arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q",
	"sv1":     "arn:aws:braket:::device/quantum-simulator/amazon/sv1",
	"ankaa":   "arn:aws:braket:us-west-1::device/qpu/rigetti/Ankaa-9",
}

func byName(l *devices.List, name string) (devices.Device, bool) {
	for _, d := range l.Devices {
		if d.Name == name {
			return d, true
		}
	}
	return devices.Device{}, false
}

func TestCollectFromG0(t *testing.T) {
	l, err := devices.Collect(context.Background(), testutil.DevicesFactory(t, fixtures), aliases)
	if err != nil {
		t.Fatal(err)
	}
	cep, ok := byName(l, "Cepheus-1-108Q")
	if !ok || cep.Alias != "cepheus" || cep.Qubits != 107 || !reflect.DeepEqual(cep.Regions, []string{"us-west-1"}) {
		t.Errorf("Cepheus %+v", cep)
	}
	sv1, _ := byName(l, "SV1")
	if !reflect.DeepEqual(sv1.Regions, []string{"eu-west-2", "us-east-1", "us-west-1", "us-west-2"}) || sv1.Alias != "sv1" {
		t.Errorf("SV1 regions %v alias %q; want the four regions G0 found it in", sv1.Regions, sv1.Alias)
	}
	tn1, _ := byName(l, "TN1")
	if tn1.Status != "RETIRED" || tn1.ReplacedBy != aliases["sv1"] || len(tn1.Regions) != 3 || tn1.Calibration.Missing == "" {
		t.Errorf("TN1 %+v", tn1)
	}
	// Merged simulators: queues per region, and the headline queue from the
	// first region in sorted order, not whichever goroutine finished first.
	if n, _ := sv1.QueueSize(""); n != 0 || len(sv1.QueuesByRegion) != 4 {
		t.Errorf("SV1 jobs queue %d (want eu-west-2's 0), %d regions of queues", n, len(sv1.QueuesByRegion))
	}
	if q := sv1.QueuesByRegion["us-west-1"]; len(q) != 3 || q[2].Size != "2" {
		t.Errorf("SV1 us-west-1 queues %+v; G0 measured 2 jobs queued there", q)
	}
	if ankaa, _ := byName(l, "Ankaa-3"); ankaa.Status != "RETIRED" || ankaa.ReplacedBy != "" {
		t.Errorf("Ankaa-3 %+v: retired, with no guessed replacement", ankaa)
	}
	if fe, _ := byName(l, "Forte Enterprise 1"); fe.Status != "OFFLINE" {
		t.Errorf("Forte Enterprise %s, want OFFLINE as measured", fe.Status)
	}
	if l.Devices[0].Type != "QPU" || l.Devices[len(l.Devices)-1].Type != "SIMULATOR" {
		t.Error("QPUs are not listed before simulators")
	}
	d, err := l.Resolve("CEPHEUS", aliases)
	if err != nil || d.Name != "Cepheus-1-108Q" {
		t.Errorf("Resolve(CEPHEUS) = %v, %v", d.Name, err)
	}
	if _, err := l.Resolve("Garnet", aliases); err != nil {
		t.Errorf("Resolve by name: %v", err)
	}
	if d, err := l.Resolve("TN1", aliases); err == nil || !strings.Contains(err.Error(), "RETIRED") || !strings.Contains(err.Error(), "sv1") || d.Arn != "" {
		t.Errorf("a retired device resolved as %q, %v; want a zero Device and an error naming its replacement", d.Arn, err)
	}
	if tn1.Missing == nil || cep.Missing != nil {
		t.Errorf("missing[]: TN1 %v (want the unread fields), Cepheus %v (want none)", tn1.Missing, cep.Missing)
	}
	if len(sv1.StatusByRegion) != 4 || sv1.StatusByRegion["us-west-2"] != "ONLINE" {
		t.Errorf("SV1 status by region %v", sv1.StatusByRegion)
	}
	if _, err := l.Resolve("ankaa", aliases); err == nil || !strings.Contains(err.Error(), "not found in any region") {
		t.Errorf("a known alias missing from the service: %v", err)
	}
	if _, err := l.Resolve("nope", aliases); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown device: %v", err)
	}
}

func TestCollectPartialFailures(t *testing.T) {
	cases := map[string]func(*testutil.FakeFactory){
		"region search fails": func(f *testutil.FakeFactory) { f.Regions["eu-north-1"].Err = errors.New("throttled") },
		"GetDevice fails": func(f *testutil.FakeFactory) {
			delete(f.Regions["eu-north-1"].Devices, "arn:aws:braket:eu-north-1::device/qpu/iqm/Garnet")
		},
	}
	// One failed device keeps the rest of its region (but fails the region).
	f0 := testutil.DevicesFactory(t, fixtures)
	delete(f0.Regions["eu-north-1"].Devices, "arn:aws:braket:eu-north-1::device/qpu/iqm/Garnet")
	l0, _ := devices.Collect(context.Background(), f0, aliases)
	if _, ok := byName(l0, "Emerald"); !ok {
		t.Error("one failed GetDevice dropped the rest of its region")
	}
	if _, ok := byName(l0, "Garnet"); ok {
		t.Error("the failed device was listed")
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f := testutil.DevicesFactory(t, fixtures)
			mut(f)
			l, err := devices.Collect(context.Background(), f, aliases)
			ie, ok := pbraket.AsIncomplete(err)
			if !ok || l == nil || l.Err != err || !reflect.DeepEqual(ie.Regions(), []string{"eu-north-1"}) {
				t.Fatalf("Collect = %v, %v; want the partial list and an IncompleteError naming eu-north-1", l, err)
			}
			if _, ok := byName(l, "Cepheus-1-108Q"); !ok {
				t.Error("devices from regions that answered are missing")
			}
		})
	}
	// The client for a region that answered SearchDevices fails later: a
	// factory that hands out search but not GetDevice can't be built here,
	// so a region whose fake errors after its search pages stands in.
	f1 := testutil.DevicesFactory(t, fixtures)
	f1.Regions["us-east-1"].Err, f1.Regions["us-east-1"].FailAfter = errors.New("reset"), 1
	if _, err := devices.Collect(context.Background(), f1, aliases); err == nil || !strings.Contains(err.Error(), "us-east-1") {
		t.Errorf("GetDevice failure after a good search: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l2, err := devices.Collect(ctx, testutil.DevicesFactory(t, fixtures), aliases)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v, want context.Canceled", err)
	}
	if _, ok := pbraket.AsIncomplete(err); !ok || l2 == nil {
		t.Errorf("cancelled: %v, %v; want a list and an IncompleteError, never an empty success", l2, err)
	}
	f := testutil.DevicesFactory(t, fixtures)
	f.SSM.Err = errors.New("denied")
	if l, err := devices.Collect(context.Background(), f, aliases); l != nil || err == nil {
		t.Error("an SSM failure must return no list")
	}
	f = testutil.DevicesFactory(t, fixtures)
	f.Regions["us-west-1"].Devices["arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q"].DeviceCapabilities = aws.String("{")
	if _, err := devices.Collect(context.Background(), f, aliases); err == nil {
		t.Error("unparseable capabilities not reported")
	}
}

func at(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func TestWindowAt(t *testing.T) {
	cep := load(t, "us-west-1/rigetti_cepheus-1-108q.json")
	garnet := load(t, "eu-north-1/iqm_garnet.json")
	ibex := load(t, "eu-north-1/aqt_ibex-q1.json")
	cases := []struct {
		name string
		d    devices.Device
		now  string
		open bool
		next string
	}{
		{"Cepheus in a window", cep, "2026-10-28T16:56:00Z", true, ""},
		{"Cepheus last second", cep, "2026-10-28T23:59:59Z", true, ""},
		{"Cepheus gap", cep, "2026-10-28T15:00:00Z", false, "2026-10-28T16:00:00Z"},
		{"Cepheus gap after midnight", cep, "2026-10-29T02:30:00Z", false, "2026-10-29T04:00:00Z"},
		{"Garnet Saturday", garnet, "2026-10-31T12:00:00Z", false, "2026-11-02T00:00:00Z"},
		{"Garnet Monday", garnet, "2026-11-02T10:00:00Z", true, ""},
		{"IBEX Thursday", ibex, "2026-10-29T09:00:00Z", false, "2026-10-30T08:00:00Z"},
	}
	for _, tc := range cases {
		s := tc.d.WindowAt(at(tc.now))
		if !s.Known || s.Open != tc.open || (tc.next != "" && !s.Next.Equal(at(tc.next))) {
			t.Errorf("%s: %+v, want open=%v next=%s", tc.name, s, tc.open, tc.next)
		}
	}
	weekend := devices.Device{Windows: []devices.Window{{Day: "Weekend", Start: "09:00", End: "19:00"}}}
	if s := weekend.WindowAt(at("2026-10-31T10:00:00Z")); !s.Known || !s.Open {
		t.Errorf("Weekend (schema value) with HH:MM times on a Saturday: %+v", s)
	}
	if s := weekend.WindowAt(at("2026-11-02T10:00:00Z")); !s.Next.Equal(at("2026-11-07T09:00:00Z")) {
		t.Errorf("Weekend from Monday: %+v", s)
	}
	odd := devices.Device{Windows: []devices.Window{{Day: "Holidays", Start: "00:00:00", End: "23:59:59"}}}
	if s := odd.WindowAt(time.Now()); s.Known {
		t.Error("an unknown executionDay was interpreted")
	}
	wrap := devices.Device{Windows: []devices.Window{{Day: "Everyday", Start: "22:00:00", End: "02:00:00"}}}
	if s := wrap.WindowAt(time.Now()); s.Known {
		t.Error("a window wrapping past midnight (never measured) was interpreted")
	}
	bad := devices.Device{Windows: []devices.Window{{Day: "Everyday", Start: "noon", End: "23:59:59"}}}
	if s := bad.WindowAt(time.Now()); s.Known {
		t.Error("an unreadable window time was interpreted")
	}
	if s := (devices.Device{}).WindowAt(time.Now()); s.Known {
		t.Error("no windows reported as known")
	}
	never := devices.Device{Windows: []devices.Window{{Day: "Everyday", Start: "00:00:00", End: "00:00:00"}}}
	if s := never.WindowAt(at("2026-10-28T00:00:00Z")); !s.Open {
		t.Errorf("a one-second window at its second: %+v", s)
	}
}
