package testutil

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	pbraket "github.com/spore-host/puffball/pkg/braket"
)

func TestDevicesFactoryLoadsG0Fixtures(t *testing.T) {
	f := DevicesFactory(t, "../../testdata/devices")
	if len(f.SSM.Regions) != 5 || f.Seed() != "us-west-2" {
		t.Fatalf("regions %v seed %s", f.SSM.Regions, f.Seed())
	}
	c, err := f.Braket(context.Background(), "us-west-1")
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.GetDevice(context.Background(), &braket.GetDeviceInput{DeviceArn: aws.String("arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q")})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.DeviceQueueInfo) == 0 || aws.ToString(d.DeviceCapabilities) == "" {
		t.Error("GetDevice fixture lost deviceQueueInfo or deviceCapabilities in decoding")
	}
	if _, err := f.Braket(context.Background(), "mars-1"); err == nil {
		t.Error("unknown region did not fail")
	}
	if _, err := c.GetDevice(context.Background(), &braket.GetDeviceInput{DeviceArn: aws.String("nope")}); err == nil {
		t.Error("unknown device did not fail")
	}
	out, err := f.SSM.GetParametersByPath(context.Background(), &ssm.GetParametersByPathInput{Path: aws.String(pbraket.RegionsParameterPath)})
	if err != nil || len(out.Parameters) != 5 {
		t.Fatalf("GetParametersByPath = %v, %v", out, err)
	}
}

func TestFakeBraketBehaviour(t *testing.T) {
	ctx := context.Background()
	b := &FakeBraket{
		Tasks:      []types.QuantumTaskSummary{{QuantumTaskArn: aws.String("arn:t/1"), Status: "QUEUED"}},
		TaskDetail: map[string]*braket.GetQuantumTaskOutput{"arn:t/1": {QuantumTaskArn: aws.String("arn:t/1")}},
	}
	if _, err := b.GetQuantumTask(ctx, &braket.GetQuantumTaskInput{QuantumTaskArn: aws.String("arn:t/1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetQuantumTask(ctx, &braket.GetQuantumTaskInput{QuantumTaskArn: aws.String("arn:t/2")}); err == nil {
		t.Error("unknown task did not fail")
	}
	for name, flt := range map[string]types.SearchQuantumTasksFilter{
		"multi-value status": {Name: aws.String("status"), Operator: types.SearchQuantumTasksFilterOperatorEqual, Values: []string{"QUEUED", "RUNNING"}},
		"tag filter":         {Name: aws.String("tags"), Operator: types.SearchQuantumTasksFilterOperatorEqual, Values: []string{"x"}},
		"bad createdAt":      {Name: aws.String("createdAt"), Operator: types.SearchQuantumTasksFilterOperatorGte, Values: []string{"yesterday"}},
	} {
		if _, err := b.SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{flt}}); !errors.Is(err, ErrInvalidFilters) {
			t.Errorf("%s: err = %v, want ErrInvalidFilters (as the service)", name, err)
		}
	}
	if out, err := (&FakeBraket{}).SearchDevices(ctx, &braket.SearchDevicesInput{Filters: []types.SearchDevicesFilter{}}); err != nil || len(out.Devices) != 0 {
		t.Errorf("empty region: %v %v", out, err)
	}
	b.Err = errors.New("down")
	if _, err := b.SearchDevices(ctx, &braket.SearchDevicesInput{Filters: []types.SearchDevicesFilter{}}); err == nil {
		t.Error("Err not returned")
	}
	if got := len(b.Calls()); got != 6 {
		t.Errorf("Calls() recorded %d calls, want 6", got)
	}
	if _, err := (&FakeSSM{Err: errors.New("x")}).GetParametersByPath(ctx, &ssm.GetParametersByPathInput{}); err == nil {
		t.Error("FakeSSM.Err not returned")
	}
	if _, err := (&FakeFactory{}).Parameters(ctx, "r"); err == nil {
		t.Error("FakeFactory without SSM did not fail")
	}
}

func TestFakeBraketPagingAndStatusFilter(t *testing.T) {
	ctx := context.Background()
	b := &FakeBraket{
		DevicePages: [][]types.DeviceSummary{{{DeviceName: aws.String("a")}}, {{DeviceName: aws.String("b")}}},
		Tasks: []types.QuantumTaskSummary{
			{QuantumTaskArn: aws.String("arn:t/1"), Status: "QUEUED"},
			{QuantumTaskArn: aws.String("arn:t/2"), Status: "COMPLETED"},
		},
	}
	p1, _ := b.SearchDevices(ctx, &braket.SearchDevicesInput{Filters: []types.SearchDevicesFilter{}})
	if p1.NextToken == nil {
		t.Fatal("first of two device pages has no NextToken")
	}
	p2, _ := b.SearchDevices(ctx, &braket.SearchDevicesInput{Filters: []types.SearchDevicesFilter{}, NextToken: p1.NextToken})
	if p2.NextToken != nil || aws.ToString(p2.Devices[0].DeviceName) != "b" {
		t.Fatalf("second page = %+v", p2)
	}
	out, err := b.SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{
		{Name: aws.String("status"), Operator: types.SearchQuantumTasksFilterOperatorEqual, Values: []string{"COMPLETED"}},
	}})
	if err != nil || len(out.QuantumTasks) != 1 || aws.ToString(out.QuantumTasks[0].QuantumTaskArn) != "arn:t/2" {
		t.Fatalf("status filter: %+v %v", out, err)
	}
}

func TestFakeRejectsNilFilters(t *testing.T) {
	ctx := context.Background()
	if _, err := (&FakeBraket{}).SearchDevices(ctx, &braket.SearchDevicesInput{}); !errors.Is(err, ErrMissingFilters) {
		t.Errorf("SearchDevices nil Filters: %v", err)
	}
	if _, err := (&FakeBraket{}).SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{}); !errors.Is(err, ErrMissingFilters) {
		t.Errorf("SearchQuantumTasks nil Filters: %v", err)
	}
}

func TestFakeSSMPaging(t *testing.T) {
	ctx := context.Background()
	s := &FakeSSM{Regions: []string{"a", "b", "c"}, PageSize: 2}
	in := &ssm.GetParametersByPathInput{Path: aws.String(pbraket.RegionsParameterPath)}
	p1, err := s.GetParametersByPath(ctx, in)
	if err != nil || len(p1.Parameters) != 2 || p1.NextToken == nil {
		t.Fatalf("page 1: %+v %v", p1, err)
	}
	in.NextToken = p1.NextToken
	p2, err := s.GetParametersByPath(ctx, in)
	if err != nil || len(p2.Parameters) != 1 || p2.NextToken != nil {
		t.Fatalf("page 2: %+v %v", p2, err)
	}
	other, _ := s.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{Path: aws.String("/elsewhere")})
	if len(other.Parameters) != 0 {
		t.Error("unknown path returned parameters")
	}
	in.NextToken = aws.String("99")
	if _, err := s.GetParametersByPath(ctx, in); err == nil {
		t.Error("bad NextToken accepted")
	}
}

func TestFakeBraketBadInputsDontPanic(t *testing.T) {
	ctx := context.Background()
	b := &FakeBraket{DevicePages: [][]types.DeviceSummary{{}}}
	if _, err := b.SearchDevices(ctx, &braket.SearchDevicesInput{Filters: []types.SearchDevicesFilter{}, NextToken: aws.String("7")}); err == nil {
		t.Error("out-of-range device NextToken accepted")
	}
	if _, err := b.SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{}, NextToken: aws.String("x")}); err == nil {
		t.Error("bad task NextToken accepted")
	}
	if _, err := b.SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{{Name: aws.String("status")}}}); !errors.Is(err, ErrInvalidFilters) {
		t.Errorf("empty filter Values: %v", err)
	}
}

func TestFakeBraketRecordsSearchStatuses(t *testing.T) {
	b := &FakeBraket{}
	ctx := context.Background()
	_, _ = b.SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{}})
	_, _ = b.SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{
		{Name: aws.String("status"), Operator: types.SearchQuantumTasksFilterOperatorEqual, Values: []string{"QUEUED"}},
	}})
	if got := b.Searches(); len(got) != 2 || got[0] != "" || got[1] != "QUEUED" {
		t.Errorf("Searches() = %q", got)
	}
}
