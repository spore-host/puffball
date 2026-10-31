// Package testutil holds fakes for the AWS interfaces puff uses, driven by the
// scrubbed G0 fixtures under testdata/.
package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
)

// ErrMissingFilters mirrors the SDK's client-side validation: Filters is a
// required member of SearchDevicesInput and SearchQuantumTasksInput.
var ErrMissingFilters = errors.New("missing required field, Filters")

// ErrInvalidFilters mirrors the service's response to a multi-value EQUAL
// status filter (measured in G0, #4).
var ErrInvalidFilters = errors.New("ValidationException: Invalid search filters supplied")

// FakeBraket is an in-memory Braket region implementing [pbraket.API].
// Lists are served one page per element; FailAfter > 0 returns Err after that
// many pages, to simulate an interrupted pagination.
type FakeBraket struct {
	DevicePages [][]types.DeviceSummary
	Devices     map[string]*braket.GetDeviceOutput // by device ARN
	Tasks       []types.QuantumTaskSummary
	TaskDetail  map[string]*braket.GetQuantumTaskOutput // by task ARN
	TaskPage    int                                     // page size for task search; 0 = all in one page

	Err       error // returned by every call (FailAfter == 0) or after FailAfter pages
	FailAfter int

	mu       sync.Mutex
	pages    int
	calls    []string
	searches []string
}

var _ pbraket.API = (*FakeBraket)(nil)

func (f *FakeBraket) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.Err == nil {
		return nil
	}
	if f.FailAfter == 0 || f.pages >= f.FailAfter {
		return f.Err
	}
	f.pages++
	return nil
}

// Searches returns the status value of each SearchQuantumTasks call, in
// order ("" for a search with no status filter).
func (f *FakeBraket) Searches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.searches...)
}

// Calls returns the operations made against this region, in order.
func (f *FakeBraket) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// pageIndex parses a NextToken; a token this fake didn't issue is an error,
// as it is from the service.
func pageIndex(tok *string, pages int) (int, error) {
	if tok == nil {
		return 0, nil
	}
	n, err := strconv.Atoi(*tok)
	if err != nil || n < 0 || n >= pages {
		return 0, fmt.Errorf("ValidationException: invalid NextToken %q", *tok)
	}
	return n, nil
}

func nextToken(i, n int) *string {
	if i+1 >= n {
		return nil
	}
	return aws.String(strconv.Itoa(i + 1))
}

// SearchDevices serves DevicePages.
func (f *FakeBraket) SearchDevices(_ context.Context, in *braket.SearchDevicesInput, _ ...func(*braket.Options)) (*braket.SearchDevicesOutput, error) {
	if in.Filters == nil {
		return nil, ErrMissingFilters
	}
	if err := f.record("SearchDevices"); err != nil {
		return nil, err
	}
	if len(f.DevicePages) == 0 {
		return &braket.SearchDevicesOutput{}, nil
	}
	i, err := pageIndex(in.NextToken, len(f.DevicePages))
	if err != nil {
		return nil, err
	}
	return &braket.SearchDevicesOutput{Devices: f.DevicePages[i], NextToken: nextToken(i, len(f.DevicePages))}, nil
}

// GetDevice serves Devices.
func (f *FakeBraket) GetDevice(_ context.Context, in *braket.GetDeviceInput, _ ...func(*braket.Options)) (*braket.GetDeviceOutput, error) {
	if err := f.record("GetDevice"); err != nil {
		return nil, err
	}
	if d, ok := f.Devices[aws.ToString(in.DeviceArn)]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("ResourceNotFoundException: device %s", aws.ToString(in.DeviceArn))
}

// SearchQuantumTasks applies status (EQUAL, one value only) and createdAt
// (GTE) filters to Tasks, as the service does.
func (f *FakeBraket) SearchQuantumTasks(_ context.Context, in *braket.SearchQuantumTasksInput, _ ...func(*braket.Options)) (*braket.SearchQuantumTasksOutput, error) {
	if in.Filters == nil {
		return nil, ErrMissingFilters
	}
	if err := f.record("SearchQuantumTasks"); err != nil {
		return nil, err
	}
	status := ""
	for _, flt := range in.Filters {
		if aws.ToString(flt.Name) == "status" && len(flt.Values) > 0 {
			status = flt.Values[0]
		}
	}
	f.mu.Lock()
	f.searches = append(f.searches, status)
	f.mu.Unlock()
	match := append([]types.QuantumTaskSummary(nil), f.Tasks...)
	for _, flt := range in.Filters {
		if len(flt.Values) == 0 {
			return nil, ErrInvalidFilters
		}
		switch aws.ToString(flt.Name) {
		case "status":
			if flt.Operator != types.SearchQuantumTasksFilterOperatorEqual || len(flt.Values) != 1 {
				return nil, ErrInvalidFilters
			}
			match = keep(match, func(t types.QuantumTaskSummary) bool { return string(t.Status) == flt.Values[0] })
		case "createdAt":
			since, err := time.Parse(time.RFC3339, flt.Values[0])
			if err != nil || flt.Operator != types.SearchQuantumTasksFilterOperatorGte {
				return nil, ErrInvalidFilters
			}
			match = keep(match, func(t types.QuantumTaskSummary) bool { return !aws.ToTime(t.CreatedAt).Before(since) })
		default:
			return nil, ErrInvalidFilters // the service rejects tag filters too (#4)
		}
	}
	size := f.TaskPage
	if size <= 0 {
		size = len(match) + 1
	}
	pages := (len(match) + size - 1) / size
	if pages == 0 {
		pages = 1
	}
	i, err := pageIndex(in.NextToken, pages)
	if err != nil {
		return nil, err
	}
	start, end := i*size, (i+1)*size
	if start > len(match) {
		start = len(match)
	}
	if end > len(match) {
		end = len(match)
	}
	var tok *string
	if end < len(match) {
		tok = aws.String(strconv.Itoa(i + 1))
	}
	return &braket.SearchQuantumTasksOutput{QuantumTasks: match[start:end], NextToken: tok}, nil
}

// GetQuantumTask serves TaskDetail.
func (f *FakeBraket) GetQuantumTask(_ context.Context, in *braket.GetQuantumTaskInput, _ ...func(*braket.Options)) (*braket.GetQuantumTaskOutput, error) {
	if err := f.record("GetQuantumTask"); err != nil {
		return nil, err
	}
	if t, ok := f.TaskDetail[aws.ToString(in.QuantumTaskArn)]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("ResourceNotFoundException: task %s", aws.ToString(in.QuantumTaskArn))
}

func keep(ts []types.QuantumTaskSummary, ok func(types.QuantumTaskSummary) bool) []types.QuantumTaskSummary {
	var out []types.QuantumTaskSummary
	for _, t := range ts {
		if ok(t) {
			out = append(out, t)
		}
	}
	return out
}

// FakeSSM serves the Braket region list under [pbraket.RegionsParameterPath],
// PageSize parameters per page (default 10, the service's maximum). Any other
// path is empty, as an unknown path is in SSM. FailAfter > 0 returns Err after
// that many pages.
type FakeSSM struct {
	Regions   []string
	Err       error
	FailAfter int
	PageSize  int

	mu    sync.Mutex
	pages int
}

// GetParametersByPath pages Regions.
func (s *FakeSSM) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	s.mu.Lock()
	fail := s.Err != nil && (s.FailAfter == 0 || s.pages >= s.FailAfter)
	s.pages++
	s.mu.Unlock()
	if fail {
		return nil, s.Err
	}
	if aws.ToString(in.Path) != pbraket.RegionsParameterPath {
		return &ssm.GetParametersByPathOutput{}, nil
	}
	size := s.PageSize
	if size <= 0 {
		size = 10
	}
	pages := (len(s.Regions) + size - 1) / size
	if pages == 0 {
		pages = 1
	}
	i, err := pageIndex(in.NextToken, pages)
	if err != nil {
		return nil, err
	}
	out := &ssm.GetParametersByPathOutput{NextToken: nextToken(i, pages)}
	for _, r := range s.Regions[min(i*size, len(s.Regions)):min((i+1)*size, len(s.Regions))] {
		out.Parameters = append(out.Parameters, ssmtypes.Parameter{Name: aws.String(pbraket.RegionsParameterPath + "/" + r), Value: aws.String(r)})
	}
	return out, nil
}

// FakeFactory implements [pbraket.Factory] over per-region fakes. A region
// with no entry fails like an unreachable endpoint.
type FakeFactory struct {
	SeedRegion string
	Regions    map[string]*FakeBraket
	SSM        *FakeSSM
}

var _ pbraket.Factory = (*FakeFactory)(nil)

// Seed returns SeedRegion.
func (f *FakeFactory) Seed() string { return f.SeedRegion }

// Braket returns the region's fake.
func (f *FakeFactory) Braket(_ context.Context, region string) (pbraket.API, error) {
	if b, ok := f.Regions[region]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("no Braket endpoint for region %s", region)
}

// Parameters returns the SSM fake.
func (f *FakeFactory) Parameters(context.Context, string) (pbraket.ParameterReader, error) {
	if f.SSM == nil {
		return nil, errors.New("no SSM fake")
	}
	return f.SSM, nil
}

// LoadJSON decodes a fixture into v, failing the test on error. SDK output
// types have no JSON tags, but encoding/json matches field names
// case-insensitively, so raw service responses decode into them directly.
func LoadJSON(t testing.TB, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test fixtures
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode fixture %s: %v", path, err)
	}
}

// DevicesFactory builds a FakeFactory from a G0 devices fixture directory
// (testdata/devices): the SSM region list, every region's SearchDevices pages
// and GetDevice responses. The seed is us-west-2, as in G0.
func DevicesFactory(t testing.TB, dir string) *FakeFactory {
	t.Helper()
	// SSM's JSON protocol sends timestamps as epoch numbers, which the SDK
	// type can't take from encoding/json; only the values are needed.
	var prm struct{ Parameters []struct{ Value *string } }
	LoadJSON(t, filepath.Join(dir, "ssm-braket-regions.json"), &prm)
	f := &FakeFactory{SeedRegion: "us-west-2", Regions: map[string]*FakeBraket{}, SSM: &FakeSSM{}}
	for _, p := range prm.Parameters {
		region := aws.ToString(p.Value)
		f.SSM.Regions = append(f.SSM.Regions, region)
		fb := &FakeBraket{Devices: map[string]*braket.GetDeviceOutput{}}
		pages, err := filepath.Glob(filepath.Join(dir, region, "search-devices.*.json"))
		if err != nil || len(pages) == 0 {
			t.Fatalf("no SearchDevices pages for %s in %s", region, dir)
		}
		sort.Strings(pages)
		for _, pg := range pages {
			var out braket.SearchDevicesOutput
			LoadJSON(t, pg, &out)
			fb.DevicePages = append(fb.DevicePages, out.Devices)
		}
		files, _ := filepath.Glob(filepath.Join(dir, region, "*.json"))
		for _, file := range files {
			if strings.HasPrefix(filepath.Base(file), "search-devices.") {
				continue
			}
			var d braket.GetDeviceOutput
			LoadJSON(t, file, &d)
			fb.Devices[aws.ToString(d.DeviceArn)] = &d
		}
		f.Regions[region] = fb
	}
	return f
}
