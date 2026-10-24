package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

// Cost Explorer API requests are billed per request (AWS Cost Explorer pricing
// page: $0.01 per request with the primary billing view, read 2026-10-24).
const (
	ceAsOf          = "2026-10-24"
	cePerRequestUSD = 0.01
	ceRequests      = 3
	// costExplorerRegion is the Cost Explorer API's endpoint region.
	costExplorerRegion = "us-east-1"
)

// runCosts is G0(e) (#6): how Braket usage appears in Cost Explorer. Billable.
func runCosts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("costs", flag.ContinueOnError)
	out := fs.String("out", ".probe/costs", "output directory (raw, unscrubbed)")
	real := fs.Bool("real", false, "actually call Cost Explorer (billable)")
	days := fs.Int("days", 30, "look-back window in days")
	if err := fs.Parse(args); err != nil {
		return usageErr{err.Error()}
	}
	est := cePerRequestUSD * ceRequests
	fmt.Fprintf(os.Stderr, "G0(e): %d Cost Explorer requests: estimate %d x $%.2f = %s (Cost Explorer pricing page, as of %s)\n", ceRequests, ceRequests, cePerRequestUSD, money(est), ceAsOf)
	if !*real {
		fmt.Fprintln(os.Stderr, "Not called. Pass --real to spend this.")
		return nil
	}
	cfg, rec, err := loadConfig(ctx, costExplorerRegion)
	if err != nil {
		return err
	}
	ce := costexplorer.NewFromConfig(cfg)
	end := time.Now().UTC().Format("2006-01-02")
	start := time.Now().UTC().AddDate(0, 0, -*days).Format("2006-01-02")
	period := &cetypes.DateInterval{Start: aws.String(start), End: aws.String(end)}

	// 1: which SERVICE dimension value is Braket.
	dv, err := ce.GetDimensionValues(ctx, &costexplorer.GetDimensionValuesInput{
		TimePeriod: period, Dimension: cetypes.DimensionService, SearchString: aws.String("Braket"),
	})
	if err != nil {
		return fmt.Errorf("GetDimensionValues: %w", err)
	}
	if err := writeRaw(filepath.Join(*out, "dimension-values-service.json"), rec.Last()); err != nil {
		return err
	}
	var services []string
	for _, v := range dv.DimensionValues {
		services = append(services, aws.ToString(v.Value))
	}
	fmt.Fprintf(os.Stderr, "SERVICE values matching Braket: %v\n", services)
	if len(services) == 0 {
		return fmt.Errorf("no Cost Explorer SERVICE value matches Braket in %s..%s (1 request made, %s)", start, end, money(cePerRequestUSD))
	}

	// 2: Braket cost by usage type over the window.
	if _, err := ce.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
		TimePeriod:  period,
		Granularity: cetypes.GranularityMonthly,
		Metrics:     []string{"UnblendedCost", "UsageQuantity"},
		Filter:      &cetypes.Expression{Dimensions: &cetypes.DimensionValues{Key: cetypes.DimensionService, Values: services}},
		GroupBy:     []cetypes.GroupDefinition{{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("USAGE_TYPE")}},
	}); err != nil {
		return fmt.Errorf("GetCostAndUsage: %w", err)
	}
	if err := writeRaw(filepath.Join(*out, "cost-by-usage-type.json"), rec.Last()); err != nil {
		return err
	}

	// 3: whether the puffball:project tag is visible to Cost Explorer at all
	// (it is only once activated as a cost allocation tag).
	if _, err := ce.GetTags(ctx, &costexplorer.GetTagsInput{TimePeriod: period, TagKey: aws.String("puffball:project")}); err != nil {
		if err := writeRaw(filepath.Join(*out, "get-tags-error.txt"), []byte(err.Error())); err != nil {
			return err
		}
	} else if err := writeRaw(filepath.Join(*out, "get-tags-puffball-project.json"), rec.Last()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d requests made (estimate %s); raw responses in %s (%s)\n", ceRequests, money(est), *out, strings.Join([]string{start, end}, ".."))
	return nil
}
