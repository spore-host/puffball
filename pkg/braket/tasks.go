package braket

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
)

// TaskQuery selects quantum tasks across regions.
type TaskQuery struct {
	// Statuses to include; empty means any status (one search, no window).
	// The service rejects an EQUAL status filter with several values (G0(c),
	// #4), so each status is a separate search. Searches run in lifecycle
	// order (CREATED → QUEUED → RUNNING → CANCELLING → terminal), so a task
	// that moves forward between two searches is seen at its later status if
	// that status is also queried. A task that leaves the queried set between
	// samples is not reported: the result is a series of snapshots, not one.
	Statuses []types.QuantumTaskStatus
	// Since, if non-zero, keeps tasks created at or after it.
	Since time.Time
	// Tags that must all match. SearchQuantumTasks can't filter on tags (#4),
	// so they are matched on the returned summaries. An empty value matches
	// any value of that key.
	Tags map[string]string
}

// Task is a task summary with the region it was found in.
type Task struct {
	Region  string
	Summary types.QuantumTaskSummary
}

// SearchTasks searches every region for tasks matching q, sorted by creation
// time then ARN. Tasks come only from regions that completed: if any region
// failed, the error is an *IncompleteError naming it and the tasks returned
// cover the other regions.
func SearchTasks(ctx context.Context, f Factory, regions []string, q TaskQuery) ([]Task, error) {
	results := FanOut(ctx, regions, func(ctx context.Context, region string) ([]Task, error) {
		return searchRegionTasks(ctx, f, region, q)
	})
	var tasks []Task
	for _, r := range results {
		if r.Err == nil {
			tasks = append(tasks, r.Value...)
		}
	}
	sort.Slice(tasks, func(i, j int) bool {
		ti, tj := aws.ToTime(tasks[i].Summary.CreatedAt), aws.ToTime(tasks[j].Summary.CreatedAt)
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return aws.ToString(tasks[i].Summary.QuantumTaskArn) < aws.ToString(tasks[j].Summary.QuantumTaskArn)
	})
	return tasks, Incomplete("SearchQuantumTasks", results)
}

func searchRegionTasks(ctx context.Context, f Factory, region string, q TaskQuery) ([]Task, error) {
	c, err := f.Braket(ctx, region)
	if err != nil {
		return nil, err
	}
	statuses := lifecycleOrder(q.Statuses)
	if len(statuses) == 0 {
		statuses = []types.QuantumTaskStatus{""} // one search, no status filter
	}
	seen := map[string]bool{}
	var out []Task
	for _, st := range statuses {
		filters := []types.SearchQuantumTasksFilter{}
		if st != "" {
			filters = append(filters, types.SearchQuantumTasksFilter{
				Name: aws.String("status"), Operator: types.SearchQuantumTasksFilterOperatorEqual, Values: []string{string(st)},
			})
		}
		if !q.Since.IsZero() {
			filters = append(filters, types.SearchQuantumTasksFilter{
				Name: aws.String("createdAt"), Operator: types.SearchQuantumTasksFilterOperatorGte, Values: []string{q.Since.UTC().Format(time.RFC3339)},
			})
		}
		pg := braket.NewSearchQuantumTasksPaginator(c, &braket.SearchQuantumTasksInput{Filters: filters})
		for pg.HasMorePages() {
			page, err := pg.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("SearchQuantumTasks: %w", err)
			}
			for _, s := range page.QuantumTasks {
				arn := aws.ToString(s.QuantumTaskArn)
				if seen[arn] || !MatchTags(s.Tags, q.Tags) {
					continue
				}
				seen[arn] = true
				out = append(out, Task{Region: region, Summary: s})
			}
		}
	}
	return out, nil
}

// lifecycleRank orders task statuses as tasks move through them.
var lifecycleRank = map[types.QuantumTaskStatus]int{
	types.QuantumTaskStatusCreated:    0,
	types.QuantumTaskStatusQueued:     1,
	types.QuantumTaskStatusRunning:    2,
	types.QuantumTaskStatusCancelling: 3,
	types.QuantumTaskStatusCompleted:  4,
	types.QuantumTaskStatusFailed:     4,
	types.QuantumTaskStatusCancelled:  4,
}

// lifecycleOrder returns statuses deduplicated and sorted by lifecycle rank;
// unknown statuses go last, by name.
func lifecycleOrder(in []types.QuantumTaskStatus) []types.QuantumTaskStatus {
	seen := map[types.QuantumTaskStatus]bool{}
	var out []types.QuantumTaskStatus
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	rank := func(s types.QuantumTaskStatus) int {
		if r, ok := lifecycleRank[s]; ok {
			return r
		}
		return len(lifecycleRank)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// MatchTags reports whether tags carries every key in want with the wanted
// value; an empty wanted value matches any value.
func MatchTags(tags, want map[string]string) bool {
	for k, v := range want {
		got, ok := tags[k]
		if !ok || (v != "" && got != v) {
			return false
		}
	}
	return true
}

// Terminal reports whether a task status is final.
func Terminal(s types.QuantumTaskStatus) bool {
	return s == types.QuantumTaskStatusCompleted || s == types.QuantumTaskStatusFailed || s == types.QuantumTaskStatusCancelled
}

// GetTask reads a task, with its queue position, in the region its ARN names.
func GetTask(ctx context.Context, f Factory, arn string) (*braket.GetQuantumTaskOutput, error) {
	region := RegionOf(arn)
	if region == "" {
		return nil, fmt.Errorf("%q is not a quantum task ARN", arn)
	}
	c, err := f.Braket(ctx, region)
	if err != nil {
		return nil, err
	}
	out, err := c.GetQuantumTask(ctx, &braket.GetQuantumTaskInput{
		QuantumTaskArn:           aws.String(arn),
		AdditionalAttributeNames: []types.QuantumTaskAdditionalAttributeName{types.QuantumTaskAdditionalAttributeNameQueueInfo},
	})
	if err != nil {
		return nil, fmt.Errorf("GetQuantumTask: %w", err)
	}
	return out, nil
}

// TaskARN builds a quantum task ARN from its parts.
func TaskARN(region, account, id string) string {
	return fmt.Sprintf("arn:aws:braket:%s:%s:quantum-task/%s", region, account, id)
}

// taskNotHere is the service's answer when a task ARN names a task that
// doesn't exist in the region asked: measured 2026-11-01 for both a random
// UUID and a real task asked about in another region, GetQuantumTask returns
// ValidationException "quantumTaskArn is invalid." (never
// ResourceNotFoundException).
const taskNotHere = "quantumTaskArn is invalid"

// IsNotFound reports whether err is the service's "no such task here".
// Callers build ARNs only from IDs that pass TaskIDPattern, so this can't
// hide a malformed ARN.
func IsNotFound(err error) bool {
	var ve *types.ValidationException
	if errors.As(err, &ve) && strings.Contains(aws.ToString(ve.Message), taskNotHere) {
		return true
	}
	var nf *types.ResourceNotFoundException
	return errors.As(err, &nf)
}

// TaskIDPattern is a quantum task ID: a lowercase UUID, as every task ARN in
// the G0 fixtures ends with.
var TaskIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// FindTask finds a task by its ID in any of regions, for an account: an ID
// carries no region, so each region is asked. It returns the first match.
// If no region has it and some region failed, the error is an
// *IncompleteError, since the task may be in a region that didn't answer.
func FindTask(ctx context.Context, f Factory, regions []string, account, id string) (*braket.GetQuantumTaskOutput, error) {
	if !TaskIDPattern.MatchString(id) {
		return nil, fmt.Errorf("%q is not a task ID (a UUID) or a task ARN", id)
	}
	results := FanOut(ctx, regions, func(ctx context.Context, region string) (*braket.GetQuantumTaskOutput, error) {
		out, err := GetTask(ctx, f, TaskARN(region, account, id))
		if err != nil && IsNotFound(err) {
			return nil, nil
		}
		return out, err
	})
	for _, r := range results {
		if r.Err == nil && r.Value != nil {
			return r.Value, nil
		}
	}
	if err := Incomplete("GetQuantumTask", results); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("task %s was not found in any region (%s)", id, strings.Join(regions, ", "))
}
