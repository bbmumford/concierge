package concierge

import (
	"errors"
	"testing"
	"time"
)

func melbourne(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Australia/Melbourne")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	return loc
}

// officeHours is Monday to Friday, 09:00 to 17:00, in Melbourne.
func officeHours() BusinessHours {
	day := []Window{{Start: 9 * 60, End: 17 * 60}}
	var h BusinessHours
	h.TimeZone = "Australia/Melbourne"
	for d := time.Monday; d <= time.Friday; d++ {
		h.Week[d] = day
	}
	return h
}

func TestBusinessHours_ZeroIsAroundTheClock(t *testing.T) {
	var h BusinessHours
	from := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	got, err := h.Elapsed(from, from.Add(70*time.Hour))
	if err != nil || got != 70*time.Hour {
		t.Fatalf("Elapsed = %v, %v; want 70h", got, err)
	}
	if !h.AlwaysOpen() {
		t.Fatal("zero hours should be always open")
	}
}

func TestBusinessHours_CountsOnlyOpenWindows(t *testing.T) {
	loc := melbourne(t)
	h := officeHours()
	// Friday 16:00 to Monday 10:00: one hour on Friday, one on Monday.
	from := time.Date(2026, 11, 27, 16, 0, 0, 0, loc)
	to := time.Date(2026, 11, 30, 10, 0, 0, 0, loc)
	got, err := h.Elapsed(from, to)
	if err != nil || got != 2*time.Hour {
		t.Fatalf("Elapsed = %v, %v; want 2h", got, err)
	}
	// A span inside closed time is nothing.
	sat := time.Date(2026, 11, 28, 10, 0, 0, 0, loc)
	if got, _ := h.Elapsed(sat, sat.Add(5*time.Hour)); got != 0 {
		t.Fatalf("weekend Elapsed = %v; want 0", got)
	}
	// A reversed span is nothing, not negative.
	if got, _ := h.Elapsed(to, from); got != 0 {
		t.Fatalf("reversed Elapsed = %v; want 0", got)
	}
}

func TestBusinessHours_HolidayHasNoBusinessTime(t *testing.T) {
	loc := melbourne(t)
	h := officeHours()
	h.Holidays = []string{"2026-11-30"}
	from := time.Date(2026, 11, 27, 16, 0, 0, 0, loc)
	to := time.Date(2026, 12, 1, 10, 0, 0, 0, loc)
	got, err := h.Elapsed(from, to)
	if err != nil || got != 2*time.Hour {
		t.Fatalf("Elapsed = %v, %v; want 2h (Friday and Tuesday, Monday a holiday)", got, err)
	}
}

func TestBusinessHours_WindowsFollowTheLocalClockAcrossDaylightSaving(t *testing.T) {
	loc := melbourne(t)
	var h BusinessHours
	h.TimeZone = "Australia/Melbourne"
	h.Week[time.Sunday] = []Window{{Start: 0, End: 24 * 60}}
	// Daylight saving starts in Melbourne on Sunday 4 October 2026: that day
	// has 23 hours.
	day := time.Date(2026, 10, 4, 0, 0, 0, 0, loc)
	got, err := h.Elapsed(day.Add(-time.Hour), day.Add(48*time.Hour))
	if err != nil || got != 23*time.Hour {
		t.Fatalf("Elapsed = %v, %v; want 23h", got, err)
	}
}

func TestBusinessHours_ValidateRefusesWhatCannotBeCounted(t *testing.T) {
	cases := map[string]BusinessHours{
		"unknown zone":   {TimeZone: "Nowhere/Never"},
		"bad holiday":    {Holidays: []string{"30/11/2026"}},
		"empty window":   func() BusinessHours { var h BusinessHours; h.Week[1] = []Window{{Start: 600, End: 600}}; return h }(),
		"past midnight":  func() BusinessHours { var h BusinessHours; h.Week[1] = []Window{{Start: 600, End: 1441}}; return h }(),
		"negative start": func() BusinessHours { var h BusinessHours; h.Week[1] = []Window{{Start: -1, End: 60}}; return h }(),
		"overlap":        func() BusinessHours { var h BusinessHours; h.Week[1] = []Window{{0, 600}, {540, 700}}; return h }(),
		"out of order":   func() BusinessHours { var h BusinessHours; h.Week[1] = []Window{{700, 800}, {0, 600}}; return h }(),
	}
	for name, h := range cases {
		if err := h.Validate(); !errors.Is(err, ErrInvalidBusinessHours) {
			t.Errorf("%s: Validate = %v; want ErrInvalidBusinessHours", name, err)
		}
		if _, err := h.Elapsed(time.Unix(0, 0), time.Unix(3600, 0)); !errors.Is(err, ErrInvalidBusinessHours) {
			t.Errorf("%s: Elapsed = %v; want ErrInvalidBusinessHours", name, err)
		}
	}
	if err := officeHours().Validate(); err != nil {
		t.Fatalf("office hours: %v", err)
	}
}

func TestBusinessHours_RefusesASpanTooLongToWalk(t *testing.T) {
	h := officeHours()
	from := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := h.Elapsed(from, from.AddDate(30, 0, 0)); !errors.Is(err, ErrSpanTooLong) {
		t.Fatalf("Elapsed over 30 years = %v; want ErrSpanTooLong", err)
	}
}

func TestEvaluateSLA_BusinessTimeLessPauses(t *testing.T) {
	loc := melbourne(t)
	p := &SLAPolicy{Slug: "standard", FirstResponse: 2 * time.Hour, Resolution: 8 * time.Hour, Hours: officeHours()}
	start := time.Date(2026, 11, 27, 16, 0, 0, 0, loc) // Friday
	s := &TicketSLA{StartedAt: start}
	tk := &Ticket{}

	at := func(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, loc) }
	got, err := EvaluateSLA(p, s, tk, at(30, 9, 30))
	if err != nil || got.FirstResponseElapsed != 90*time.Minute || got.FirstResponseBreached {
		t.Fatalf("Monday 09:30: %+v, %v; want 90m, not breached", got, err)
	}
	got, _ = EvaluateSLA(p, s, tk, at(30, 10, 30))
	if !got.FirstResponseBreached || got.ResolutionBreached {
		t.Fatalf("Monday 10:30: %+v; want first response breached, resolution not", got)
	}

	// Paused from Friday 16:30 to Monday 10:00: half an hour each side.
	resumed := at(30, 10, 0)
	s.Pauses = []SLAPause{{From: at(27, 16, 30), To: &resumed}}
	got, _ = EvaluateSLA(p, s, tk, at(30, 10, 30))
	if got.FirstResponseElapsed != time.Hour || got.FirstResponseBreached || got.Paused {
		t.Fatalf("paused: %+v; want 1h, not breached, not paused", got)
	}
	s.Pauses = append(s.Pauses, SLAPause{From: at(30, 10, 30)})
	got, _ = EvaluateSLA(p, s, tk, at(30, 15, 0))
	if got.FirstResponseElapsed != time.Hour || !got.Paused {
		t.Fatalf("running pause: %+v; want 1h and paused", got)
	}

	// A stamped first response is measured to its stamp, whatever now is.
	answered := at(27, 16, 45)
	tk.FirstResponseAt = &answered
	s.Pauses = nil
	got, _ = EvaluateSLA(p, s, tk, at(30, 15, 0))
	if got.FirstResponseElapsed != 45*time.Minute || got.FirstResponseBreached {
		t.Fatalf("answered: %+v; want 45m, not breached", got)
	}

	// A zero target is never breached.
	p.Resolution = 0
	got, _ = EvaluateSLA(p, s, tk, at(30, 15, 0).AddDate(0, 1, 0))
	if got.ResolutionBreached {
		t.Fatalf("zero resolution target breached: %+v", got)
	}
	if _, err := EvaluateSLA(nil, s, tk, start); !errors.Is(err, ErrSLANotStarted) {
		t.Fatalf("nil policy: %v; want ErrSLANotStarted", err)
	}
}
