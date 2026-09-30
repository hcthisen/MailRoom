package settings

import (
	"fmt"
	"slices"
	"time"

	"mailroom/internal/core"
)

// SendWindow restricts sending to certain weekdays and hours in the
// install's time zone (S-5), e.g. Mon-Fri 08:00-20:00:
//
//	{"days": [1,2,3,4,5], "start": "08:00", "end": "20:00"}
//
// Days are ISO weekdays: 1 = Monday ... 7 = Sunday. The window is
// [start, end) on each listed day; start must be before end.
type SendWindow struct {
	_     struct{} `nullable:"true"`
	Days  []int    `json:"days" minItems:"1" maxItems:"7" uniqueItems:"true" doc:"ISO weekdays: 1 = Monday ... 7 = Sunday" example:"[1,2,3,4,5]"`
	Start string   `json:"start" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"Opening time, HH:MM (24 h)" example:"08:00"`
	End   string   `json:"end" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"Closing time, HH:MM (24 h), after start" example:"20:00"`
}

func (w SendWindow) clone() SendWindow {
	w.Days = slices.Clone(w.Days)
	return w
}

// Validate checks the window; field names are prefixed with field.
func (w SendWindow) Validate(field string) error {
	if len(w.Days) == 0 {
		return core.Invalid(field+".days", "must list at least one weekday")
	}
	seen := map[int]bool{}
	for _, d := range w.Days {
		if d < 1 || d > 7 {
			return core.Invalid(field+".days", "must be ISO weekdays 1 (Monday) to 7 (Sunday)")
		}
		if seen[d] {
			return core.Invalid(field+".days", "lists weekday %d twice", d)
		}
		seen[d] = true
	}
	start, err := parseClock(w.Start)
	if err != nil {
		return core.Invalid(field+".start", "must be HH:MM")
	}
	end, err := parseClock(w.End)
	if err != nil {
		return core.Invalid(field+".end", "must be HH:MM")
	}
	if end <= start {
		return core.Invalid(field+".end", "must be after start")
	}
	return nil
}

// parseClock parses "HH:MM" into minutes after midnight.
func parseClock(s string) (int, error) {
	var h, m int
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("bad clock %q", s)
	}
	if _, err := fmt.Sscanf(s, "%02d:%02d", &h, &m); err != nil || h > 23 || m > 59 || h < 0 || m < 0 {
		return 0, fmt.Errorf("bad clock %q", s)
	}
	return h*60 + m, nil
}

func isoWeekday(t time.Time) int {
	if wd := int(t.Weekday()); wd != 0 {
		return wd
	}
	return 7
}

// Contains reports whether t falls inside the window in loc. An invalid
// window contains nothing.
func (w SendWindow) Contains(t time.Time, loc *time.Location) bool {
	start, err1 := parseClock(w.Start)
	end, err2 := parseClock(w.End)
	if err1 != nil || err2 != nil {
		return false
	}
	local := t.In(loc)
	if !slices.Contains(w.Days, isoWeekday(local)) {
		return false
	}
	min := local.Hour()*60 + local.Minute()
	return min >= start && min < end
}

// NextOpen returns t if it is inside the window, else the next time the
// window opens (in UTC). An invalid window returns t unchanged.
func (w SendWindow) NextOpen(t time.Time, loc *time.Location) time.Time {
	if w.Validate("window") != nil || w.Contains(t, loc) {
		return t
	}
	start, _ := parseClock(w.Start)
	local := t.In(loc)
	for i := 0; i <= 7; i++ {
		day := local.AddDate(0, 0, i)
		if !slices.Contains(w.Days, isoWeekday(day)) {
			continue
		}
		open := time.Date(day.Year(), day.Month(), day.Day(), start/60, start%60, 0, 0, loc)
		if !open.Before(t) {
			return open.UTC()
		}
	}
	return t
}
