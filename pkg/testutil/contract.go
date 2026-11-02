package testutil

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
)

// TasksFactory is DevicesFactory plus the tasks G0 recorded in us-west-1
// (testdata/tasks): their search summaries and GetQuantumTask responses.
// root is the path to testdata.
func TasksFactory(t testing.TB, root string) *FakeFactory {
	t.Helper()
	f := DevicesFactory(t, filepath.Join(root, "devices"))
	var search braket.SearchQuantumTasksOutput
	LoadJSON(t, filepath.Join(root, "tasks", "tags", "search-createdAt.json"), &search)
	r := f.Regions["us-west-1"]
	r.Tasks = search.QuantumTasks
	r.TaskDetail = map[string]*braket.GetQuantumTaskOutput{}
	files, _ := filepath.Glob(filepath.Join(root, "tasks", "*", "get.final.json"))
	for _, file := range files {
		var g braket.GetQuantumTaskOutput
		LoadJSON(t, file, &g)
		r.TaskDetail[aws.ToString(g.QuantumTaskArn)] = &g
	}
	if len(r.Tasks) == 0 || len(r.TaskDetail) == 0 {
		t.Fatalf("no G0 tasks loaded from %s", root)
	}
	return f
}

// ContractOp is a multi-region operation under [AssertIncompleteContract]. It
// returns the region of every item it got back (one entry per device, task,
// ...), and its error.
type ContractOp func(ctx context.Context, f *FakeFactory) (itemRegions []string, err error)

// AssertIncompleteContract is #47: every multi-region operation must report
// a region it couldn't cover as an *IncompleteError returned as its error,
// never a shorter result that looks complete, and must still return what the
// other regions gave.
//
// Against fresh fixtures from mk it runs op: untouched (a baseline that must
// succeed); with region failing outright and failing part-way (after its
// first page) in each of calls (the fake's operation names, such as
// "SearchDevices"; none means every operation); and with the context already
// cancelled. A case whose failure never fires (the region has too little data
// to fail part-way) fails the test, so the contract can't pass vacuously.
func AssertIncompleteContract(t *testing.T, region string, mk func(t *testing.T) *FakeFactory, op ContractOp, calls ...string) {
	t.Helper()
	base, err := op(context.Background(), mk(t))
	if err != nil {
		t.Fatalf("baseline: err = %v; want success with untouched fixtures", err)
	}
	want := map[string]bool{} // healthy regions that returned items
	for _, r := range base {
		if r != region {
			want[r] = true
		}
	}
	if len(calls) == 0 {
		calls = []string{""}
	}
	broken := errors.New("injected failure")
	for _, call := range calls {
		cases := []struct {
			name string
			mut  func(*FakeBraket)
		}{
			{"fails", func(b *FakeBraket) { b.Err = broken }},
			{"fails part-way", func(b *FakeBraket) {
				// One item per page, so the failure lands mid-listing.
				var pages [][]types.DeviceSummary
				for _, p := range b.DevicePages {
					for _, d := range p {
						pages = append(pages, []types.DeviceSummary{d})
					}
				}
				b.DevicePages, b.TaskPage = pages, 1
				b.Err, b.FailAfter = broken, 1
			}},
		}
		for _, tc := range cases {
			name := tc.name
			if call != "" {
				name = call + " " + name
			}
			t.Run(name, func(t *testing.T) {
				f := mk(t)
				b, ok := f.Regions[region]
				if !ok {
					t.Fatalf("no fixture region %s", region)
				}
				tc.mut(b)
				b.FailOn = call
				got, err := op(context.Background(), f)
				if !b.Injected {
					t.Fatalf("the injected failure never fired in %s (%q): the fixtures are too small for this case", region, call)
				}
				ie, ok := pbraket.AsIncomplete(err)
				if !ok {
					t.Fatalf("err = %v; want an *IncompleteError naming %s (a partial result must not look complete)", err, region)
				}
				if !slices.Contains(ie.Regions(), region) {
					t.Errorf("IncompleteError names %v, not %s", ie.Regions(), region)
				}
				have := map[string]bool{}
				for _, r := range got {
					have[r] = true
				}
				for r := range want {
					if !have[r] {
						t.Errorf("%s's items were dropped when %s failed; the rest must still be returned", r, region)
					}
				}
			})
		}
	}
	t.Run("context cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := op(ctx, mk(t)); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v; want it to wrap context.Canceled", err)
		}
	})
}
