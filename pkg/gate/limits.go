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

// Limiters returns the client that changes spending limits in a region.
type Limiters func(ctx context.Context, region string) (braket.Limiter, error)

// AWSLimiters returns spending-limit clients from the shared spore.host
// config. Spending limits aren't billable (law 1's operations are task and
// job creation and Cost Explorer requests), but they change account state,
// so the client is built here, as for cancels.
func AWSLimiters() Limiters {
	return func(ctx context.Context, region string) (braket.Limiter, error) {
		cfg, err := awscfg.Load(ctx, region)
		if err != nil {
			return nil, err
		}
		return braketsdk.NewFromConfig(cfg), nil
	}
}

// Limit is a spending limit to create or update: USD is a decimal dollar
// amount; Period, if set, is when the limit is in force (outside it, Braket
// rejects every task on the device).
type Limit struct {
	DeviceArn string
	USD       string
	Period    *types.TimePeriod
	Tags      map[string]string
}

// CreateLimit creates a spending limit in region and returns its ARN.
func CreateLimit(ctx context.Context, ls Limiters, region string, l Limit) (string, error) {
	c, err := ls(ctx, region)
	if err != nil {
		return "", scrub.Error(err)
	}
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	out, err := c.CreateSpendingLimit(ctx, &braketsdk.CreateSpendingLimitInput{ClientToken: aws.String(tok), DeviceArn: aws.String(l.DeviceArn),
		SpendingLimit: aws.String(l.USD), TimePeriod: l.Period, Tags: l.Tags})
	if err != nil {
		return "", scrub.Error(fmt.Errorf("CreateSpendingLimit: %w", err))
	}
	return aws.ToString(out.SpendingLimitArn), nil
}

// UpdateLimit sets an existing limit's maximum and, if given, its period.
// Braket rejects a maximum below the device's current plus queued spend.
func UpdateLimit(ctx context.Context, ls Limiters, region, arn string, l Limit) error {
	c, err := ls(ctx, region)
	if err != nil {
		return scrub.Error(err)
	}
	tok, err := newToken()
	if err != nil {
		return err
	}
	if _, err := c.UpdateSpendingLimit(ctx, &braketsdk.UpdateSpendingLimitInput{ClientToken: aws.String(tok), SpendingLimitArn: aws.String(arn),
		SpendingLimit: aws.String(l.USD), TimePeriod: l.Period}); err != nil {
		return scrub.Error(fmt.Errorf("UpdateSpendingLimit: %w", err))
	}
	return nil
}

// DeleteLimit deletes a spending limit.
func DeleteLimit(ctx context.Context, ls Limiters, region, arn string) error {
	c, err := ls(ctx, region)
	if err != nil {
		return scrub.Error(err)
	}
	if _, err := c.DeleteSpendingLimit(ctx, &braketsdk.DeleteSpendingLimitInput{SpendingLimitArn: aws.String(arn)}); err != nil {
		return scrub.Error(fmt.Errorf("DeleteSpendingLimit: %w", err))
	}
	return nil
}
