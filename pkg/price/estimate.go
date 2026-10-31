package price

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// errOverflow is returned when an estimate does not fit in int64 nano-dollars
// (about $9.2 billion): it is reported, never wrapped into a wrong figure.
var errOverflow = errors.New("the estimate is too large to compute exactly")

// mul multiplies non-negative a and b, failing on overflow.
func mul(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, errors.New("negative quantity in an estimate")
	}
	if a != 0 && b > math.MaxInt64/a {
		return 0, errOverflow
	}
	return a * b, nil
}

// add adds non-negative a and b, failing on overflow.
func add(a, b int64) (int64, error) {
	if a > math.MaxInt64-b {
		return 0, errOverflow
	}
	return a + b, nil
}

// Request is what to price: Programs circuits of Shots shots each, run as
// separate tasks or (Pack > 0, gate-based QPUs only) packed into program sets
// of up to Pack programs. Duration is a simulator's expected billed duration
// per task; zero means unknown.
type Request struct {
	Programs int
	Shots    int64
	Pack     int
	Duration time.Duration
}

// Estimate is the pricing-page arithmetic for a Request. Kind is always
// [Kind] ("estimate"): it is not a bill (#41).
type Estimate struct {
	Kind        string `json:"kind"`
	Device      string `json:"device"`
	DeviceArn   string `json:"device_arn"`
	PricesAsOf  string `json:"prices_as_of"`
	Programs    int    `json:"programs"`
	Shots       int64  `json:"shots_per_program"`
	Pack        int    `json:"pack,omitempty"`
	BilledTasks int    `json:"billed_tasks"`
	TotalShots  int64  `json:"total_shots"`

	PerTask   Nano `json:"-"`
	PerShot   Nano `json:"-"`
	PerMinute Nano `json:"-"`
	// BilledMillis is the per-task billed duration a simulator estimate used.
	BilledMillis int64 `json:"billed_ms_per_task,omitempty"`
	// Minimum is true when a simulator's duration was unknown, so the
	// estimate is the minimum charge: the real one can only be higher.
	Minimum bool `json:"minimum,omitempty"`

	// Unit prices, as decimal dollar strings, so the arithmetic can be
	// checked from the JSON alone.
	PerTaskUSD   string `json:"per_task_usd,omitempty"`
	PerShotUSD   string `json:"per_shot_usd,omitempty"`
	PerMinuteUSD string `json:"per_minute_usd,omitempty"`

	Total      Nano   `json:"-"`
	TotalUSD   string `json:"total_usd"`
	Arithmetic string `json:"arithmetic"`
}

// Estimate prices req on d.
func (t *Table) Estimate(d Device, req Request) (Estimate, error) {
	if req.Programs < 1 {
		return Estimate{}, errors.New("at least one program (--tasks) is needed")
	}
	if req.Pack < 0 {
		return Estimate{}, errors.New("--pack must not be negative")
	}
	if lo, hi := d.ShotsRange[0], d.ShotsRange[1]; req.Shots < lo || req.Shots > hi {
		return Estimate{}, fmt.Errorf("%s takes %d to %d shots per task (its capabilities' shotsRange); %d is outside it", d.Alias, lo, hi, req.Shots)
	}
	total, err := mul(int64(req.Programs), req.Shots)
	if err != nil {
		return Estimate{}, err
	}
	e := Estimate{Kind: Kind, Device: d.Alias, DeviceArn: d.Arn, PricesAsOf: t.AsOf,
		Programs: req.Programs, Shots: req.Shots, TotalShots: total}
	if d.Simulator() {
		err = t.simulator(d, req, &e)
	} else {
		err = t.qpu(d, req, &e)
	}
	if err != nil {
		return Estimate{}, err
	}
	e.TotalUSD = e.Total.String()
	if e.PerTask != 0 || e.PerShot != 0 {
		e.PerTaskUSD, e.PerShotUSD = e.PerTask.String(), e.PerShot.String()
	}
	if e.PerMinute != 0 {
		e.PerMinuteUSD = e.PerMinute.String()
	}
	return e, nil
}

func (t *Table) qpu(d Device, req Request, e *Estimate) error {
	if req.Duration != 0 {
		return fmt.Errorf("%s is billed per task and shot; --duration applies only to simulators", d.Alias)
	}
	e.PerTask, e.PerShot = t.perTask, d.perShot
	e.BilledTasks = req.Programs
	if req.Pack > 0 {
		ps := d.ProgramSet
		if ps == nil {
			return fmt.Errorf("%s does not run program sets (its capabilities have no program_set action), so --pack does not apply", d.Alias)
		}
		if req.Pack > ps.MaxExecutables {
			return fmt.Errorf("--pack %d exceeds %s's limit of %d programs per program set", req.Pack, d.Alias, ps.MaxExecutables)
		}
		e.Pack = req.Pack
		e.BilledTasks = req.Programs / req.Pack
		if req.Programs%req.Pack != 0 {
			e.BilledTasks++
		}
		largest := int64(min(req.Pack, req.Programs)) * req.Shots
		smallest := int64(req.Programs-(e.BilledTasks-1)*req.Pack) * req.Shots
		if largest > ps.MaxTotalShots {
			return fmt.Errorf("a program set of %d programs x %d shots is %d shots; %s allows at most %d per program set", min(req.Pack, req.Programs), req.Shots, largest, d.Alias, ps.MaxTotalShots)
		}
		if ps.MinTotalShots > 0 && smallest < ps.MinTotalShots {
			return fmt.Errorf("the last program set would have %d shots; %s needs at least %d per program set", smallest, d.Alias, ps.MinTotalShots)
		}
	}
	fees, err := mul(int64(e.BilledTasks), int64(e.PerTask))
	if err != nil {
		return err
	}
	shots, err := mul(e.TotalShots, int64(e.PerShot))
	if err != nil {
		return err
	}
	sum, err := add(fees, shots)
	if err != nil {
		return err
	}
	e.Total = Nano(sum)
	switch {
	case e.BilledTasks == 1 && req.Programs == 1:
		e.Arithmetic = fmt.Sprintf("%s + %d x %s = %s", e.PerTask, req.Shots, e.PerShot, e.Total)
	case req.Pack > 0:
		e.Arithmetic = fmt.Sprintf("%d program sets x %s + %d programs x %d shots x %s = %s",
			e.BilledTasks, e.PerTask, req.Programs, req.Shots, e.PerShot, e.Total)
	default:
		e.Arithmetic = fmt.Sprintf("%d tasks x (%s + %d x %s) = %s", e.BilledTasks, e.PerTask, req.Shots, e.PerShot, e.Total)
	}
	return nil
}

func (t *Table) simulator(d Device, req Request, e *Estimate) error {
	if req.Pack > 0 {
		return fmt.Errorf("%s does not run program sets (#2), so --pack does not apply", d.Alias)
	}
	if req.Duration < 0 {
		return errors.New("--duration must not be negative")
	}
	if req.Duration > 30*24*time.Hour {
		return errors.New("--duration over 30 days is not a simulator task")
	}
	e.PerMinute = d.perMinute
	e.BilledTasks = req.Programs
	minMs := int64(d.MinimumSeconds) * 1000
	// Billed per millisecond, rounded up, with a minimum per task.
	ms := (req.Duration.Nanoseconds() + 999_999) / 1_000_000
	what := fmt.Sprintf("%d s minimum", d.MinimumSeconds)
	switch {
	case req.Duration == 0:
		e.Minimum = true
		ms = minMs
	case ms < minMs:
		what = fmt.Sprintf("%s billed as the %d s minimum", req.Duration, d.MinimumSeconds)
		ms = minMs
	default:
		what = req.Duration.String()
	}
	e.BilledMillis = ms
	// Per task: per-minute rate x ms / 60000, rounded up to the nano-dollar
	// (the pricing page doesn't say how a partial unit rounds; up keeps the
	// estimate from falling below the charge). Then times the task count.
	num, err := mul(int64(d.perMinute), ms)
	if err != nil {
		return err
	}
	perTask := (num + 59_999) / 60_000
	total, err := mul(perTask, int64(req.Programs))
	if err != nil {
		return err
	}
	e.Total = Nano(total)
	tasks := ""
	if req.Programs > 1 {
		tasks = fmt.Sprintf("%d tasks x ", req.Programs)
	}
	e.Arithmetic = fmt.Sprintf("%s%s x %s/min = %s", tasks, what, e.PerMinute, e.Total)
	if e.Minimum {
		e.Arithmetic += " (at least: the billed duration is not known before the run; pass --duration to estimate it)"
	}
	return nil
}
