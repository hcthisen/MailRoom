package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/core"
)

// audMaxSuggestions is how many tag names a tag input suggests.
const audMaxSuggestions = 500

// audTagNames returns existing tag names for the tag inputs' suggestions
// (list-tags). A failure only loses the suggestions.
func (u *UI) audTagNames(ctx context.Context) []string {
	ts, err := u.d.Contacts.ListTags(ctx, "", 0, audMaxSuggestions)
	if err != nil {
		u.log.WarnContext(ctx, "ui: tag suggestions", "err", err)
		return nil
	}
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Name
	}
	return names
}

// audStatusOptions are the contact statuses for filters and forms.
func audStatusOptions(any string) []Option {
	opts := []Option{}
	if any != "" {
		opts = append(opts, Option{Value: "", Label: any})
	}
	for _, s := range []core.ContactStatus{core.StatusActive, core.StatusPending, core.StatusUnsubscribed,
		core.StatusBounced, core.StatusComplained, core.StatusSuppressedManual} {
		opts = append(opts, Option{Value: string(s), Label: audStatusLabel(string(s))})
	}
	return opts
}

// audStatusLabel is the human name of a contact status.
func audStatusLabel(s string) string {
	switch core.ContactStatus(s) {
	case core.StatusActive:
		return "Active"
	case core.StatusPending:
		return "Pending (awaiting opt-in)"
	case core.StatusUnsubscribed:
		return "Unsubscribed"
	case core.StatusBounced:
		return "Bounced"
	case core.StatusComplained:
		return "Complained (spam report)"
	case core.StatusSuppressedManual:
		return "Suppressed (manual)"
	}
	return s
}

// audFormMessage is every error of f in one line (for a flash when the
// form isn't shown again).
func audFormMessage(f *Form) string {
	parts := []string{}
	if f.Message != "" && (len(f.Errors) == 0 || !strings.HasPrefix(f.Message, "Please correct")) {
		parts = append(parts, f.Message)
	}
	keys := make([]string, 0, len(f.Errors))
	for k := range f.Errors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+": "+f.Errors[k])
	}
	return strings.Join(parts, " ")
}

// audVersion reads the hidden version field (the If-Match of the API);
// missing or malformed is 0, which skips the check.
func audVersion(f *Form) int64 {
	v, _ := strconv.ParseInt(f.Get("version"), 10, 64)
	return max(v, 0)
}

// audParseDate reads a <input type="date"> value as the start of that day
// in the settings time zone. Empty is nil.
func (u *UI) audParseDate(field, s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, u.location())
	if err != nil {
		return nil, fieldError(field, "is not a date (YYYY-MM-DD)")
	}
	t = t.UTC()
	return &t, nil
}

// audBack returns the local return address of a form (field "back"), or
// def when it is missing or points outside the UI.
func audBack(f *Form, def string) string {
	if b := f.Get("back"); b != "" {
		return localNext(b)
	}
	return def
}

// ---------------------------------------------------------------- attributes

// Attributes are edited as "key: value" lines. Values are text, except
// true/false, plain numbers (e.g. 42, -3.5; not 007), null, JSON objects
// and arrays, and "quoted" text (for text that looks like one of those).
// audFormatAttributes writes them the same way, so an unchanged form
// round-trips every type exactly.

// audParseAttributes parses the attributes textarea.
func audParseAttributes(s string) (map[string]any, error) {
	out := map[string]any{}
	for i, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fieldError("attributes", fmt.Sprintf("line %d: write each attribute as key: value", i+1))
		}
		if _, dup := out[key]; dup {
			return nil, fieldError("attributes", fmt.Sprintf("line %d: %s is listed twice", i+1, key))
		}
		v, err := audParseValue(strings.TrimSpace(val))
		if err != nil {
			return nil, fieldError("attributes", fmt.Sprintf("line %d: %s", i+1, err.Error()))
		}
		out[key] = v
	}
	return out, nil
}

// audParseValue reads one attribute value.
func audParseValue(v string) (any, error) {
	switch {
	case v == "true":
		return true, nil
	case v == "false":
		return false, nil
	case v == "null":
		return nil, nil
	case strings.HasPrefix(v, `"`), strings.HasPrefix(v, "{"), strings.HasPrefix(v, "["):
		var x any
		if err := json.Unmarshal([]byte(v), &x); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON; quote text like \"this\"", v)
		}
		return x, nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && strconv.FormatFloat(f, 'f', -1, 64) == v {
		return f, nil
	}
	return v, nil
}

// audFormatValue writes one attribute value for the textarea.
func audFormatValue(v any) string {
	switch x := v.(type) {
	case string:
		p, err := audParseValue(x)
		if ps, isText := p.(string); err != nil || !isText || ps != x || strings.ContainsAny(x, "\r\n") || strings.TrimSpace(x) != x {
			b, _ := json.Marshal(x)
			return string(b)
		}
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// audFormatAttributes writes attributes as sorted "key: value" lines.
func audFormatAttributes(m map[string]any) string {
	var b strings.Builder
	for _, k := range audSortedKeys(m) {
		b.WriteString(k + ": " + audFormatValue(m[k]) + "\n")
	}
	return b.String()
}

func audSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// audKV is one attribute on the contact page.
type audKV struct {
	Key   string
	Value string
}

func audAttrList(m map[string]any) []audKV {
	out := make([]audKV, 0, len(m))
	for _, k := range audSortedKeys(m) {
		v := m[k]
		s, ok := v.(string)
		if !ok {
			s = audFormatValue(v)
		}
		out = append(out, audKV{Key: k, Value: s})
	}
	return out
}

// audRenameField moves a validation error of field from to the form input
// named to (two forms on one page can't share input names).
func audRenameField(err error, from, to string) error {
	var ve *core.ValidationError
	if errors.As(err, &ve) && strings.TrimPrefix(ve.Field, "body.") == from {
		return &core.ValidationError{Field: to, Message: ve.Message}
	}
	return err
}

// audTabs is the navigation between the sections under the Contacts nav
// item (contacts, tags, suppressions, imports), with cur highlighted. It
// is built here rather than in a shared template partial because every
// piece of it is a constant.
func audTabs(cur string) template.HTML {
	var b strings.Builder
	b.WriteString(`<nav class="subnav" aria-label="Contact sections">`)
	for _, t := range [][3]string{
		{"contacts", "Contacts", Prefix + "/contacts"},
		{"tags", "Tags", Prefix + "/tags"},
		{"suppressions", "Suppressions", Prefix + "/suppressions"},
		{"imports", "Import CSV", Prefix + "/imports"},
	} {
		if t[0] == cur {
			b.WriteString(`<a class="btn btn-sm btn-primary" aria-current="page" href="` + t[2] + `">` + t[1] + `</a>`)
		} else {
			b.WriteString(`<a class="btn btn-sm" href="` + t[2] + `">` + t[1] + `</a>`)
		}
	}
	b.WriteString(`</nav>`)
	return template.HTML(b.String())
}
