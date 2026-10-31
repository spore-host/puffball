// Package price is puff's price table and estimator.
//
// Every figure it produces is an estimate: the pricing-page arithmetic, as of
// the table's date (law 2). It is not a bill, not what the service reports
// as incurred, and not spending-limit accounting; see the cost-terms comment
// on #41. An unknown device or price is an error, never a guess or zero.
//
// Amounts are integer nano-dollars, parsed exactly from the table's decimal
// strings, so $0.30 + 100 x $0.000425 is exactly $0.3425 (A-price, #10).
package price

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

//go:embed prices.json
var embedded []byte

// Kind names what an Estimate's dollars are (#41): always "estimate" here.
const Kind = "estimate"

// StaleAfter is how old the table may be before every estimate warns.
const StaleAfter = 60 * 24 * time.Hour

// Nano is an amount in nano-dollars (1e-9 USD).
type Nano int64

// USD returns the amount in dollars, for comparisons that tolerate float.
func (n Nano) USD() float64 { return float64(n) / 1e9 }

// String formats dollars with at least two and at most nine decimals,
// without trailing zeros: $0.30, $0.3425, $0.00375.
func (n Nano) String() string {
	neg := n < 0
	if neg {
		n = -n
	}
	whole, frac := int64(n)/1e9, int64(n)%1e9
	f := strings.TrimRight(fmt.Sprintf("%09d", frac), "0")
	for len(f) < 2 {
		f += "0"
	}
	s := fmt.Sprintf("$%d.%s", whole, f)
	if neg {
		s = "-" + s
	}
	return s
}

// ParseUSD parses a decimal dollar string exactly. More than nine decimal
// places are allowed only if the extra digits are zero (the Price List writes
// "0.0004250000").
func ParseUSD(s string) (Nano, error) {
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") {
		return 0, fmt.Errorf("price %q is not a non-negative decimal", s)
	}
	if len(frac) > 9 {
		if strings.Trim(frac[9:], "0") != "" {
			return 0, fmt.Errorf("price %q has more precision than a nano-dollar", s)
		}
		frac = frac[:9]
	}
	frac += strings.Repeat("0", 9-len(frac))
	// Every character must be a digit: "1e3", "0x10" and "1 2" are not prices.
	digits := func(str string) (int64, bool) {
		var v int64
		for _, c := range str {
			if c < '0' || c > '9' || v > 1e9 {
				return 0, false
			}
			v = v*10 + int64(c-'0')
		}
		return v, true
	}
	w, ok1 := digits(whole)
	f, ok2 := digits(frac)
	if !ok1 || !ok2 || w > 1e9 {
		return 0, fmt.Errorf("price %q is not a non-negative decimal of at most $1,000,000,000", s)
	}
	return Nano(w*1e9 + f), nil
}

// ProgramSet is a device's program-set limits, from its capabilities.
type ProgramSet struct {
	MaxExecutables int   `json:"max_executables"`
	MaxTotalShots  int64 `json:"max_total_shots"`
	MinTotalShots  int64 `json:"min_total_shots"`
}

// Usage names the Price List usagetypes a device is billed under.
type Usage struct {
	Task    string   `json:"task"`
	Shot    string   `json:"shot"`
	Minutes []string `json:"minutes"` // simulators: one per region they run in
}

// usagePrefix maps a Price List usagetype prefix to its region. A prefix
// not here is a table error, so a copy-paste between devices can't pass.
var usagePrefix = map[string]string{
	"USE1": "us-east-1", "USW1": "us-west-1", "USW2": "us-west-2",
	"EUN1": "eu-north-1", "EUW2": "eu-west-2",
}

func usageRegion(usage string) (string, bool) {
	pre, _, ok := strings.Cut(usage, "-")
	r, known := usagePrefix[pre]
	return r, ok && known
}

// Device is one priced device.
type Device struct {
	Alias          string      `json:"alias"`
	Name           string      `json:"name"`
	Arn            string      `json:"arn"`
	PerShotUSD     string      `json:"per_shot_usd"`
	PerMinuteUSD   string      `json:"per_minute_usd"`
	MinimumSeconds int         `json:"minimum_seconds"`
	ShotsRange     [2]int64    `json:"shots_range"` // [min, max] per task, from capabilities
	Usage          Usage       `json:"usage"`
	ProgramSet     *ProgramSet `json:"program_set"`

	perShot, perMinute Nano
}

// Simulator reports whether the device bills per minute.
func (d Device) Simulator() bool { return d.PerMinuteUSD != "" }

// Table is the price table.
type Table struct {
	AsOf       string   `json:"as_of"` // YYYY-MM
	Sources    []string `json:"sources"`
	PerTaskUSD string   `json:"per_task_usd"`
	Devices    []Device `json:"devices"`

	perTask Nano
	asOf    time.Time
}

// Load returns the embedded table.
func Load() (*Table, error) { return Parse(embedded) }

// Parse reads and validates a table.
func Parse(b []byte) (*Table, error) {
	var t Table
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("price table: %w", err)
	}
	var err error
	if t.asOf, err = time.Parse("2006-01", t.AsOf); err != nil {
		return nil, fmt.Errorf("price table as_of %q: want YYYY-MM", t.AsOf)
	}
	if t.perTask, err = ParseUSD(t.PerTaskUSD); err != nil {
		return nil, fmt.Errorf("price table per_task_usd: %w", err)
	}
	seen := map[string]bool{}
	for i := range t.Devices {
		d := &t.Devices[i]
		for _, k := range []string{strings.ToLower(d.Alias), d.Arn} {
			if k == "" || seen[k] {
				return nil, fmt.Errorf("price table: device %q: empty or duplicate alias/ARN %q", d.Name, k)
			}
			seen[k] = true
		}
		if d.ShotsRange[1] <= 0 || d.ShotsRange[0] < 0 || d.ShotsRange[0] > d.ShotsRange[1] {
			return nil, fmt.Errorf("price table %s: shots_range %v is not [min, max]", d.Alias, d.ShotsRange)
		}
		switch {
		case d.Simulator() && d.PerShotUSD == "":
			if d.perMinute, err = ParseUSD(d.PerMinuteUSD); err != nil {
				return nil, fmt.Errorf("price table %s: %w", d.Alias, err)
			}
			if d.MinimumSeconds <= 0 || len(d.Usage.Minutes) == 0 || d.ProgramSet != nil {
				return nil, fmt.Errorf("price table %s: a simulator needs minimum_seconds and usage.minutes, and has no program sets", d.Alias)
			}
			for _, u := range d.Usage.Minutes {
				if _, ok := usageRegion(u); !ok {
					return nil, fmt.Errorf("price table %s: usagetype %q has no known region prefix", d.Alias, u)
				}
			}
		case !d.Simulator() && d.PerShotUSD != "":
			if d.perShot, err = ParseUSD(d.PerShotUSD); err != nil {
				return nil, fmt.Errorf("price table %s: %w", d.Alias, err)
			}
			if d.Usage.Task == "" || d.Usage.Shot == "" {
				return nil, fmt.Errorf("price table %s: a QPU needs usage.task and usage.shot", d.Alias)
			}
			arnRegion := strings.SplitN(d.Arn, ":", 5)
			for _, u := range []string{d.Usage.Task, d.Usage.Shot} {
				if r, ok := usageRegion(u); !ok || len(arnRegion) < 4 || r != arnRegion[3] {
					return nil, fmt.Errorf("price table %s: usagetype %q is not for the ARN's region", d.Alias, u)
				}
			}
		default:
			return nil, fmt.Errorf("price table %s: exactly one of per_shot_usd and per_minute_usd", d.Alias)
		}
	}
	return &t, nil
}

// ErrUnknownDevice is returned (wrapped) for a device the table doesn't price.
var ErrUnknownDevice = errors.New("unknown device")

// Lookup finds a device by alias, name (case-insensitive) or ARN.
func (t *Table) Lookup(s string) (Device, error) {
	for _, d := range t.Devices {
		if strings.EqualFold(s, d.Alias) || strings.EqualFold(s, d.Name) || s == d.Arn {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("%w %q: the price table (as of %s) prices %s; an unpriced device is an error, never a guess (law 2)",
		ErrUnknownDevice, s, t.AsOf, strings.Join(t.Aliases(), ", "))
}

// Aliases returns every device alias, sorted.
func (t *Table) Aliases() []string {
	out := make([]string, len(t.Devices))
	for i, d := range t.Devices {
		out[i] = d.Alias
	}
	sort.Strings(out)
	return out
}

// Age is how old the table is at now, counted from the first of its month
// (the earliest the prices could have been read).
func (t *Table) Age(now time.Time) time.Duration { return now.Sub(t.asOf) }

// Stale reports whether the table is older than StaleAfter at now.
func (t *Table) Stale(now time.Time) bool { return t.Age(now) > StaleAfter }

// StaleWarning is the warning every estimate carries when the table is stale,
// or "" when it isn't.
func (t *Table) StaleWarning(now time.Time) string {
	if !t.Stale(now) {
		return ""
	}
	return fmt.Sprintf("the price table is %d days old (as of %s; warns after %d days): check the Amazon Braket pricing page, or `puff doctor` for drift against the Price List",
		int(t.Age(now).Hours()/24), t.AsOf, int(StaleAfter.Hours()/24))
}
