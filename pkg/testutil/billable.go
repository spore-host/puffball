package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
