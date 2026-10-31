package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runPuff runs the root command with args and returns stdout, stderr and the
// exit code, with flags reset before the run.
func runPuff(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	ensureI18nInitialized()
	if c, _, err := rootCmd.Find(args); err == nil {
		resetFlags(c)
	}
	var out, errb bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errb)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })
	code := run(rootCmd, args, &errb)
	return out.String(), errb.String(), code
}

func fixedNow(t *testing.T, at time.Time) {
	t.Helper()
	orig := priceNow
	priceNow = func() time.Time { return at }
	t.Cleanup(func() { priceNow = orig })
}

func TestPriceHuman(t *testing.T) {
	fixedNow(t, time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC))
	out, errs, code := runPuff(t, "price", "cepheus", "--shots", "100")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"Rigetti Cepheus-1-108Q (cepheus)", "estimate (prices as of 2026-10): $0.30 + 100 x $0.000425 = $0.3425", "not a bill"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if errs != "" {
		t.Errorf("unexpected stderr: %q", errs)
	}
}

// TestPriceJSONGolden checks the --json shape against testdata/golden.
func TestPriceJSONGolden(t *testing.T) {
	fixedNow(t, time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC))
	cases := map[string][]string{
		"price_cepheus_vqe_packed.json": {"price", "cepheus", "--shots", "1000", "--tasks", "500", "--pack", "5", "--json"},
		"price_sv1_minimum.json":        {"price", "sv1", "--json"},
	}
	for golden, args := range cases {
		out, errs, code := runPuff(t, args...)
		if code != exitOK {
			t.Fatalf("%v: exit %d: %s", args, code, errs)
		}
		path := filepath.Join("..", "testdata", "golden", golden)
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path) //nolint:gosec // golden files
		if err != nil {
			t.Fatalf("%v (run with UPDATE_GOLDEN=1 to create)", err)
		}
		if out != string(want) {
			t.Errorf("%v:\n got %s\nwant %s", args, out, want)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(out), &v); err != nil || v["kind"] != "estimate" {
			t.Errorf("%v: not JSON with kind=estimate: %v", args, err)
		}
	}
}

func TestPriceStaleWarning(t *testing.T) {
	fixedNow(t, time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC))
	_, errs, code := runPuff(t, "price", "cepheus", "--shots", "100")
	if code != exitOK || !strings.Contains(errs, "warning: the price table is 106 days old") {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
	out, _, _ := runPuff(t, "price", "cepheus", "--shots", "100", "--json")
	if !strings.Contains(out, "106 days old") {
		t.Errorf("--json lacks the warning: %s", out)
	}
}

func TestPriceUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"unknown device":    {"price", "ankaa", "--shots", "100"},
		"no device":         {"price"},
		"no shots for QPU":  {"price", "cepheus"},
		"pack on simulator": {"price", "sv1", "--tasks", "2", "--pack", "2"},
		"duration on QPU":   {"price", "cepheus", "--shots", "1", "--duration", "1s"},
		"pack over limit":   {"price", "cepheus", "--shots", "1", "--tasks", "200", "--pack", "101"},
	}
	for name, args := range cases {
		if _, errs, code := runPuff(t, args...); code != exitUsage {
			t.Errorf("%s: exit %d, want 2 (%s)", name, code, errs)
		}
	}
	if _, errs, _ := runPuff(t, "price", "ankaa", "--shots", "100"); !strings.Contains(errs, "never a guess") {
		t.Errorf("unknown-device error does not say why: %s", errs)
	}
}
