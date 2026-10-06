package ui

// Shared helpers of the "automation" page group: sequences
// (automation_sequences.go, automation_steps.go, automation_enrollments.go,
// automation_yaml.go) and sign-up forms (automation_forms.go).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"mailroom/internal/core"
	"mailroom/internal/settings"
)

// autoTagLimit bounds the tag names offered as suggestions in tag inputs.
const autoTagLimit = 500

// autoRetarget renames the field of a validation error, so it lands next
// to the input that holds the value (e.g. "email.subject" -> "subject",
// "emails[3]" -> "emails"). rename gets the service's field and message
// and returns the ones to show. Other errors are returned unchanged.
func autoRetarget(err error, rename func(field, msg string) (string, string)) error {
	var ve *core.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	field, msg := rename(ve.Field, ve.Message)
	return core.Invalid(field, "%s", msg)
}

// autoIndexed matches an indexed field such as "emails[3]" or
// "fields[0].key": the list name, the index and the rest.
var autoIndexed = regexp.MustCompile(`^([a-z_]+)\[(\d+)\](.*)$`)

// autoFoldIndex renames "list[i]..." to "list" and puts the i-th value
// (when known) in front of the message, for lists edited in one input.
func autoFoldIndex(list string, values []string) func(field, msg string) (string, string) {
	return func(field, msg string) (string, string) {
		m := autoIndexed.FindStringSubmatch(field)
		if m == nil || m[1] != list {
			return field, msg
		}
		if i, err := strconv.Atoi(m[2]); err == nil && i < len(values) {
			return list, fmt.Sprintf("%q %s", values[i], msg)
		}
		return list, msg
	}
}

// autoSplitList splits a list typed into one input (commas, spaces,
// semicolons or line breaks) into its non-empty entries.
func autoSplitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
}

// autoVersion reads the hidden "version" field an edit form carries, the
// If-Match of the change (0 = don't check).
func autoVersion(f *Form) int64 {
	v, err := strconv.ParseInt(f.Get("version"), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// autoTagSuggestions returns existing tag names for a tag input's
// suggestions (list-tags). A failure only loses the suggestions.
func (u *UI) autoTagSuggestions(ctx context.Context) []string {
	if u.d.Contacts == nil {
		return nil
	}
	tags, err := u.d.Contacts.ListTags(ctx, "", 0, autoTagLimit)
	if err != nil {
		u.log.WarnContext(ctx, "ui: tag suggestions", "err", err)
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, t.Name)
	}
	return out
}

// autoWeekdays are the ISO weekdays of a send window, for checkboxes.
var autoWeekdays = []Option{
	{"1", "Mon"}, {"2", "Tue"}, {"3", "Wed"}, {"4", "Thu"}, {"5", "Fri"}, {"6", "Sat"}, {"7", "Sun"},
}

// autoWindowText describes a send window ("Mon–Fri, 08:00–20:00");
// nil is "".
func autoWindowText(w *settings.SendWindow) string {
	if w == nil {
		return ""
	}
	days := make([]bool, 8)
	for _, d := range w.Days {
		if d >= 1 && d <= 7 {
			days[d] = true
		}
	}
	var parts []string
	for d := 1; d <= 7; {
		if !days[d] {
			d++
			continue
		}
		start := d
		for d <= 7 && days[d] {
			d++
		}
		end := d - 1
		switch {
		case start == end:
			parts = append(parts, autoWeekdays[start-1].Label)
		case end == start+1:
			parts = append(parts, autoWeekdays[start-1].Label, autoWeekdays[end-1].Label)
		default:
			parts = append(parts, autoWeekdays[start-1].Label+"–"+autoWeekdays[end-1].Label)
		}
	}
	return strings.Join(parts, ", ") + ", " + w.Start + "–" + w.End
}

// autoDefaultWindow describes the settings' default send window
// (get-settings), for sequences without one of their own.
func (u *UI) autoDefaultWindow(ctx context.Context) string {
	if u.d.Settings == nil {
		return ""
	}
	st, err := u.d.Settings.Get(ctx)
	if err != nil {
		return ""
	}
	return autoWindowText(st.DefaultSendWindow)
}

// Delay units of the step form.
var autoDelayUnits = []struct {
	Value   string
	Label   string
	Seconds int64
}{{"d", "days", 86400}, {"h", "hours", 3600}, {"m", "minutes", 60}, {"s", "seconds", 1}}

// autoDelayOptions are the unit choices; seconds only when the current
// value needs them (a delay imported from YAML such as "45s").
func autoDelayOptions(unit string) []Option {
	out := []Option{{"m", "minutes"}, {"h", "hours"}, {"d", "days"}}
	if unit == "s" {
		out = append(out, Option{"s", "seconds"})
	}
	return out
}

// autoSplitDelay writes seconds as a number and the largest unit that
// divides it exactly.
func autoSplitDelay(seconds int64) (string, string) {
	if seconds <= 0 {
		return "0", "d"
	}
	for _, un := range autoDelayUnits {
		if seconds%un.Seconds == 0 {
			return strconv.FormatInt(seconds/un.Seconds, 10), un.Value
		}
	}
	return strconv.FormatInt(seconds, 10), "s"
}

// autoParseDelay reads the delay_value and delay_unit fields as seconds.
// A malformed number is an error on delay_value (the service checks the
// range).
func autoParseDelay(f *Form) (int64, bool) {
	n, err := strconv.ParseInt(f.Get("delay_value"), 10, 64)
	if err != nil || n < 0 {
		f.AddError("delay_value", "must be a whole number of 0 or more")
		return 0, false
	}
	for _, un := range autoDelayUnits {
		if un.Value == f.Get("delay_unit") {
			if n > (1<<62)/un.Seconds {
				f.AddError("delay_value", "is too large")
				return 0, false
			}
			return n * un.Seconds, true
		}
	}
	f.AddError("delay_unit", "choose minutes, hours or days")
	return 0, false
}

// autoDelayText is a delay for people: "immediately", "2 days", "36 hours".
func autoDelayText(seconds int64) string {
	if seconds <= 0 {
		return "immediately"
	}
	v, unit := autoSplitDelay(seconds)
	n, _ := strconv.Atoi(v)
	for _, un := range autoDelayUnits {
		if un.Value == unit {
			return plural(n, strings.TrimSuffix(un.Label, "s"), un.Label)
		}
	}
	return v + " s"
}
