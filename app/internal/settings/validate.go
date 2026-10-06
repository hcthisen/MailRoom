package settings

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/core"
)

// Limits on text settings.
const (
	MaxFromNameLength = 200
	MaxFooterLength   = 2000
)

// Validate checks s against the sending domain and returns it with email
// addresses normalised. Errors are *core.ValidationError with dotted field
// names ("sender.from_email").
func Validate(s Settings, domain string) (Settings, error) {
	s = s.clone()
	var err error
	if s.Sender.FromName, err = text("sender.from_name", s.Sender.FromName, MaxFromNameLength, false); err != nil {
		return s, err
	}
	if s.CompanyFooter, err = text("company_footer", s.CompanyFooter, MaxFooterLength, true); err != nil {
		return s, err
	}
	if s.Sender.FromEmail, err = address("sender.from_email", s.Sender.FromEmail, domain); err != nil {
		return s, err
	}
	if s.Sender.ReplyTo, err = address("sender.reply_to", s.Sender.ReplyTo, domain); err != nil {
		return s, err
	}
	if s.OwnerEmail, err = address("owner_email", s.OwnerEmail, ""); err != nil {
		return s, err
	}
	if s.AlertEmail, err = address("alert_email", s.AlertEmail, ""); err != nil {
		return s, err
	}
	s.Timezone = strings.TrimSpace(s.Timezone)
	if s.Timezone == "" || s.Timezone == "Local" {
		return s, core.Invalid("timezone", "must be an IANA time zone such as Europe/London or UTC")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return s, core.Invalid("timezone", "%q is not a known IANA time zone", s.Timezone)
	}
	if r := s.Sending.RateOverridePerSec; r < 0 || r > 1000 {
		return s, core.Invalid("sending.rate_override_per_sec", "must be between 0 (automatic) and 1000")
	}
	if f := s.Sending.RateFraction; f <= 0 || f > 1 {
		return s, core.Invalid("sending.rate_fraction", "must be greater than 0 and at most 1")
	}
	cb := s.CircuitBreaker
	if cb.MinMessages < 1 || cb.MinMessages > 1_000_000 {
		return s, core.Invalid("circuit_breaker.min_messages", "must be between 1 and 1000000")
	}
	if cb.HardBounceRate <= 0 || cb.HardBounceRate > 1 {
		return s, core.Invalid("circuit_breaker.hard_bounce_rate", "must be greater than 0 and at most 1")
	}
	if cb.ComplaintRate <= 0 || cb.ComplaintRate > 1 {
		return s, core.Invalid("circuit_breaker.complaint_rate", "must be greater than 0 and at most 1")
	}
	if s.DefaultSendWindow != nil {
		if err := s.DefaultSendWindow.Validate("default_send_window"); err != nil {
			return s, err
		}
	}
	return s, nil
}

// text trims v and checks its length and characters. Only multiline
// fields may contain newlines; no field may contain other control
// characters (a From name ends up in a mail header).
func text(field, v string, maxLen int, multiline bool) (string, error) {
	v = strings.TrimSpace(v)
	if !utf8.ValidString(v) {
		return v, core.Invalid(field, "must be valid UTF-8")
	}
	if utf8.RuneCountInString(v) > maxLen {
		return v, core.Invalid(field, "must be at most %d characters", maxLen)
	}
	for _, r := range v {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\r' || r == '\t')) {
			return v, core.Invalid(field, "must not contain control characters")
		}
	}
	return v, nil
}

// address normalises an optional email address; with domain set it must be
// on exactly that domain (D-7).
func address(field, v, domain string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	norm, err := core.NormalizeEmail(v)
	if err != nil {
		return v, core.Invalid(field, "is not a valid email address")
	}
	if domain != "" && !strings.HasSuffix(norm, "@"+strings.ToLower(domain)) {
		return norm, core.Invalid(field, "must be an address on the sending domain %s", domain)
	}
	return norm, nil
}

// diff returns the changed leaf fields between a and b as
// {"sender.from_email": {"from": ..., "to": ...}}, for the audit log.
func diff(a, b Settings) (map[string]any, error) {
	fa, err := flatten(a)
	if err != nil {
		return nil, err
	}
	fb, err := flatten(b)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for k, vb := range fb {
		if va, ok := fa[k]; !ok || !reflect.DeepEqual(va, vb) {
			out[k] = map[string]any{"from": fa[k], "to": vb}
		}
	}
	for k, va := range fa {
		if _, ok := fb[k]; !ok {
			out[k] = map[string]any{"from": va, "to": nil}
		}
	}
	delete(out, "version")
	delete(out, "domain")
	return out, nil
}

// flatten maps a settings document to dotted leaf paths. Arrays and null
// are leaves.
func flatten(s Settings) (map[string]any, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("settings: encode: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("settings: decode: %w", err)
	}
	out := map[string]any{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if obj, ok := v.(map[string]any); ok && prefix != "default_send_window" {
			for k, sub := range obj {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, sub)
			}
			return
		}
		out[prefix] = v
	}
	walk("", m)
	return out, nil
}
