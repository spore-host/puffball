// Package ledger is puff's account-side spend estimate. It prices tasks the
// service lists (from every region; the caller searches them) using the
// embedded price table, and groups the figures by project, device, rung or
// day.
//
// Every dollar figure here is an estimate: pricing-page arithmetic as of the
// price table's date (#41's kind 1). It is never a bill, and figures of
// different standing are never summed together:
//
//   - completed tasks: the estimate;
//   - created, queued and running tasks: pending, kept apart;
//   - cancelled, cancelling and failed tasks: what they would cost if billed.
//     The pricing page doesn't say whether they are, so they stay out of the
//     total;
//   - tasks on a device the price table doesn't price: unpriced, named, with
//     no figure (law 2).
//
// A QPU task's estimate is the per-task fee plus its shots times the
// per-shot price. A program set's shots are the total across its programs
// (measured, G0(c), #4) and it pays one per-task fee, so the same arithmetic
// holds. A simulator task is billed for its duration with a minimum. The
// duration comes from the task's results.json in S3,
// additionalMetadata.simulatorMetadata.executionDuration in milliseconds
// (measured, G0(c), #4). It isn't in any Braket API response. When it can't
// be read, the estimate is the minimum charge and says "at least".
//
// The ledger is rebuilt from the service every time (law 4). The only local
// state is a cache of the durations read from S3 for finished tasks
// ([FileCache]); deleting it costs only the S3 reads.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	"github.com/spore-host/puffball/internal/scrub"
	"github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/results"
)

// Standing is how a task's figure counts.
type Standing string

// The standings a line can have. Figures of different standings are never
// summed together.
const (
	Completed Standing = "completed"
	Pending   Standing = "pending"
	IfBilled  Standing = "if-billed"
	Unpriced  Standing = "unpriced"
)

// Untagged is the group for tasks without the grouping tag.
const Untagged = "untagged"

// DurationSource names where a simulator's billed duration came from.
const DurationSource = "results.json additionalMetadata.simulatorMetadata.executionDuration (ms)"

// resultsLimit caps a results.json read. It holds every measurement, so a
// large task's file is large. A larger file gets the minimum estimate, with
// a note.
const resultsLimit = 256 << 20

// Tag keys the ledger groups by (set by puff run, #39).
const (
	tagProject = "puffball:project"
	tagRung    = "puffball:rung"
	tagRunID   = "puffball:run-id"
)

// Figure is a task's estimate: the pricing-page arithmetic for what the
// service reports about it. It always says it is an estimate, with the price
// date (law 2).
type Figure struct {
	Kind       string     `json:"kind"`
	PricesAsOf string     `json:"prices_as_of"`
	Nano       price.Nano `json:"-"`
	USD        string     `json:"estimate_usd"`
	Arithmetic string     `json:"arithmetic"`
	// Minimum is true when it is a simulator's minimum charge because no
	// billed duration was read: the real figure can only be higher.
	Minimum      bool   `json:"minimum,omitempty"`
	BilledMillis int64  `json:"billed_ms,omitempty"`
	PerTaskUSD   string `json:"per_task_usd,omitempty"`
	PerShotUSD   string `json:"per_shot_usd,omitempty"`
	PerMinuteUSD string `json:"per_minute_usd,omitempty"`
}

func figure(e price.Estimate) *Figure {
	return &Figure{Kind: e.Kind, PricesAsOf: e.PricesAsOf, Nano: e.Total, USD: e.TotalUSD, Arithmetic: e.Arithmetic,
		Minimum: e.Minimum, BilledMillis: e.BilledMillis, PerTaskUSD: e.PerTaskUSD, PerShotUSD: e.PerShotUSD, PerMinuteUSD: e.PerMinuteUSD}
}

// Line is one task and its estimate.
type Line struct {
	TaskID    string    `json:"task_id"`
	Region    string    `json:"region"`
	Device    string    `json:"device"`
	DeviceArn string    `json:"device_arn"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Shots     int64     `json:"shots"`
	Project   string    `json:"project"`
	Rung      string    `json:"rung"`
	RunID     string    `json:"run_id,omitempty"`
	Standing  Standing  `json:"standing"`
	// Estimate is nil for an unpriced line.
	Estimate *Figure `json:"estimate,omitempty"`
	// DurationMs is a simulator's billed duration as the service reported
	// it, when it was read; DurationSource says from where.
	DurationMs     *int64 `json:"duration_ms,omitempty"`
	DurationSource string `json:"duration_source,omitempty"`
	// Note says why a figure is a minimum or missing.
	Note string `json:"note,omitempty"`
	// Missing names what an unpriced line lacks: "price" (the device isn't
	// in the price table) or "shots" (the service reported none).
	Missing string `json:"missing,omitempty"`
}

// Ledger is every priced task.
type Ledger struct {
	PricesAsOf string `json:"prices_as_of"`
	Lines      []Line `json:"tasks"`
}

// Options configure [Build].
type Options struct {
	Prices *price.Table
	// Reader reads results.json for simulator durations; nil reads none
	// (every simulator estimate is then the minimum).
	Reader results.Reader
	// Account returns the caller's account, the expected owner of the results
	// buckets. It is called only if an object is read.
	Account func(ctx context.Context) (string, error)
	// Cache holds durations already read; nil caches nothing.
	Cache *FileCache
}

// Build prices tasks. A failed duration read is not an error: that line is
// estimated at the minimum and says why.
func Build(ctx context.Context, tasks []braket.Task, o Options) (*Ledger, error) {
	if o.Prices == nil {
		return nil, errors.New("ledger: no price table")
	}
	l := &Ledger{PricesAsOf: o.Prices.AsOf, Lines: make([]Line, 0, len(tasks))}
	acct := accountOnce(o.Account)
	for _, t := range tasks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := price1(ctx, t, o, acct)
		if err != nil {
			return nil, err
		}
		l.Lines = append(l.Lines, line)
	}
	sort.SliceStable(l.Lines, func(i, j int) bool { return l.Lines[i].CreatedAt.Before(l.Lines[j].CreatedAt) })
	return l, nil
}

func accountOnce(f func(context.Context) (string, error)) func(context.Context) (string, error) {
	var acct string
	var err error
	done := false
	return func(ctx context.Context) (string, error) {
		if !done {
			done = true
			if f == nil {
				err = errors.New("no account source")
			} else {
				acct, err = f(ctx)
			}
		}
		return acct, err
	}
}

func standing(s types.QuantumTaskStatus) Standing {
	switch s {
	case types.QuantumTaskStatusCompleted:
		return Completed
	case types.QuantumTaskStatusCancelled, types.QuantumTaskStatusCancelling, types.QuantumTaskStatusFailed:
		return IfBilled
	default:
		return Pending
	}
}

func price1(ctx context.Context, t braket.Task, o Options, acct func(context.Context) (string, error)) (Line, error) {
	s := t.Summary
	arn := aws.ToString(s.QuantumTaskArn)
	dev := aws.ToString(s.DeviceArn)
	line := Line{TaskID: arn[strings.LastIndex(arn, "/")+1:], Region: t.Region,
		Device: dev[strings.LastIndex(dev, "/")+1:], DeviceArn: dev, Status: string(s.Status),
		Shots: aws.ToInt64(s.Shots), Project: tagOr(s.Tags, tagProject), Rung: tagOr(s.Tags, tagRung),
		RunID: s.Tags[tagRunID], Standing: standing(s.Status)}
	if s.CreatedAt != nil {
		line.CreatedAt = s.CreatedAt.UTC()
	}
	d, err := o.Prices.Lookup(dev)
	if err != nil {
		line.Standing, line.Note, line.Missing = Unpriced, err.Error(), "price"
		return line, nil
	}
	if s.Shots == nil && !d.Simulator() {
		// A QPU is billed per shot; with no shot count there is no figure
		// (law 6), never a fee-only one.
		line.Standing, line.Note, line.Missing = Unpriced, "the service reported no shots for this task", "shots"
		return line, nil
	}
	var dur time.Duration
	known := false
	if d.Simulator() {
		switch {
		case line.Standing == Pending:
			line.Note = "no billed duration until the task completes"
		case s.Status != types.QuantumTaskStatusCompleted:
			line.Note = "no billed duration: the task didn't complete"
		default:
			ms, err := duration(ctx, t, o, acct)
			if err != nil {
				line.Note = "billed duration not read: " + scrub.Error(err).Error()
				break
			}
			dur, known = time.Duration(ms)*time.Millisecond, true
			line.DurationMs, line.DurationSource = &ms, DurationSource
		}
	}
	e, err := o.Prices.Task(d, line.Shots, dur, known)
	if err != nil {
		return Line{}, fmt.Errorf("ledger: price task %s: %w", line.TaskID, err)
	}
	line.Estimate = figure(e)
	return line, nil
}

func tagOr(tags map[string]string, k string) string {
	if v := tags[k]; v != "" {
		return v
	}
	return Untagged
}

// duration returns a finished simulator task's billed duration in ms, from
// the cache or its results.json.
func duration(ctx context.Context, t braket.Task, o Options, acct func(context.Context) (string, error)) (int64, error) {
	arn := aws.ToString(t.Summary.QuantumTaskArn)
	// Keyed by region and task UUID: the cache holds no account ID.
	ck := t.Region + "/" + arn[strings.LastIndex(arn, "/")+1:]
	if ms, ok := o.Cache.Get(ck); ok {
		return ms, nil
	}
	if o.Reader == nil {
		return 0, errors.New("no results reader")
	}
	bucket, dir := aws.ToString(t.Summary.OutputS3Bucket), aws.ToString(t.Summary.OutputS3Directory)
	if bucket == "" || dir == "" {
		return 0, errors.New("the task names no results location")
	}
	a, err := acct(ctx)
	if err != nil {
		return 0, fmt.Errorf("account: %w", err)
	}
	b, err := o.Reader.Read(ctx, t.Region, bucket, dir+"/results.json", a, resultsLimit)
	if err != nil {
		return 0, err
	}
	ms, err := ParseDuration(b)
	if err != nil {
		return 0, err
	}
	o.Cache.Put(ck, ms)
	return ms, nil
}

// ParseDuration reads additionalMetadata.simulatorMetadata.executionDuration
// (milliseconds) from a simulator task's results.json.
func ParseDuration(b []byte) (int64, error) {
	var r struct {
		AdditionalMetadata struct {
			SimulatorMetadata *struct {
				ExecutionDuration *int64 `json:"executionDuration"`
			} `json:"simulatorMetadata"`
		} `json:"additionalMetadata"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, fmt.Errorf("results.json: %w", err)
	}
	sm := r.AdditionalMetadata.SimulatorMetadata
	if sm == nil || sm.ExecutionDuration == nil {
		return 0, errors.New("results.json has no additionalMetadata.simulatorMetadata.executionDuration")
	}
	if *sm.ExecutionDuration < 0 {
		return 0, errors.New("results.json executionDuration is negative")
	}
	return *sm.ExecutionDuration, nil
}

// Unpriced says why lines have no figure, one entry per device and reason,
// sorted: "<device>: no price in the table" or "<device>: no shots reported".
func (l *Ledger) Unpriced() []string {
	set := map[string]bool{}
	for _, ln := range l.Lines {
		if ln.Standing != Unpriced {
			continue
		}
		why := "no price in the table"
		if ln.Missing == "shots" {
			why = "no shots reported"
		}
		set[ln.Device+": "+why] = true
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Sum is one standing's figure for a group.
type Sum struct {
	Tasks int   `json:"tasks"`
	Shots int64 `json:"shots"`
	// Nano is the estimate in nano-dollars; USD is it as a dollar string.
	Nano price.Nano `json:"-"`
	USD  string     `json:"estimate_usd"`
	// AtLeast is true when a line in it is a minimum charge, so the real
	// figure can only be higher.
	AtLeast bool `json:"at_least,omitempty"`
}

func (s *Sum) add(ln Line) {
	s.Tasks++
	s.Shots += ln.Shots
	if ln.Estimate != nil {
		s.Nano += ln.Estimate.Nano
		s.AtLeast = s.AtLeast || ln.Estimate.Minimum
	}
	s.USD = s.Nano.String()
}

// Group is the figures for one key. The standings are kept apart.
type Group struct {
	Key       string `json:"key"`
	Completed Sum    `json:"completed"`
	Pending   Sum    `json:"pending"`
	IfBilled  Sum    `json:"if_billed"`
	// Unpriced counts tasks with no figure, and their shots.
	Unpriced      int   `json:"unpriced_tasks"`
	UnpricedShots int64 `json:"unpriced_shots"`
}

// Tasks and Shots count every task in the group, whatever its standing.
func (g Group) Tasks() int {
	return g.Completed.Tasks + g.Pending.Tasks + g.IfBilled.Tasks + g.Unpriced
}

// Shots is the shots of every task in the group.
func (g Group) Shots() int64 {
	return g.Completed.Shots + g.Pending.Shots + g.IfBilled.Shots + g.UnpricedShots
}

func (g *Group) add(ln Line) {
	switch ln.Standing {
	case Completed:
		g.Completed.add(ln)
	case Pending:
		g.Pending.add(ln)
	case IfBilled:
		g.IfBilled.add(ln)
	case Unpriced:
		g.Unpriced++
		g.UnpricedShots += ln.Shots
	}
}

// Bys are the groupings Group accepts.
var Bys = []string{"project", "device", "rung", "day"}

func key(by string, ln Line) string {
	switch by {
	case "project":
		return ln.Project
	case "device":
		return ln.Device
	case "rung":
		return ln.Rung
	default: // day
		if ln.CreatedAt.IsZero() {
			return "unknown"
		}
		return ln.CreatedAt.Format("2006-01-02")
	}
}

// Group groups the lines by project, device, rung or day (UTC creation
// date), sorted by key, and returns the groups and their total.
func (l *Ledger) Group(by string) ([]Group, Group, error) {
	ok := false
	for _, b := range Bys {
		ok = ok || b == by
	}
	if !ok {
		return nil, Group{}, fmt.Errorf("group by %q: want one of %s", by, strings.Join(Bys, ", "))
	}
	m := map[string]*Group{}
	total := Group{Key: "total"}
	for _, ln := range l.Lines {
		k := key(by, ln)
		g, ok := m[k]
		if !ok {
			g = &Group{Key: k}
			m[k] = g
		}
		g.add(ln)
		total.add(ln)
	}
	out := make([]Group, 0, len(m))
	for _, g := range m {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	fill := func(g *Group) {
		for _, s := range []*Sum{&g.Completed, &g.Pending, &g.IfBilled} {
			s.USD = s.Nano.String()
		}
	}
	for i := range out {
		fill(&out[i])
	}
	fill(&total)
	return out, total, nil
}
