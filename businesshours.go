package concierge

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidBusinessHours refuses business hours that cannot be counted: a time
// zone the tz database does not name, a window that is empty, out of the day's
// bounds, overlapping or out of order, or a holiday that is not a YYYY-MM-DD
// date.
var ErrInvalidBusinessHours = errors.New("concierge: invalid business hours")

// ErrSpanTooLong refuses an Elapsed whose span covers more days than
// maxBusinessDays, so a measurement over a ticket left open for decades is an
// error rather than a walk of every day in between.
var ErrSpanTooLong = errors.New("concierge: span too long to count in business hours")

// maxBusinessDays bounds the days Elapsed walks: ten years.
const maxBusinessDays = 3660

// minutesPerDay is the end of the last window a day can hold.
const minutesPerDay = 24 * 60

// Window is one open interval of a day, in minutes from local midnight:
// 0 <= Start < End <= 1440.
type Window struct {
	Start int
	End   int
}

// BusinessHours is when a desk's clock runs: per weekday (indexed by
// time.Weekday) the open windows in TimeZone's local time, less Holidays (local
// dates, YYYY-MM-DD). Windows follow the local clock, so a day on which daylight
// saving starts or ends is counted at its real length. Hours with no window on
// any day are open around the clock, holidays included; an empty TimeZone is
// UTC.
type BusinessHours struct {
	TimeZone string
	Week     [7][]Window
	Holidays []string
}

// AlwaysOpen reports whether no window is set, so every instant is business
// time.
func (h BusinessHours) AlwaysOpen() bool {
	for _, day := range h.Week {
		if len(day) > 0 {
			return false
		}
	}
	return true
}

// Validate reports why h cannot be counted, wrapping ErrInvalidBusinessHours,
// or nil.
func (h BusinessHours) Validate() error {
	_, _, err := h.compile()
	return err
}

// compile resolves the time zone and the holiday set, checking every window.
func (h BusinessHours) compile() (*time.Location, map[string]bool, error) {
	loc := time.UTC
	if h.TimeZone != "" {
		l, err := time.LoadLocation(h.TimeZone)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: time zone %q: %v", ErrInvalidBusinessHours, h.TimeZone, err)
		}
		loc = l
	}
	for d, day := range h.Week {
		last := -1
		for _, w := range day {
			if w.Start < 0 || w.End > minutesPerDay || w.Start >= w.End {
				return nil, nil, fmt.Errorf("%w: %s window %d-%d is not inside the day", ErrInvalidBusinessHours, time.Weekday(d), w.Start, w.End)
			}
			if w.Start < last {
				return nil, nil, fmt.Errorf("%w: %s windows overlap or are out of order", ErrInvalidBusinessHours, time.Weekday(d))
			}
			last = w.End
		}
	}
	holidays := make(map[string]bool, len(h.Holidays))
	for _, s := range h.Holidays {
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			return nil, nil, fmt.Errorf("%w: holiday %q is not a YYYY-MM-DD date", ErrInvalidBusinessHours, s)
		}
		holidays[s] = true
	}
	return loc, holidays, nil
}

// Elapsed is the business time between from and to: zero when to is not after
// from, the plain difference when h is always open, and otherwise the sum of
// each local day's windows clipped to [from, to], holidays skipped.
func (h BusinessHours) Elapsed(from, to time.Time) (time.Duration, error) {
	loc, holidays, err := h.compile()
	if err != nil {
		return 0, err
	}
	if !to.After(from) {
		return 0, nil
	}
	if h.AlwaysOpen() {
		return to.Sub(from), nil
	}
	lf, lt := from.In(loc), to.In(loc)
	day := time.Date(lf.Year(), lf.Month(), lf.Day(), 0, 0, 0, 0, loc)
	last := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	var total time.Duration
	for n := 0; !day.After(last); n++ {
		if n > maxBusinessDays {
			return 0, ErrSpanTooLong
		}
		if !holidays[day.Format(time.DateOnly)] {
			for _, w := range h.Week[day.Weekday()] {
				start := time.Date(day.Year(), day.Month(), day.Day(), 0, w.Start, 0, 0, loc)
				end := time.Date(day.Year(), day.Month(), day.Day(), 0, w.End, 0, 0, loc)
				if start.Before(from) {
					start = from
				}
				if end.After(to) {
					end = to
				}
				if end.After(start) {
					total += end.Sub(start)
				}
			}
		}
		day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
	}
	return total, nil
}
