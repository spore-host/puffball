package devices

import (
	"fmt"
	"time"
)

// dayMatches reports whether a window's executionDay covers weekday wd.
// The values are amazon-braket-schemas-python's ExecutionDay (commit 8aa7ca9,
// src/braket/device_schema/device_execution_window.py): Everyday, Weekdays,
// Weekend ("only on the weekends") and the weekday names. The schema also
// allows any other string; that is unknown, and ok is false rather than a guess.
func dayMatches(day string, wd time.Weekday) (match, ok bool) {
	switch day {
	case "Everyday":
		return true, true
	case "Weekdays":
		return wd >= time.Monday && wd <= time.Friday, true
	case "Weekend":
		return wd == time.Saturday || wd == time.Sunday, true
	}
	for d := time.Sunday; d <= time.Saturday; d++ {
		if day == d.String() {
			return d == wd, true
		}
	}
	return false, false
}

// clock parses a window time. The schema types it as a time, "UTC 24-hour
// format"; G0's devices send "HH:MM:SS" and the schema's example "HH:MM".
func clock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04:05", s)
	if err != nil {
		if t, err = time.Parse("15:04", s); err != nil {
			return 0, fmt.Errorf("window time %q: %w", s, err)
		}
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second, nil
}

// WindowState says whether the device is inside an execution window at now,
// and if not, when the next one opens. Known is false when a window's day or
// time couldn't be read, so the state isn't claimed. A device that reports
// no windows has Known false too.
type WindowState struct {
	Known bool
	Open  bool
	Next  time.Time // the next opening, when !Open
}

// WindowAt evaluates d's windows at now (UTC; window end is inclusive to the
// second, as in "23:59:59").
func (d Device) WindowAt(now time.Time) WindowState {
	if len(d.Windows) == 0 {
		return WindowState{}
	}
	now = now.UTC()
	day0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var next time.Time
	for offset := 0; offset <= 7; offset++ {
		day := day0.AddDate(0, 0, offset)
		for _, w := range d.Windows {
			match, ok := dayMatches(w.Day, day.Weekday())
			if !ok {
				return WindowState{}
			}
			start, err1 := clock(w.Start)
			end, err2 := clock(w.End)
			// A window that wraps past midnight (end before start) was never
			// measured; its state isn't claimed.
			if err1 != nil || err2 != nil || end < start {
				return WindowState{}
			}
			if !match {
				continue
			}
			// The end is inclusive to the second: "23:59:59" (measured) runs to
			// midnight; an "HH:MM" end (schema example, not measured) is read
			// as HH:MM:00, so "19:00" includes 19:00:00 and nothing after.
			from, to := day.Add(start), day.Add(end+time.Second)
			if offset == 0 && !now.Before(from) && now.Before(to) {
				return WindowState{Known: true, Open: true}
			}
			if from.After(now) && (next.IsZero() || from.Before(next)) {
				next = from
			}
		}
		if !next.IsZero() {
			return WindowState{Known: true, Next: next}
		}
	}
	return WindowState{Known: true}
}
