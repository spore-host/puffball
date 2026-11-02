package gate

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	"github.com/spore-host/puffball/internal/scrub"
	"github.com/spore-host/puffball/pkg/awscfg"
	"github.com/spore-host/puffball/pkg/braket"
)

// Cancellers returns the client that cancels tasks in a region.
type Cancellers func(ctx context.Context, region string) (braket.Canceller, error)

// AWSCancellers returns cancel clients from the shared spore.host config.
// Cancelling isn't billable (it is not one of law 1's operations), but it
// changes account state, so the client is built here, the only package that
// holds one.
func AWSCancellers() Cancellers {
	return func(ctx context.Context, region string) (braket.Canceller, error) {
		cfg, err := awscfg.Load(ctx, region)
		if err != nil {
			return nil, err
		}
		return braketsdk.NewFromConfig(cfg), nil
	}
}

// Cancel asks the service to cancel a task and returns the status it
// reports. The client token is new for each call; it makes the SDK's own
// retries of that call one request, not repeated puff cancel commands.
func Cancel(ctx context.Context, cs Cancellers, region, arn string) (types.CancellationStatus, error) {
	c, err := cs(ctx, region)
	if err != nil {
		return "", scrub.Error(err)
	}
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	out, err := c.CancelQuantumTask(ctx, &braketsdk.CancelQuantumTaskInput{QuantumTaskArn: aws.String(arn), ClientToken: aws.String(tok)})
	if err != nil {
		return "", scrub.Error(fmt.Errorf("CancelQuantumTask: %w", err))
	}
	return out.CancellationStatus, nil
}
