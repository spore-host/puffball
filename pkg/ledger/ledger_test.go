package ledger_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/ledger"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/results"
	"github.com/spore-host/puffball/pkg/testutil"
)

const (
	sv1ID  = "d1b61d7d-936d-4574-af57-f962beb72375"
	bellID = "617b7e2b-f77f-433b-8226-6cc55eb8d077"
	psID   = "a543ab67-6545-4cf0-8bbe-55f55bef1aa6"
)

// g0Tasks are the three G0 tasks as SearchQuantumTasks returned them.
func g0Tasks(t *testing.T) []pbraket.Task {
	t.Helper()
	var out braket.SearchQuantumTasksOutput
	testutil.LoadJSON(t, "../../testdata/tasks/tags/search-createdAt.json", &out)
	var tasks []pbraket.Task
	for _, s := range out.QuantumTasks {
		tasks = append(tasks, pbraket.Task{Region: "us-west-1", Summary: s})
	}
	return tasks
}

// fakeReader serves the G0 SV1 task's results.json and counts reads.
type fakeReader struct {
	reads  []string
	err    error
	owners []string
}

func (f *fakeReader) Read(_ context.Context, region, bucket, key, account string, _ int64) (results.Object, error) {
	f.reads = append(f.reads, region+" "+bucket+"/"+key)
	f.owners = append(f.owners, account)
	if f.err != nil {
		return results.Object{}, f.err
	}
	if !strings.Contains(key, sv1ID) {
		return results.Object{}, errors.New("NoSuchKey")
	}
	b, err := os.ReadFile("../../testdata/tasks/sv1-bell-100/s3/results.json")
	return results.Object{Body: b, ETag: `"fixture"`}, err
}

func opts(t *testing.T, r *fakeReader, c *ledger.FileCache) ledger.Options {
	t.Helper()
	tb, err := price.Load()
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Options{Prices: tb, Reader: r, Cache: c,
		Account: func(context.Context) (string, error) { return "000000000000", nil }}
}

func byID(l *ledger.Ledger) map[string]ledger.Line {
	m := map[string]ledger.Line{}
	for _, ln := range l.Lines {
		m[ln.TaskID] = ln
	}
	return m
}

// A-price on the G0 tasks: the pricing-page arithmetic for each.
func TestBuildG0(t *testing.T) {
	r := &fakeReader{}
	l, err := ledger.Build(context.Background(), g0Tasks(t), opts(t, r, nil))
	if err != nil {
		t.Fatal(err)
	}
	m := byID(l)
	for id, want := range map[string]string{bellID: "$0.3425", psID: "$0.385", sv1ID: "$0.00375"} {
		ln := m[id]
		if ln.Estimate == nil || ln.Estimate.USD != want || ln.Standing != ledger.Completed {
			t.Errorf("%s: %+v; want %s completed", id, ln, want)
		}
	}
	sv := m[sv1ID]
	if sv.DurationMs == nil || *sv.DurationMs != 2 || sv.DurationSource != ledger.DurationSource || sv.Estimate.Minimum {
		t.Errorf("sv1 duration: %+v", sv)
	}
	if len(r.reads) != 1 || !strings.Contains(r.reads[0], "us-west-1 amazon-braket-us-west-1-000000000000/puffball/g0c-1a1fdca0/"+sv1ID+"/results.json") || r.owners[0] != "000000000000" {
		t.Errorf("reads %v owners %v: want only the SV1 results.json, with the account as expected owner", r.reads, r.owners)
	}
	if m[bellID].Project != "puffball-g0" || m[bellID].Rung != "qpu" || m[sv1ID].Rung != "sv1" {
		t.Errorf("tags: %+v", m[bellID])
	}
	_, total, err := l.Group("project")
	if err != nil {
		t.Fatal(err)
	}
	if total.Completed.USD != "$0.73125" || total.Completed.Tasks != 3 || total.Completed.Shots != 400 || total.Completed.AtLeast {
		t.Errorf("total %+v", total.Completed)
	}
	if l.PricesAsOf == "" {
		t.Error("no price date")
	}
}

func TestStandings(t *testing.T) {
	tasks := g0Tasks(t)
	set := func(id string, st types.QuantumTaskStatus) {
		for i := range tasks {
			if strings.HasSuffix(aws.ToString(tasks[i].Summary.QuantumTaskArn), id) {
				tasks[i].Summary.Status = st
			}
		}
	}
	set(bellID, types.QuantumTaskStatusCancelled)
	set(psID, types.QuantumTaskStatusQueued)
	set(sv1ID, types.QuantumTaskStatusRunning)
	untagged := tasks[0]
	untagged.Summary.QuantumTaskArn = aws.String("arn:aws:braket:us-east-1:000000000000:quantum-task/11111111-1111-4111-8111-111111111111")
	untagged.Summary.DeviceArn = aws.String("arn:aws:braket:::device/quantum-simulator/amazon/tn1")
	untagged.Summary.Status = types.QuantumTaskStatusCompleted
	untagged.Summary.Tags = nil
	untagged.Region = "us-east-1"
	tasks = append(tasks, untagged)

	r := &fakeReader{}
	l, err := ledger.Build(context.Background(), tasks, opts(t, r, nil))
	if err != nil {
		t.Fatal(err)
	}
	m := byID(l)
	if m[bellID].Standing != ledger.NotCharged || m[psID].Standing != ledger.Pending || m[sv1ID].Standing != ledger.Pending {
		t.Errorf("standings %s %s %s", m[bellID].Standing, m[psID].Standing, m[sv1ID].Standing)
	}
	if sv := m[sv1ID]; !sv.Estimate.Minimum || !strings.Contains(sv.Note, "until the task completes") || len(r.reads) != 0 {
		t.Errorf("running SV1: %+v, reads %v", sv, r.reads)
	}
	tn := m["11111111-1111-4111-8111-111111111111"]
	if tn.Standing != ledger.Unpriced || tn.Estimate != nil || tn.Project != ledger.Untagged || !strings.Contains(tn.Note, "never a guess") {
		t.Errorf("tn1: %+v", tn)
	}
	if got := l.Unpriced(); len(got) != 1 || got[0] != "tn1: no price in the table" || tn.Missing != "price" {
		t.Errorf("Unpriced = %v", got)
	}
	groups, total, err := l.Group("project")
	if err != nil {
		t.Fatal(err)
	}
	// Nothing completed and priced; each standing summed on its own.
	if total.Completed.Tasks != 0 || total.NotCharged.USD != "$0.00" || total.NotCharged.Tasks != 1 || total.Pending.USD != "$0.38875" || !total.Pending.AtLeast || total.Unpriced != 1 {
		t.Errorf("total %+v", total)
	}
	if total.Tasks() != 4 || total.Shots() != 600 || total.UnpricedShots != 200 {
		t.Errorf("total counts %d tasks, %d shots", total.Tasks(), total.Shots())
	}
	if b := m[bellID]; b.Estimate.USD != "$0.00" || !strings.Contains(b.Estimate.Arithmetic, "$0: cancelled; "+ledger.ZeroSource) || b.Estimate.Kind != "estimate" || b.Estimate.PricesAsOf == "" {
		t.Errorf("cancelled QPU line: %+v %+v", b, b.Estimate)
	}
	if len(groups) != 2 || groups[0].Key != "puffball-g0" || groups[1].Key != ledger.Untagged {
		t.Errorf("groups %+v", groups)
	}
}

func TestGroupBy(t *testing.T) {
	l, err := ledger.Build(context.Background(), g0Tasks(t), opts(t, &fakeReader{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	for by, keys := range map[string][]string{
		"device":  {"Cepheus-1-108Q", "sv1"},
		"rung":    {"qpu", "sv1"},
		"day":     {"2026-10-24", "2026-10-28"},
		"project": {"puffball-g0"},
	} {
		groups, _, err := l.Group(by)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, g := range groups {
			got = append(got, g.Key)
		}
		if strings.Join(got, ",") != strings.Join(keys, ",") {
			t.Errorf("by %s: %v, want %v", by, got, keys)
		}
	}
	if _, _, err := l.Group("user"); err == nil {
		t.Error("--by user accepted")
	}
}

func TestDurationUnreadable(t *testing.T) {
	r := &fakeReader{err: errors.New("AccessDenied for 123456789012")}
	l, err := ledger.Build(context.Background(), g0Tasks(t), opts(t, r, nil))
	if err != nil {
		t.Fatal(err)
	}
	sv := byID(l)[sv1ID]
	if !sv.Estimate.Minimum || sv.Estimate.USD != "$0.00375" || !strings.Contains(sv.Note, "AccessDenied") || strings.Contains(sv.Note, "123456789012") {
		t.Errorf("sv1: %+v", sv)
	}
	_, total, _ := l.Group("day")
	if !total.Completed.AtLeast {
		t.Error("total with a minimum line is not marked at least")
	}
}

func TestCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger", "durations.jsonl")
	r := &fakeReader{}
	if _, err := ledger.Build(context.Background(), g0Tasks(t), opts(t, r, &ledger.FileCache{Path: path})); err != nil {
		t.Fatal(err)
	}
	// A second build, with a fresh cache object on the same file, reads
	// nothing from S3.
	r2 := &fakeReader{err: errors.New("must not be read")}
	c := &ledger.FileCache{Path: path}
	l, err := ledger.Build(context.Background(), g0Tasks(t), opts(t, r2, c))
	if err != nil || len(r2.reads) != 0 || c.Err != nil {
		t.Fatalf("err %v, reads %v, cache err %v", err, r2.reads, c.Err)
	}
	if sv := byID(l)[sv1ID]; sv.DurationMs == nil || *sv.DurationMs != 2 {
		t.Errorf("cached duration: %+v", sv)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"task":"us-west-1/`+sv1ID+`"`) || strings.Contains(string(b), "000000000000") || strings.Contains(string(b), "arn:") {
		t.Errorf("cache holds more than region/task: %s", b)
	}
	// A line too long for a line scanner doesn't hide the lines after it.
	long := strings.Repeat("x", 2<<20)
	if err := os.WriteFile(path, append([]byte(long+"\n"), b...), 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := &ledger.FileCache{Path: path}
	if ms, ok := c2.Get("us-west-1/" + sv1ID); !ok || ms != 2 || c2.Err != nil {
		t.Errorf("after a long line: %d %v %v", ms, ok, c2.Err)
	}
	// Deleting or corrupting the cache loses nothing (law 4).
	if err := os.WriteFile(path, []byte("not json\n{\"schema\":\"other\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r3 := &fakeReader{}
	l, err = ledger.Build(context.Background(), g0Tasks(t), opts(t, r3, &ledger.FileCache{Path: path}))
	if err != nil || len(r3.reads) != 1 || byID(l)[sv1ID].Estimate.Minimum {
		t.Errorf("corrupt cache: err %v reads %v", err, r3.reads)
	}
	// An unwritable cache is a warning, not a failure.
	bad := &ledger.FileCache{Path: filepath.Join(path, "x", "durations.jsonl")}
	if _, err := ledger.Build(context.Background(), g0Tasks(t), opts(t, &fakeReader{}, bad)); err != nil || bad.Err == nil {
		t.Errorf("unwritable cache: err %v, cache err %v", err, bad.Err)
	}
	var nilCache *ledger.FileCache
	if _, ok := nilCache.Get("x"); ok {
		t.Error("nil cache hit")
	}
	nilCache.Put("x", 1)
}

func TestParseDuration(t *testing.T) {
	for _, tc := range []struct {
		in   string
		ms   int64
		fail bool
	}{
		{`{"additionalMetadata":{"simulatorMetadata":{"executionDuration":2}}}`, 2, false},
		{`{"additionalMetadata":{"simulatorMetadata":{"executionDuration":0}}}`, 0, false},
		{`{"additionalMetadata":{}}`, 0, true},
		{`{"additionalMetadata":{"simulatorMetadata":{"executionDuration":-1}}}`, 0, true},
		{`nope`, 0, true},
	} {
		ms, err := ledger.ParseDuration([]byte(tc.in))
		if (err != nil) != tc.fail || ms != tc.ms {
			t.Errorf("%s: %d %v", tc.in, ms, err)
		}
	}
}

func TestBuildErrors(t *testing.T) {
	if _, err := ledger.Build(context.Background(), nil, ledger.Options{}); err == nil {
		t.Error("no price table accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ledger.Build(ctx, g0Tasks(t), opts(t, &fakeReader{}, nil)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	o := opts(t, &fakeReader{}, nil)
	o.Account = func(context.Context) (string, error) { return "", errors.New("no creds") }
	l, err := ledger.Build(context.Background(), g0Tasks(t), o)
	if err != nil || !strings.Contains(byID(l)[sv1ID].Note, "no creds") {
		t.Errorf("account failure: %v", err)
	}
}

// A QPU task the service reports no shots for gets no figure, never a
// fee-only one (law 6).
func TestNilShots(t *testing.T) {
	tasks := g0Tasks(t)
	for i := range tasks {
		if strings.HasSuffix(aws.ToString(tasks[i].Summary.QuantumTaskArn), bellID) {
			tasks[i].Summary.Shots = nil
		}
	}
	l, err := ledger.Build(context.Background(), tasks, opts(t, &fakeReader{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	ln := byID(l)[bellID]
	if ln.Standing != ledger.Unpriced || ln.Estimate != nil || ln.Missing != "shots" {
		t.Errorf("%+v", ln)
	}
	if got := l.Unpriced(); len(got) != 1 || got[0] != "Cepheus-1-108Q: no shots reported" {
		t.Errorf("Unpriced = %v", got)
	}
}

func TestGroupUnknownDay(t *testing.T) {
	tasks := g0Tasks(t)
	tasks[0].Summary.CreatedAt = nil
	l, err := ledger.Build(context.Background(), tasks, opts(t, &fakeReader{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	groups, _, _ := l.Group("day")
	if groups[len(groups)-1].Key != "unknown" {
		t.Errorf("groups %+v", groups)
	}
}

// A cancelled or failed task is $0 whatever its device, so one on a device
// the table doesn't price isn't unpriced; a cancelling task is still pending.
func TestNotChargedBeforePrice(t *testing.T) {
	tasks := g0Tasks(t)
	tasks[0].Summary.DeviceArn = aws.String("arn:aws:braket:::device/quantum-simulator/amazon/tn1")
	tasks[0].Summary.Status = types.QuantumTaskStatusFailed
	tasks[1].Summary.Status = types.QuantumTaskStatusCancelling
	l, err := ledger.Build(context.Background(), tasks, opts(t, &fakeReader{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	m := byID(l)
	id0 := aws.ToString(tasks[0].Summary.QuantumTaskArn)
	id0 = id0[strings.LastIndex(id0, "/")+1:]
	id1 := aws.ToString(tasks[1].Summary.QuantumTaskArn)
	id1 = id1[strings.LastIndex(id1, "/")+1:]
	if m[id0].Standing != ledger.NotCharged || m[id0].Estimate.USD != "$0.00" || len(l.Unpriced()) != 0 {
		t.Errorf("failed tn1 task: %+v; unpriced %v", m[id0], l.Unpriced())
	}
	if m[id1].Standing != ledger.Pending {
		t.Errorf("cancelling task: %s", m[id1].Standing)
	}
}
