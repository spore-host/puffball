package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
)

// FailingCreators returns a Creator factory whose clients fail the test on
// any billable call. It is A-gate's fake (#8): a command run without --real
// must never reach it.
func FailingCreators(t testing.TB) func(context.Context, string) (pbraket.Creator, error) {
	return func(_ context.Context, region string) (pbraket.Creator, error) {
		return failingCreator{t: t, region: region}, nil
	}
}

type failingCreator struct {
	t      testing.TB
	region string
}

func (f failingCreator) CreateQuantumTask(context.Context, *braket.CreateQuantumTaskInput, ...func(*braket.Options)) (*braket.CreateQuantumTaskOutput, error) {
	f.t.Errorf("billable call reached: CreateQuantumTask in %s (law 1)", f.region)
	return nil, errors.New("billable call in a test")
}

// RecordingCreator records CreateQuantumTask calls. It mirrors the service's
// ClientToken behaviour, measured in #35: a repeated token with the same input
// returns the first ARN; with different input, a 409 ConflictException.
// LoseResponse > 0 makes that many calls create the task but return an
// error, as a dropped connection would.
type RecordingCreator struct {
	mu           sync.Mutex
	Calls        []braket.CreateQuantumTaskInput
	byToken      map[string]string
	inputs       map[string]string
	LoseResponse int
	Err          error // returned without creating anything
}

// Factory returns a Creator factory; each region's client mints ARNs in its
// own region.
func (c *RecordingCreator) Factory() func(context.Context, string) (pbraket.Creator, error) {
	return func(_ context.Context, region string) (pbraket.Creator, error) { return regionCreator{c, region}, nil }
}

type regionCreator struct {
	c      *RecordingCreator
	region string
}

func (r regionCreator) CreateQuantumTask(ctx context.Context, in *braket.CreateQuantumTaskInput, opts ...func(*braket.Options)) (*braket.CreateQuantumTaskOutput, error) {
	return r.c.create(r.region, in)
}

// Created returns how many distinct tasks were created.
func (c *RecordingCreator) Created() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byToken)
}

// CreateQuantumTask records the call and returns a task ARN per client token,
// in us-west-1; use Factory for other regions.
func (c *RecordingCreator) CreateQuantumTask(_ context.Context, in *braket.CreateQuantumTaskInput, _ ...func(*braket.Options)) (*braket.CreateQuantumTaskOutput, error) {
	return c.create("us-west-1", in)
}

func (c *RecordingCreator) create(region string, in *braket.CreateQuantumTaskInput) (*braket.CreateQuantumTaskOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls = append(c.Calls, *in)
	if c.Err != nil {
		return nil, c.Err
	}
	// The SDK checks required members before sending.
	for name, missing := range map[string]bool{
		"Action": in.Action == nil, "ClientToken": in.ClientToken == nil, "DeviceArn": in.DeviceArn == nil,
		"OutputS3Bucket": in.OutputS3Bucket == nil, "OutputS3KeyPrefix": in.OutputS3KeyPrefix == nil, "Shots": in.Shots == nil,
	} {
		if missing {
			return nil, fmt.Errorf("missing required field, CreateQuantumTaskInput.%s", name)
		}
	}
	tok := aws.ToString(in.ClientToken)
	if c.byToken == nil {
		c.byToken, c.inputs = map[string]string{}, map[string]string{}
	}
	cp := *in
	cp.ClientToken = nil
	b, _ := json.Marshal(cp)
	arn, ok := c.byToken[tok]
	if ok && c.inputs[tok] != string(b) {
		// The type and message the service returned in #35 (HTTP 409).
		return nil, &types.ConflictException{Message: aws.String("Idempotency token reused across different requests")}
	}
	c.inputs[tok] = string(b)
	if !ok {
		arn = fmt.Sprintf("arn:aws:braket:%s:000000000000:quantum-task/fake-%d", region, len(c.byToken)+1)
		c.byToken[tok] = arn
	}
	if c.LoseResponse > 0 {
		c.LoseResponse--
		return nil, errors.New("connection reset by peer")
	}
	return &braket.CreateQuantumTaskOutput{QuantumTaskArn: aws.String(arn)}, nil
}

// FakeCanceller records CancelQuantumTask calls and returns Status, or Err.
type FakeCanceller struct {
	mu     sync.Mutex
	Calls  []braket.CancelQuantumTaskInput
	Status types.CancellationStatus
	Err    error
}

// Factory returns a Cancellers func handing out c for every region.
func (c *FakeCanceller) Factory() func(context.Context, string) (pbraket.Canceller, error) {
	return func(context.Context, string) (pbraket.Canceller, error) { return c, nil }
}

// CancelQuantumTask records the call.
func (c *FakeCanceller) CancelQuantumTask(_ context.Context, in *braket.CancelQuantumTaskInput, _ ...func(*braket.Options)) (*braket.CancelQuantumTaskOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls = append(c.Calls, *in)
	if c.Err != nil {
		return nil, c.Err
	}
	if in.ClientToken == nil || in.QuantumTaskArn == nil {
		return nil, errors.New("missing required field, CancelQuantumTaskInput")
	}
	return &braket.CancelQuantumTaskOutput{QuantumTaskArn: in.QuantumTaskArn, CancellationStatus: c.Status}, nil
}

// FakeLimiter changes a FakeFactory's spending limits as the service does
// (Amazon Braket Developer Guide, spending limits): Create is rejected when the
// device already has a limit; Update is rejected below the current plus
// queued spend; Delete removes by ARN. Calls records each call by name.
type FakeLimiter struct {
	F     *FakeFactory
	Err   error
	mu    sync.Mutex
	Calls []string
	n     int
}

// Factory returns a func handing out l for every region, as the region's
// limiter.
func (l *FakeLimiter) Factory() func(context.Context, string) (pbraket.Limiter, error) {
	return func(_ context.Context, region string) (pbraket.Limiter, error) {
		b, ok := l.F.Regions[region]
		if !ok {
			return nil, fmt.Errorf("no Braket endpoint for region %s", region)
		}
		return &regionLimiter{l: l, b: b, region: region}, nil
	}
}

type regionLimiter struct {
	l      *FakeLimiter
	b      *FakeBraket
	region string
}

func (r *regionLimiter) call(name string) error {
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	r.l.Calls = append(r.l.Calls, name+" "+r.region)
	return r.l.Err
}

// CreateSpendingLimit adds a limit with no spend.
func (r *regionLimiter) CreateSpendingLimit(_ context.Context, in *braket.CreateSpendingLimitInput, _ ...func(*braket.Options)) (*braket.CreateSpendingLimitOutput, error) {
	if err := r.call("CreateSpendingLimit"); err != nil {
		return nil, err
	}
	if in.ClientToken == nil || in.DeviceArn == nil || in.SpendingLimit == nil {
		return nil, errors.New("missing required field, CreateSpendingLimitInput")
	}
	if !strings.Contains(aws.ToString(in.DeviceArn), ":"+r.region+"::device/qpu/") {
		return nil, &types.ValidationException{Message: aws.String("the device is not a QPU in this region")}
	}
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	for _, x := range r.b.Limits {
		if aws.ToString(x.DeviceArn) == aws.ToString(in.DeviceArn) {
			return nil, &types.ConflictException{Message: aws.String("A spending limit already exists for this device")}
		}
	}
	r.l.mu.Lock()
	r.l.n++
	arn := fmt.Sprintf("arn:aws:braket:%s:000000000000:spending-limit/fake-%d", r.region, r.l.n)
	r.l.mu.Unlock()
	r.b.Limits = append(r.b.Limits, types.SpendingLimitSummary{SpendingLimitArn: aws.String(arn), DeviceArn: in.DeviceArn,
		SpendingLimit: in.SpendingLimit, TotalSpend: aws.String("0.00"), QueuedSpend: aws.String("0.00"), TimePeriod: in.TimePeriod, Tags: in.Tags})
	return &braket.CreateSpendingLimitOutput{SpendingLimitArn: aws.String(arn)}, nil
}

// UpdateSpendingLimit changes the maximum and period.
func (r *regionLimiter) UpdateSpendingLimit(_ context.Context, in *braket.UpdateSpendingLimitInput, _ ...func(*braket.Options)) (*braket.UpdateSpendingLimitOutput, error) {
	if err := r.call("UpdateSpendingLimit"); err != nil {
		return nil, err
	}
	if in.ClientToken == nil || in.SpendingLimitArn == nil {
		return nil, errors.New("missing required field, UpdateSpendingLimitInput")
	}
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	for i, x := range r.b.Limits {
		if aws.ToString(x.SpendingLimitArn) != aws.ToString(in.SpendingLimitArn) {
			continue
		}
		if in.SpendingLimit != nil {
			var limit, total, queued float64
			_, _ = fmt.Sscan(aws.ToString(in.SpendingLimit), &limit)
			_, _ = fmt.Sscan(aws.ToString(x.TotalSpend), &total)
			_, _ = fmt.Sscan(aws.ToString(x.QueuedSpend), &queued)
			if total+queued > limit {
				return nil, &types.ValidationException{Message: aws.String("current spend plus queued spend is higher than the requested spending limit")}
			}
			r.b.Limits[i].SpendingLimit = in.SpendingLimit
		}
		if in.TimePeriod != nil {
			r.b.Limits[i].TimePeriod = in.TimePeriod
		}
		return &braket.UpdateSpendingLimitOutput{}, nil
	}
	return nil, &types.ResourceNotFoundException{Message: aws.String("spending limit not found")}
}

// DeleteSpendingLimit removes a limit.
func (r *regionLimiter) DeleteSpendingLimit(_ context.Context, in *braket.DeleteSpendingLimitInput, _ ...func(*braket.Options)) (*braket.DeleteSpendingLimitOutput, error) {
	if err := r.call("DeleteSpendingLimit"); err != nil {
		return nil, err
	}
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	for i, x := range r.b.Limits {
		if aws.ToString(x.SpendingLimitArn) == aws.ToString(in.SpendingLimitArn) {
			r.b.Limits = append(r.b.Limits[:i], r.b.Limits[i+1:]...)
			return &braket.DeleteSpendingLimitOutput{}, nil
		}
	}
	return nil, &types.ResourceNotFoundException{Message: aws.String("spending limit not found")}
}
