package braket

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
)

// Limit is a spending limit and the region it was found in. Spending limits
// live in their device's region; SearchSpendingLimits in another region
// gives wrong results (Amazon Braket Developer Guide, spending limits).
type Limit struct {
	Region  string
	Summary types.SpendingLimitSummary
}

// SearchLimits lists the spending limits in every region, sorted by region
// and device. A region that fails is named in an *IncompleteError, with the
// rest returned (law 3).
func SearchLimits(ctx context.Context, f Factory, regions []string) ([]Limit, error) {
	results := FanOut(ctx, regions, func(ctx context.Context, region string) ([]Limit, error) {
		api, err := f.Braket(ctx, region)
		if err != nil {
			return nil, err
		}
		var out []Limit
		p := braket.NewSearchSpendingLimitsPaginator(api, &braket.SearchSpendingLimitsInput{})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("SearchSpendingLimits: %w", err)
			}
			for _, l := range page.SpendingLimits {
				out = append(out, Limit{Region: region, Summary: l})
			}
		}
		return out, nil
	})
	var all []Limit
	for _, r := range results {
		all = append(all, r.Value...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Region != all[j].Region {
			return all[i].Region < all[j].Region
		}
		return aws.ToString(all[i].Summary.DeviceArn) < aws.ToString(all[j].Summary.DeviceArn)
	})
	return all, Incomplete("SearchSpendingLimits", results)
}
