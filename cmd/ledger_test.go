package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/spore-host/puffball/pkg/results"
	"github.com/spore-host/puffball/pkg/testutil"
)

// g0SV1 is the G0 SV1 task, whose results.json says it ran 2 ms.
const g0SV1 = "d1b61d7d-936d-4574-af57-f962beb72375"

type fixtureReader struct {
	reads int
	err   error
}

func (r *fixtureReader) Read(_ context.Context, _, bucket, key, account string, _ int64) ([]byte, error) {
	r.reads++
	if r.err != nil {
		return nil, r.err
	}
	if !strings.Contains(bucket, account) || !strings.Contains(key, g0SV1) {
		return nil, errors.New("NoSuchKey")
	}
	return os.ReadFile("../testdata/tasks/sv1-bell-100/s3/results.json")
}

func ledgerHarness(t *testing.T, mut func(*testutil.FakeFactory)) *fixtureReader {
	t.Helper()
	tasksHarness(t, mut)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	r := &fixtureReader{}
	orig := newReader
	t.Cleanup(func() { newReader = orig })
	newReader = func() results.Reader { return r }
	return r
}

func TestLedger(t *testing.T) {
	r := ledgerHarness(t, nil)
	out, errs, code := runPuff(t, "ledger", "--since", "0", "--tasks")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"ESTIMATE by project, all tasks (prices as of 2026-10). An estimate, not a bill.",
		"puffball-g0", "TOTAL", "$0.73125",
		"$0.30 + 100 x $0.000425 = $0.3425",
		"$0.30 + 200 x $0.000425 = $0.385",
		"2ms billed as the 3 s minimum",
		"doesn't say whether cancelled or failed tasks are billed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ledger lacks %q:\n%s", want, out)
		}
	}
	noAccount(t, out, errs)
	if r.reads != 1 {
		t.Errorf("%d S3 reads, want 1 (the SV1 task)", r.reads)
	}
	// The second run reads the duration from the cache.
	if _, errs, code := runPuff(t, "ledger", "--since", "0"); code != exitOK || r.reads != 1 {
		t.Errorf("exit %d, %d reads: %s", code, r.reads, errs)
	}
	// --no-cache reads again.
	if _, _, code := runPuff(t, "ledger", "--since", "0", "--no-cache"); code != exitOK || r.reads != 2 {
		t.Errorf("--no-cache: exit %d, %d reads", code, r.reads)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "puffball", "ledger", "durations.jsonl")); err != nil {
		t.Errorf("cache file: %v", err)
	}
}

func TestLedgerSinceDefault(t *testing.T) {
	ledgerHarness(t, nil)
	// 30 days before 2026-10-31 includes every G0 task.
	out, errs, code := runPuff(t, "ledger", "--by", "device")
	if code != exitOK || !strings.Contains(out, "tasks created since 2026-10-01T00:00:00Z") || !strings.Contains(out, "Cepheus-1-108Q") {
		t.Errorf("exit %d:\n%s\n%s", code, out, errs)
	}
}

func TestLedgerJSON(t *testing.T) {
	ledgerHarness(t, nil)
	out, errs, code := runPuff(t, "ledger", "--since", "0", "--by", "rung", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var got struct {
		Kind       string `json:"kind"`
		PricesAsOf string `json:"prices_as_of"`
		Groups     []struct {
			Key       string `json:"key"`
			Completed struct {
				USD string `json:"estimate_usd"`
			} `json:"completed"`
		} `json:"groups"`
		Total struct {
			Completed struct {
				USD   string `json:"estimate_usd"`
				Tasks int    `json:"tasks"`
			} `json:"completed"`
		} `json:"total"`
		Tasks    []json.RawMessage `json:"tasks"`
		Complete bool              `json:"complete"`
		Notes    []string          `json:"notes"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "estimate" || got.PricesAsOf != "2026-10" || got.Total.Completed.USD != "$0.73125" || got.Total.Completed.Tasks != 3 ||
		len(got.Groups) != 2 || got.Groups[0].Key != "qpu" || got.Groups[0].Completed.USD != "$0.7275" || len(got.Tasks) != 3 || !got.Complete || len(got.Notes) == 0 {
		t.Errorf("got %+v", got)
	}
	noAccount(t, out)
}

func TestLedgerRegionDown(t *testing.T) {
	ledgerHarness(t, func(f *testutil.FakeFactory) { f.Regions["eu-north-1"].Err = errors.New("injected failure") })
	out, errs, code := runPuff(t, "ledger", "--since", "0")
	if code != exitError || !strings.Contains(errs, "eu-north-1") || !strings.Contains(out, "$0.73125") ||
		!strings.Contains(out, "INCOMPLETE: eu-north-1 couldn't be searched") || !strings.Contains(out, "TOTAL (incomplete)") {
		t.Errorf("a failed region must be named on stdout and stderr, the rest shown, exit 1: exit %d\n%s\n%s", code, out, errs)
	}
	ledgerHarness(t, func(f *testutil.FakeFactory) { f.Regions["eu-north-1"].Err = errors.New("injected failure") })
	out, _, code = runPuff(t, "ledger", "--since", "0", "--json")
	var got struct {
		Complete      bool              `json:"complete"`
		RegionsFailed map[string]string `json:"regions_failed"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != exitError || got.Complete || got.RegionsFailed["eu-north-1"] == "" {
		t.Errorf("JSON: exit %d, %+v, %v", code, got, err)
	}
}

// A failed region and an unpriced task are both reported.
func TestLedgerRegionDownAndUnpriced(t *testing.T) {
	ledgerHarness(t, func(f *testutil.FakeFactory) {
		f.Regions["eu-north-1"].Err = errors.New("injected failure")
		f.Regions["us-west-1"].Tasks[0].DeviceArn = aws.String("arn:aws:braket:::device/quantum-simulator/amazon/tn1")
	})
	_, errs, code := runPuff(t, "ledger", "--since", "0")
	if code != exitError || !strings.Contains(errs, "eu-north-1") || !strings.Contains(errs, "tn1: no price in the table") {
		t.Errorf("exit %d: %s", code, errs)
	}
}

// A program set's JSON line carries the total shots and the arithmetic, not
// per-program fields the summary can't supply.
func TestLedgerProgramSetJSON(t *testing.T) {
	ledgerHarness(t, nil)
	out, errs, code := runPuff(t, "ledger", "--since", "0", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var got struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	for _, tk := range got.Tasks {
		if tk["task_id"] != "a543ab67-6545-4cf0-8bbe-55f55bef1aa6" {
			continue
		}
		est := tk["estimate"].(map[string]any)
		if tk["shots"] != float64(200) || est["estimate_usd"] != "$0.385" || est["kind"] != "estimate" || est["prices_as_of"] != "2026-10" {
			t.Errorf("program set line %v", tk)
		}
		for _, k := range []string{"programs", "shots_per_program"} {
			if _, ok := est[k]; ok {
				t.Errorf("estimate carries %s", k)
			}
		}
		return
	}
	t.Error("program set not in the ledger")
}

func TestLedgerUnreadableDuration(t *testing.T) {
	r := ledgerHarness(t, nil)
	r.err = errors.New("AccessDenied for " + fakeAccount)
	out, errs, code := runPuff(t, "ledger", "--since", "0", "--tasks")
	if code != exitOK || !strings.Contains(out, "at least $0.73125") || !strings.Contains(out, "billed duration not read: AccessDenied for <account>") {
		t.Errorf("exit %d:\n%s\n%s", code, out, errs)
	}
	noAccount(t, out, errs)
}

func TestLedgerUsage(t *testing.T) {
	for _, args := range [][]string{{"ledger", "--by", "user"}, {"ledger", "--since", "-1h"}, {"ledger", "x"}} {
		ledgerHarness(t, nil)
		if _, errs, code := runPuff(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d: %s", args, code, errs)
		}
	}
}

// The ledger keeps #47's contract through listTasks (TestLsIncompleteContract);
// here, a region failing part-way still exits 1 with the rest priced.
func TestLedgerPartWay(t *testing.T) {
	ledgerHarness(t, func(f *testutil.FakeFactory) {
		b := f.Regions["us-west-1"]
		b.TaskPage, b.Err, b.FailAfter, b.FailOn = 1, errors.New("injected failure"), 1, "SearchQuantumTasks"
	})
	out, errs, code := runPuff(t, "ledger", "--since", "0")
	if code != exitError || !strings.Contains(errs, "us-west-1") || !strings.Contains(out, "INCOMPLETE: us-west-1") {
		t.Errorf("exit %d: %s", code, errs)
	}
}

// A task on a device the price table doesn't price is named, gets no figure,
// and the command exits 1 (law 2).
func TestLedgerUnpriced(t *testing.T) {
	ledgerHarness(t, func(f *testutil.FakeFactory) {
		b := f.Regions["us-west-1"]
		b.Tasks[0].DeviceArn = aws.String("arn:aws:braket:::device/quantum-simulator/amazon/tn1")
	})
	out, errs, code := runPuff(t, "ledger", "--since", "0", "--by", "device")
	row := strings.Fields(lineWith(out, "tn1"))
	if code != exitError || !strings.Contains(errs, "tn1: no price in the table") || len(row) != 7 || row[3] != "-" || row[6] != "1" {
		t.Errorf("exit %d:\n%s\n%s", code, out, errs)
	}
}
