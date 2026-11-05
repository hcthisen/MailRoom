package ui

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NavItem is one entry of the left navigation.
type NavItem struct {
	Key   string // View.Nav value that highlights it
	Label string
	URL   string
	// Count is a number to show with the item (the Inbox's unread
	// messages); 0 = none. The layout shows it as a badge.
	Count int
}

// navItems is the left navigation (PRD A-2), in order.
var navItems = []NavItem{
	{Key: "dashboard", Label: "Dashboard", URL: Prefix + "/"},
	{Key: "inbox", Label: "Inbox", URL: Prefix + "/inbox"},
	{Key: "contacts", Label: "Contacts", URL: Prefix + "/contacts"},
	{Key: "segments", Label: "Segments", URL: Prefix + "/segments"},
	{Key: "broadcasts", Label: "Broadcasts", URL: Prefix + "/broadcasts"},
	{Key: "sequences", Label: "Sequences", URL: Prefix + "/sequences"},
	{Key: "forms", Label: "Forms", URL: Prefix + "/forms"},
	{Key: "content", Label: "Emails", URL: Prefix + "/emails"},
	{Key: "settings", Label: "Settings", URL: Prefix + "/settings"},
	{Key: "health", Label: "Health", URL: Prefix + "/health"},
	{Key: "tokens", Label: "API tokens", URL: Prefix + "/tokens"},
}

// nav returns the navigation with the Inbox's unread count, or without
// the Inbox when the UI has none.
func (u *UI) nav() []NavItem {
	out := make([]NavItem, 0, len(navItems))
	for _, it := range navItems {
		if it.Key == "inbox" {
			if u.d.Inbound == nil {
				continue
			}
			if c, err := u.d.Inbound.Counts(context.Background()); err == nil && c.Unread > 0 {
				it.Count = c.Unread
			}
		}
		out = append(out, it)
	}
	return out
}

// funcs are the template functions every template can use.
func (u *UI) funcs() template.FuncMap {
	return template.FuncMap{
		"static":        u.staticURL,
		"navItems":      u.nav,
		"fmtTime":       u.fmtTime,
		"fmtDate":       u.fmtDate,
		"fmtInputTime":  u.fmtInputTime,
		"ago":           u.ago,
		"num":           formatNumber,
		"pct":           formatPercent,
		"bytes":         formatBytes,
		"badge":         Badge,
		"dict":          dict,
		"list":          func(v ...any) []any { return v },
		"join":          strings.Join,
		"truncate":      truncate,
		"tagsValue":     func(tags []string) string { return strings.Join(tags, ", ") },
		"plural":        plural,
		"timezone":      func() string { return u.location().String() },
		"defaultString": func(def, s string) string { return defaultString(s, def) },
		"checkLabel":    checkLabel,
		"timelineLabel": timelineLabel,
		"sesReady":      u.sesReady,
		"hasPrefix":     strings.HasPrefix,
		"mergeChips":    mergeChips,
		"reSubject":     reSubject,
		"fieldName":     fieldName,
		"trimPrefix":    strings.TrimPrefix,
	}
}

// location is the settings time zone (UTC when unavailable). The settings
// service caches the document, so this is cheap.
func (u *UI) location() *time.Location {
	if u.d.Settings == nil {
		return time.UTC
	}
	st, err := u.d.Settings.Get(context.Background())
	if err != nil {
		return time.UTC
	}
	return st.Location()
}

// asTime accepts time.Time and *time.Time; ok is false for nil and zero.
func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case *time.Time:
		if t == nil {
			return time.Time{}, false
		}
		return *t, !t.IsZero()
	}
	return time.Time{}, false
}

// fmtTime formats a time in the settings time zone ("5 Jan 2026, 09:00");
// zero and nil times are "—".
func (u *UI) fmtTime(v any) string {
	t, ok := asTime(v)
	if !ok {
		return "—"
	}
	return t.In(u.location()).Format("2 Jan 2006, 15:04")
}

// fmtDate formats the date part in the settings time zone ("5 Jan 2026").
func (u *UI) fmtDate(v any) string {
	t, ok := asTime(v)
	if !ok {
		return "—"
	}
	return t.In(u.location()).Format("2 Jan 2006")
}

// fmtInputTime formats a time for <input type="datetime-local"> in the
// settings time zone; ParseInputTime reads it back.
func (u *UI) fmtInputTime(v any) string {
	t, ok := asTime(v)
	if !ok {
		return ""
	}
	return t.In(u.location()).Format(InputTimeLayout)
}

// InputTimeLayout is the value format of <input type="datetime-local">.
const InputTimeLayout = "2006-01-02T15:04"

// ParseInputTime parses a datetime-local value in the settings time zone.
// An empty value is the zero time.
func (u *UI) ParseInputTime(field, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation(InputTimeLayout, s, u.location())
	if err != nil {
		return time.Time{}, fieldError(field, "is not a date and time")
	}
	return t.UTC(), nil
}

// ago is a short relative time ("3 min ago", "in 2 h"), falling back to
// the date after a week.
func (u *UI) ago(v any) string {
	t, ok := asTime(v)
	if !ok {
		return "—"
	}
	d := u.clock.Now().Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	var s string
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		s = fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 24*time.Hour:
		s = fmt.Sprintf("%d h", int(d/time.Hour))
	case d < 7*24*time.Hour:
		s = plural(int(d/(24*time.Hour)), "day", "days")
	default:
		return u.fmtDate(t)
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

// plural returns "1 contact" / "3 contacts".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return formatNumber(n) + " " + many
}

// formatNumber formats an integer with thousands separators ("12,345").
func formatNumber(v any) string {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	case int32:
		n = int64(x)
	case uint64:
		n = int64(x)
	case float64:
		n = int64(math.Round(x))
	default:
		return fmt.Sprint(v)
	}
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// formatPercent formats a fraction as a percentage with up to two
// decimals (0.0025 -> "0.25%").
func formatPercent(f float64) string {
	s := strconv.FormatFloat(f*100, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "%"
}

// formatBytes formats a size ("1.2 MB").
func formatBytes(v any) string {
	var n float64
	switch x := v.(type) {
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	case uint64:
		n = float64(x)
	default:
		return fmt.Sprint(v)
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1000 && i < len(units)-1 {
		n /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", int64(n))
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

// dict builds a map from key/value pairs, for passing several values to a
// component template: {{template "field" (dict "Form" .Data.Form "Name" "email" "Label" "Email")}}.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// truncate shortens s to n characters with an ellipsis.
func truncate(n int, s string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func defaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Badge tones.
const (
	toneOK     = "ok"
	toneInfo   = "info"
	toneWarn   = "warn"
	toneDanger = "danger"
	toneMuted  = "muted"
)

// badgeTones maps kind -> status -> tone. Kind "" is the fallback for
// any status list (jobs, enrollments, doctor checks, tokens...).
var badgeTones = map[string]map[string]string{
	"contact": {
		"active": toneOK, "pending": toneInfo, "unsubscribed": toneMuted,
		"bounced": toneDanger, "complained": toneDanger, "suppressed_manual": toneWarn,
	},
	"message": {
		"queued": toneInfo, "sending": toneInfo, "sent": toneOK, "delivered": toneOK,
		"bounced": toneDanger, "complained": toneDanger, "failed": toneDanger, "skipped": toneMuted,
	},
	"broadcast": {
		"draft": toneMuted, "scheduled": toneInfo, "sending": toneInfo, "paused": toneWarn,
		"sent": toneOK, "cancelled": toneMuted,
	},
	"": {
		"ok": toneOK, "active": toneOK, "succeeded": toneOK, "completed": toneOK, "enabled": toneOK, "healthy": toneOK,
		"delivered": toneOK, "sent": toneOK, "confirmed": toneOK, "on": toneOK,
		"queued": toneInfo, "running": toneInfo, "pending": toneInfo, "scheduled": toneInfo, "sending": toneInfo,
		"warn": toneWarn, "paused": toneWarn, "probation": toneWarn, "expiring": toneWarn,
		"fail": toneDanger, "failed": toneDanger, "error": toneDanger, "revoked": toneDanger, "shutdown": toneDanger,
		"bounced": toneDanger, "complained": toneDanger, "disabled": toneDanger,
		"skipped": toneMuted, "cancelled": toneMuted, "exited": toneMuted, "draft": toneMuted, "expired": toneMuted,
		"off": toneMuted, "unsubscribed": toneMuted, "inactive": toneMuted,
		"new": toneInfo, "replied": toneOK, "spam": toneDanger,
	},
}

// badgeLabels are statuses shown with a different label.
var badgeLabels = map[string]string{"suppressed_manual": "suppressed"}

// Badge renders a status badge: kind is "contact", "message",
// "broadcast" or "" (generic: jobs, enrollments, doctor checks, ...).
// Unknown statuses get the muted tone. The text is escaped.
func Badge(kind, status string) template.HTML {
	tone := toneMuted
	if t, ok := badgeTones[kind][status]; ok {
		tone = t
	} else if t, ok := badgeTones[""][strings.ToLower(status)]; ok {
		tone = t
	}
	label := badgeLabels[status]
	if label == "" {
		label = strings.ReplaceAll(status, "_", " ")
	}
	return template.HTML(`<span class="badge badge-` + tone + `">` + template.HTMLEscapeString(label) + `</span>`)
}

// checkLabels are plain names for the doctor's check IDs.
var checkLabels = map[string]string{
	"config":           "Configuration",
	"backup_recent":    "Recent backup",
	"disk_space":       "Disk space",
	"dns_app":          "Address of this app (DNS)",
	"dns_dkim":         "Email signing (DKIM)",
	"dns_dmarc":        "Domain policy (DMARC)",
	"dns_mail_from":    "Bounce domain (DNS)",
	"https_cert":       "HTTPS certificate",
	"ses_identity":     "Sending domain in Amazon SES",
	"ses_production":   "Amazon SES production access",
	"ses_sending":      "Amazon SES sending",
	"sns_subscription": "Bounce and complaint reports",
	"inbound_mx":       "Receiving email (MX)",
	"inbound_rules":    "Receiving email (SES rules)",
}

// checkLabel is a doctor check's plain name (the ID when unknown).
func checkLabel(name string) string {
	if l, ok := checkLabels[name]; ok {
		return l
	}
	return strings.ReplaceAll(name, "_", " ")
}

// timelineLabels are plain names for contact timeline entry types.
var timelineLabels = map[string]string{
	"consent":       "Consent",
	"status_change": "Status changed",
	"tag_added":     "Tag added",
	"message":       "Email",
	"click":         "Clicked a link",
	"received":      "Emailed you",
	"delivery":      "Delivered",
	"bounce":        "Bounced",
	"complaint":     "Marked as spam",
	"reject":        "Rejected",
	"unsubscribe":   "Unsubscribed",
	"send_error":    "Send error",
}

// timelineLabel is a timeline entry type's plain name.
func timelineLabel(t string) string {
	if l, ok := timelineLabels[t]; ok {
		return l
	}
	return strings.ReplaceAll(t, "_", " ")
}

// sesReady reports whether the sending user's SES keys are configured;
// without them nothing is sent and messages stay queued.
func (u *UI) sesReady() bool {
	return u.d.Config.AWSAccessKeyID != "" && u.d.Config.AWSRegion != ""
}

// mergeFieldRx matches a merge field in text, e.g. {{first_name | "there"}}.
var mergeFieldRx = regexp.MustCompile(`\{\{\s*([a-zA-Z_][\w.]*)\s*(?:\|\s*"([^"]*)"\s*)?\}\}`)

// mergeChips escapes s and shows its merge fields as small labelled chips
// ("First name"), for lists where raw {{first_name}} reads like code.
func mergeChips(s string) template.HTML {
	esc := template.HTMLEscapeString(s)
	return template.HTML(mergeFieldRx.ReplaceAllStringFunc(esc, func(m string) string {
		raw := html.UnescapeString(m)
		sm := mergeFieldRx.FindStringSubmatch(raw)
		if sm == nil {
			return m
		}
		label := map[string]string{"first_name": "First name", "last_name": "Last name", "email": "Email", "unsubscribe_url": "Unsubscribe link"}[sm[1]]
		if label == "" {
			label = strings.ReplaceAll(strings.TrimPrefix(sm[1], "attr."), "_", " ")
		}
		return `<span class="mr-chip" title="` + template.HTMLEscapeString(raw) + `">` + template.HTMLEscapeString(label) + `</span>`
	}))
}

// reSubject is the subject of a reply: "Re: " once, never "Re: Re:".
func reSubject(s string) string {
	t := strings.TrimSpace(s)
	if l := strings.ToLower(t); strings.HasPrefix(l, "re:") || strings.HasPrefix(l, "sv:") || strings.HasPrefix(l, "aw:") {
		return t
	}
	return "Re: " + t
}

// fieldName turns an attribute key into a label: "company_name" → "Company name".
func fieldName(key string) string {
	s := strings.ReplaceAll(key, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
