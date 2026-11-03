// Package budget wraps Amazon Braket spending limits (law 5: Braket enforces
// them; puff never reimplements them). A spending limit is per QPU and lives
// in the QPU's region. Braket checks each new task's estimated cost against
// the limit's remaining budget (limit − current spend − queued spend) and
// rejects a task that doesn't fit, or any task outside the limit's period
// (Amazon Braket Developer Guide, "Setting spending limits for Amazon Braket
// QPUs"). Simulators, notebooks, Hybrid Job instances and Braket Direct
// reservations aren't covered by spending limits.
//
// The calls that change limits go through pkg/gate; this package decides
// what to change.
package budget

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	"github.com/spore-host/puffball/internal/scrub"
	"github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/devices"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/price"
)

// Tag marks limits puff created, so make sweep-check and people can find them.
const Tag = "puffball:project"

// Target is a QPU to set or remove a limit on, in its region.
type Target struct {
	Name   string `json:"device"`
	Arn    string `json:"device_arn"`
	Region string `json:"region"`
}

// ErrNotQPU is returned for a simulator: spending limits apply to QPUs only.
var ErrNotQPU = errors.New("spending limits apply only to QPUs; simulators, notebooks, Hybrid Job instances and Braket Direct reservations aren't covered (Amazon Braket Developer Guide, spending limits)")

// Targets resolves names (aliases, names or ARNs) to QPUs, or every QPU in
// the list that isn't RETIRED when allQPUs is set.
func Targets(l *devices.List, aliases devices.Aliases, names []string, allQPUs bool) ([]Target, error) {
	var out []Target
	add := func(d devices.Device) error {
		if d.Type != string(types.DeviceTypeQpu) {
			return fmt.Errorf("%s: %w", d.Name, ErrNotQPU)
		}
		if len(d.Regions) != 1 {
			return fmt.Errorf("%s is listed in %d regions; a QPU has one", d.Name, len(d.Regions))
		}
		out = append(out, Target{Name: d.Name, Arn: d.Arn, Region: d.Regions[0]})
		return nil
	}
	if allQPUs {
		for _, d := range l.Devices {
			if d.Type == string(types.DeviceTypeQpu) && d.Status != string(types.DeviceStatusRetired) {
				if err := add(d); err != nil {
					return nil, err
				}
			}
		}
		if len(out) == 0 {
			return nil, errors.New("no QPUs found")
		}
	}
	for _, n := range names {
		d, err := l.Resolve(n, aliases)
		if err != nil {
			return nil, err
		}
		if err := add(d); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Arn < out[j].Arn })
	uniq := out[:0]
	for i, t := range out {
		if i == 0 || t.Arn != out[i-1].Arn {
			uniq = append(uniq, t)
		}
	}
	return uniq, nil
}

var usdPattern = regexp.MustCompile(`^\d{1,9}(\.\d{1,2})?$`)

// CheckUSD validates a limit amount: dollars and cents, at least $0 (a $0
// limit blocks every task on the device).
func CheckUSD(s string) error {
	if !usdPattern.MatchString(s) {
		return fmt.Errorf("%q: want dollars and cents, like 1 or 2.50", s)
	}
	return nil
}

// Period returns the time period for a limit in force from `from` until
// `until`; both zero means no period (the limit always applies).
func Period(from, until, now time.Time) (*types.TimePeriod, error) {
	if until.IsZero() && from.IsZero() {
		return nil, nil
	}
	if from.IsZero() {
		from = now
	}
	if until.IsZero() {
		return nil, errors.New("--from needs --until: a period has an end")
	}
	if !until.After(from) {
		return nil, fmt.Errorf("--until %s isn't after %s", until.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	if !until.After(now) {
		return nil, fmt.Errorf("--until %s is in the past; outside its period a limit rejects every task", until.Format(time.RFC3339))
	}
	return &types.TimePeriod{StartAt: aws.Time(from.UTC()), EndAt: aws.Time(until.UTC())}, nil
}

// Change is what happened to one device's limit.
type Change struct {
	Target
	// Action is created, updated, unchanged, deleted, kept (a limit puff
	// didn't create, left alone), none (rm with no limit) or failed.
	Action string `json:"action"`
	// Before and After are the limit amounts, in USD, as Braket stores them.
	Before string `json:"before_usd,omitempty"`
	After  string `json:"after_usd,omitempty"`
	// NotPuffs is true when the limit existed and wasn't created by puff.
	NotPuffs bool   `json:"not_created_by_puff,omitempty"`
	Error    string `json:"error,omitempty"`
}

// find returns the device's limit in its region, if any.
func find(ctx context.Context, f braket.Factory, t Target) (*types.SpendingLimitSummary, error) {
	api, err := f.Braket(ctx, t.Region)
	if err != nil {
		return nil, err
	}
	out, err := api.SearchSpendingLimits(ctx, &braketsdk.SearchSpendingLimitsInput{Filters: []types.SearchSpendingLimitsFilter{
		{Name: aws.String("deviceArn"), Operator: types.SearchSpendingLimitsFilterOperatorEqual, Values: []string{t.Arn}}}})
	if err != nil {
		return nil, fmt.Errorf("SearchSpendingLimits: %w", err)
	}
	for i, l := range out.SpendingLimits {
		if aws.ToString(l.DeviceArn) == t.Arn {
			return &out.SpendingLimits[i], nil
		}
	}
	return nil, nil
}

func puffs(tags map[string]string) bool {
	for k := range tags {
		if strings.HasPrefix(k, "puffball:") {
			return true
		}
	}
	return false
}

func samePeriod(a, b *types.TimePeriod) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return aws.ToTime(a.StartAt).Equal(aws.ToTime(b.StartAt)) && aws.ToTime(a.EndAt).Equal(aws.ToTime(b.EndAt))
}

func sameUSD(a, b string) bool {
	x, ex := price.ParseUSD(a)
	y, ey := price.ParseUSD(b)
	return ex == nil && ey == nil && x == y
}

func failed(t Target, err error) Change {
	return Change{Target: t, Action: "failed", Error: scrub.Error(err).Error()}
}

// Set gives each target a limit of usd for period: it creates one, or
// updates the device's existing limit (Braket allows one per device). Each
// device's result is reported; one failing doesn't stop the rest.
//
// A limit puff didn't create is left alone (Action "kept") unless others is
// set: it is someone else's protection.
func Set(ctx context.Context, f braket.Factory, ls gate.Limiters, targets []Target, usd string, period *types.TimePeriod, project string, others bool) []Change {
	var out []Change
	for _, t := range targets {
		cur, err := find(ctx, f, t)
		if err != nil {
			out = append(out, failed(t, err))
			continue
		}
		l := gate.Limit{DeviceArn: t.Arn, USD: usd, Period: period, Tags: map[string]string{Tag: project}}
		if cur == nil {
			if _, err := gate.CreateLimit(ctx, ls, t.Region, l); err != nil {
				out = append(out, failed(t, err))
				continue
			}
			out = append(out, Change{Target: t, Action: "created", After: usd})
			continue
		}
		c := Change{Target: t, Before: aws.ToString(cur.SpendingLimit), After: usd, NotPuffs: !puffs(cur.Tags)}
		if c.NotPuffs && !others {
			c.Action, c.After = "kept", ""
			out = append(out, c)
			continue
		}
		if sameUSD(c.Before, usd) && samePeriod(cur.TimePeriod, period) {
			c.Action = "unchanged"
			out = append(out, c)
			continue
		}
		if err := gate.UpdateLimit(ctx, ls, t.Region, aws.ToString(cur.SpendingLimitArn), l); err != nil {
			out = append(out, failed(t, err))
			continue
		}
		c.Action = "updated"
		out = append(out, c)
	}
	return out
}

// Remove deletes each target's limit, if it has one. A limit puff didn't
// create is kept unless others is set.
func Remove(ctx context.Context, f braket.Factory, ls gate.Limiters, targets []Target, others bool) []Change {
	var out []Change
	for _, t := range targets {
		cur, err := find(ctx, f, t)
		if err != nil {
			out = append(out, failed(t, err))
			continue
		}
		if cur == nil {
			out = append(out, Change{Target: t, Action: "none"})
			continue
		}
		if !puffs(cur.Tags) && !others {
			out = append(out, Change{Target: t, Action: "kept", Before: aws.ToString(cur.SpendingLimit), NotPuffs: true})
			continue
		}
		if err := gate.DeleteLimit(ctx, ls, t.Region, aws.ToString(cur.SpendingLimitArn)); err != nil {
			out = append(out, failed(t, err))
			continue
		}
		out = append(out, Change{Target: t, Action: "deleted", Before: aws.ToString(cur.SpendingLimit), NotPuffs: !puffs(cur.Tags)})
	}
	return out
}

// Failed reports whether any change failed.
func Failed(cs []Change) bool {
	for _, c := range cs {
		if c.Action == "failed" {
			return true
		}
	}
	return false
}

// View is a spending limit as puff shows it. The spend figures are Braket's
// spending-limit accounting: the estimated cost of tasks submitted while the
// limit was active (completed tasks in Spent, created/queued/running ones in
// Queued). They are Braket's estimates, not a bill (#41's kind 3).
type View struct {
	Region    string     `json:"region"`
	Device    string     `json:"device"`
	DeviceArn string     `json:"device_arn"`
	LimitUSD  string     `json:"limit_usd"`
	SpentUSD  string     `json:"spent_usd"`
	QueuedUSD string     `json:"queued_usd"`
	Remaining string     `json:"remaining_usd"`
	From      *time.Time `json:"from,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	ByPuff    bool       `json:"created_by_puff"`
	Project   string     `json:"project,omitempty"`
	Missing   []string   `json:"missing,omitempty"`
}

// Views turns limits into views. Remaining is limit − spent − queued, the
// guide's remaining budget; it is named missing if a figure is absent.
func Views(ls []braket.Limit) []View {
	out := make([]View, 0, len(ls))
	for _, l := range ls {
		s := l.Summary
		dev := aws.ToString(s.DeviceArn)
		v := View{Region: l.Region, Device: dev[strings.LastIndex(dev, "/")+1:], DeviceArn: dev,
			LimitUSD: aws.ToString(s.SpendingLimit), SpentUSD: aws.ToString(s.TotalSpend), QueuedUSD: aws.ToString(s.QueuedSpend),
			CreatedAt: s.CreatedAt, ByPuff: puffs(s.Tags), Project: s.Tags[Tag]}
		if p := s.TimePeriod; p != nil {
			v.From, v.Until = p.StartAt, p.EndAt
		}
		for name, val := range map[string]string{"limit_usd": v.LimitUSD, "spent_usd": v.SpentUSD, "queued_usd": v.QueuedUSD} {
			if val == "" {
				v.Missing = append(v.Missing, name)
			}
		}
		limit, e1 := price.ParseUSD(v.LimitUSD)
		spent, e2 := price.ParseUSD(v.SpentUSD)
		queued, e3 := price.ParseUSD(v.QueuedUSD)
		if e1 == nil && e2 == nil && e3 == nil {
			v.Remaining = (limit - spent - queued).String()
		} else {
			v.Missing = append(v.Missing, "remaining_usd")
		}
		sort.Strings(v.Missing)
		out = append(out, v)
	}
	return out
}
