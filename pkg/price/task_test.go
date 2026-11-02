package price_test

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/puffball/pkg/price"
)

func TestTask(t *testing.T) {
	tb, err := price.Load()
	if err != nil {
		t.Fatal(err)
	}
	cep, _ := tb.Lookup("cepheus")
	sv1, _ := tb.Lookup("sv1")
	cases := []struct {
		name     string
		d        price.Device
		shots    int64
		dur      time.Duration
		known    bool
		total    string
		minimum  bool
		arithSub string
	}{
		// G0 Bell task, 100 shots: the pricing-page arithmetic.
		{"qpu", cep, 100, 0, false, "$0.3425", false, "$0.30 + 100 x $0.000425"},
		// G0 program set, 2 x 100 shots: one per-task fee, 200 shots.
		{"program set total", cep, 200, 0, false, "$0.385", false, "$0.30 + 200 x"},
		// Beyond the per-task shots range: the service took it, so price it.
		{"qpu beyond range", cep, 200_000, 0, false, "$85.30", false, ""},
		// G0 SV1 task: executionDuration 2 ms, billed as the 3 s minimum.
		{"sv1 measured 2ms", sv1, 100, 2 * time.Millisecond, true, "$0.00375", false, "billed as the 3 s minimum"},
		{"sv1 under 1ms", sv1, 100, 0, true, "$0.00375", false, "billed as the 3 s minimum"},
		{"sv1 long", sv1, 100, 90 * time.Second, true, "$0.1125", false, "1m30s"},
		{"sv1 unknown", sv1, 100, 0, false, "$0.00375", true, "at least"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := tb.Task(tc.d, tc.shots, tc.dur, tc.known)
			if err != nil {
				t.Fatal(err)
			}
			if e.TotalUSD != tc.total || e.Minimum != tc.minimum || e.Kind != price.Kind || e.PricesAsOf != tb.AsOf {
				t.Errorf("got %s minimum=%v kind=%s asof=%s; want %s minimum=%v", e.TotalUSD, e.Minimum, e.Kind, e.PricesAsOf, tc.total, tc.minimum)
			}
			if !strings.Contains(e.Arithmetic, tc.arithSub) {
				t.Errorf("arithmetic %q lacks %q", e.Arithmetic, tc.arithSub)
			}
		})
	}
	if _, err := tb.Task(cep, -1, 0, false); err == nil {
		t.Error("negative shots priced")
	}
}
