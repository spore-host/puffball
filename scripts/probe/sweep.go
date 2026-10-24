package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
)

// activeStatuses are the task states that can still spend or hold budget.
var activeStatuses = []string{"CREATED", "QUEUED", "RUNNING", "CANCELLING"}

// puffballTagged reports whether any tag key is in the puffball: namespace.
func puffballTagged(tags map[string]string) bool {
	for k := range tags {
		if strings.HasPrefix(k, "puffball:") {
			return true
		}
	}
	return false
}

// sweepFinding is one thing sweep-check found in a region.
type sweepFinding struct {
	Region, Kind, Detail string
	Puffball             bool
}

// runSweepCheck asserts that no puffball-tagged task is queued or running and
// no puffball-tagged spending limit remains, in every Braket region (law 8).
// Untagged active tasks and limits are listed but don't fail the check: they
// aren't puffball's. A region that errors fails the check (law 3).
func runSweepCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sweep-check", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return usageErr{err.Error()}
	}
	seed, err := seedRegion(ctx)
	if err != nil {
		return err
	}
	d, err := discover(ctx, seed)
	if err != nil {
		return err
	}
	regions := d.sortedRegions()
	var mu sync.Mutex
	var findings []sweepFinding
	errs := map[string]error{}
	var wg sync.WaitGroup
	// Discovery errors are recorded before any goroutine starts, so errs is
	// only written concurrently under mu.
	for _, r := range regions {
		if d.Regions[r].Err != nil {
			errs[r] = d.Regions[r].Err
		}
	}
	for _, r := range regions {
		if errs[r] != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := sweepRegion(ctx, r)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[r] = err
				return
			}
			findings = append(findings, f...)
		}()
	}
	wg.Wait()

	sort.Slice(findings, func(i, j int) bool {
		return findings[i].Region+findings[i].Detail < findings[j].Region+findings[j].Detail
	})
	bad := 0
	for _, f := range findings {
		mark := "  other   "
		if f.Puffball {
			mark = "  PUFFBALL"
			bad++
		}
		fmt.Printf("%s %-10s %-14s %s\n", mark, f.Region, f.Kind, f.Detail)
	}
	for _, r := range regions {
		if err := errs[r]; err != nil {
			fmt.Printf("  ERROR    %-10s %s\n", r, scrubbed(err))
		}
	}
	fmt.Printf("sweep-check: %d region(s) checked (%s); %d puffball-tagged active task(s)/limit(s); %d other; %d region error(s)\n",
		len(regions), strings.Join(regions, ", "), bad, len(findings)-bad, len(errs))
	if bad > 0 || len(errs) > 0 {
		return fmt.Errorf("sweep-check failed")
	}
	fmt.Fprintln(os.Stderr, "✓ nothing puffball-tagged is queued, running, or holding a spending limit")
	return nil
}

func sweepRegion(ctx context.Context, region string) ([]sweepFinding, error) {
	cfg, _, err := loadConfig(ctx, region)
	if err != nil {
		return nil, err
	}
	bc := braket.NewFromConfig(cfg)
	var out []sweepFinding
	// One search per status: an EQUAL status filter with several values is
	// rejected with ValidationException (measured in G0).
	for _, status := range activeStatuses {
		tp := braket.NewSearchQuantumTasksPaginator(bc, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{{
			Name: aws.String("status"), Operator: types.SearchQuantumTasksFilterOperatorEqual, Values: []string{status},
		}}})
		for tp.HasMorePages() {
			page, err := tp.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("SearchQuantumTasks status=%s: %w", status, err)
			}
			for _, t := range page.QuantumTasks {
				id := aws.ToString(t.QuantumTaskArn)
				id = id[strings.LastIndex(id, "/")+1:]
				out = append(out, sweepFinding{region, "task " + string(t.Status), id + " " + aws.ToString(t.DeviceArn), puffballTagged(t.Tags)})
			}
		}
	}
	lp := braket.NewSearchSpendingLimitsPaginator(bc, &braket.SearchSpendingLimitsInput{})
	for lp.HasMorePages() {
		page, err := lp.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("SearchSpendingLimits: %w", err)
		}
		for _, l := range page.SpendingLimits {
			out = append(out, sweepFinding{region, "spending-limit", aws.ToString(l.DeviceArn) + " limit $" + aws.ToString(l.SpendingLimit), puffballTagged(l.Tags)})
		}
	}
	return out, nil
}
