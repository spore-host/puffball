package gate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	"github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/testutil"
)

const cancelARN = "arn:aws:braket:us-west-1:123456789012:quantum-task/617b7e2b-f77f-433b-8226-6cc55eb8d077"

func TestCancel(t *testing.T) {
	fc := &testutil.FakeCanceller{Status: types.CancellationStatusCancelling}
	st, err := gate.Cancel(context.Background(), fc.Factory(), "us-west-1", cancelARN)
	if err != nil || st != types.CancellationStatusCancelling {
		t.Fatalf("status %q, err %v", st, err)
	}
	if len(fc.Calls) != 1 || aws.ToString(fc.Calls[0].QuantumTaskArn) != cancelARN || aws.ToString(fc.Calls[0].ClientToken) == "" {
		t.Errorf("calls %+v", fc.Calls)
	}
	// A second cancel is a new request, with a new token.
	_, _ = gate.Cancel(context.Background(), fc.Factory(), "us-west-1", cancelARN)
	if aws.ToString(fc.Calls[0].ClientToken) == aws.ToString(fc.Calls[1].ClientToken) {
		t.Error("two cancels shared a client token")
	}
}

func TestCancelErrors(t *testing.T) {
	sentinel := errors.New("AccessDeniedException: not allowed for arn:aws:iam::123456789012:user/alice")
	cases := []struct {
		name string
		cs   gate.Cancellers
	}{
		{"client", func(context.Context, string) (braket.Canceller, error) { return nil, sentinel }},
		{"call", (&testutil.FakeCanceller{Err: sentinel}).Factory()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := gate.Cancel(context.Background(), tc.cs, "us-west-1", cancelARN)
			if !errors.Is(err, sentinel) {
				t.Fatalf("err = %v; want it to wrap the cause", err)
			}
			if strings.Contains(err.Error(), "123456789012") || strings.Contains(err.Error(), "alice") {
				t.Errorf("error not scrubbed: %v", err)
			}
		})
	}
}
