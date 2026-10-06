package forms

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
)

// validName checks a form name: trimmed, 1-MaxNameLength characters.
func validName(name string) (string, error) {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return "", core.Invalid("name", "is required")
	case utf8.RuneCountInString(n) > MaxNameLength:
		return "", core.Invalid("name", "is longer than %d characters", MaxNameLength)
	case hasControl(n):
		return "", core.Invalid("name", "contains control characters")
	}
	return n, nil
}

// ValidateFields normalises a form's field list: keys are first_name,
// last_name or attr.<key> (each at most once), labels are trimmed and
// default to one derived from the key. Errors name the offending entry,
// e.g. field "fields[1].key".
func ValidateFields(fields []FormField) ([]FormField, error) {
	if len(fields) > MaxFields {
		return nil, core.Invalid("fields", "must have at most %d entries", MaxFields)
	}
	out := make([]FormField, 0, len(fields))
	seen := map[string]bool{}
	for i, f := range fields {
		path := fmt.Sprintf("fields[%d]", i)
		key := strings.TrimSpace(f.Key)
		if err := validFieldKey(key); err != nil {
			return nil, core.Invalid(path+".key", "%s", err)
		}
		if seen[key] {
			return nil, core.Invalid(path+".key", "%q is used twice", key)
		}
		seen[key] = true
		label := strings.TrimSpace(f.Label)
		switch {
		case label == "":
			label = defaultLabel(key)
		case utf8.RuneCountInString(label) > MaxLabelLength:
			return nil, core.Invalid(path+".label", "is longer than %d characters", MaxLabelLength)
		case hasControl(label):
			return nil, core.Invalid(path+".label", "contains control characters")
		}
		out = append(out, FormField{Key: key, Label: label, Required: f.Required})
	}
	return out, nil
}

// validFieldKey checks one field key.
func validFieldKey(key string) error {
	switch key {
	case FieldFirstName, FieldLastName:
		return nil
	case FieldEmail:
		return fmt.Errorf("email is always part of the form; don't list it")
	case "":
		return fmt.Errorf("is required")
	}
	attr, ok := strings.CutPrefix(key, AttrPrefix)
	if !ok {
		return fmt.Errorf("must be first_name, last_name or attr.<key>, not %q", key)
	}
	if contactstore.ValidateAttributes(map[string]any{attr: ""}) != nil {
		return fmt.Errorf("attribute key %q must be 1-%d letters, digits or underscores", attr, contactstore.MaxAttributeKey)
	}
	return nil
}

// defaultLabel derives a label from a field key: "First name",
// "Last name", or the attribute key with underscores as spaces and a
// capital first letter ("attr.company_size" -> "Company size").
func defaultLabel(key string) string {
	switch key {
	case FieldFirstName:
		return "First name"
	case FieldLastName:
		return "Last name"
	}
	s := strings.ReplaceAll(strings.TrimPrefix(key, AttrPrefix), "_", " ")
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// validTags normalises a form's tag names.
func validTags(tags []string) ([]string, error) {
	if len(tags) > MaxTags {
		return nil, core.Invalid("tags", "must have at most %d entries", MaxTags)
	}
	return contactstore.NormalizeTags(tags)
}

// validConsentText trims the consent text; empty means DefaultConsentText.
func validConsentText(s string) (string, error) {
	t := strings.TrimSpace(s)
	switch {
	case t == "":
		return DefaultConsentText, nil
	case utf8.RuneCountInString(t) > MaxConsentTextLength:
		return "", core.Invalid("consent_text", "is longer than %d characters", MaxConsentTextLength)
	case strings.ContainsFunc(t, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }):
		return "", core.Invalid("consent_text", "contains control characters")
	}
	return t, nil
}

// validRedirectURL checks a success redirect: empty, or an absolute https
// URL without credentials.
func validRedirectURL(s string) (string, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", nil
	}
	if len(t) > MaxRedirectURLLength {
		return "", core.Invalid("redirect_url", "is longer than %d characters", MaxRedirectURLLength)
	}
	u, err := url.Parse(t)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" {
		return "", core.Invalid("redirect_url", "must be an absolute https URL, e.g. https://example.com/thanks")
	}
	if u.User != nil {
		return "", core.Invalid("redirect_url", "must not contain credentials")
	}
	if hasControl(t) || strings.ContainsAny(t, " \"<>\\") {
		return "", core.Invalid("redirect_url", "contains characters that are not allowed in a URL")
	}
	return t, nil
}

// ValidateOrigins normalises CORS origins: each is scheme://host[:port]
// with scheme http or https and nothing else (a trailing "/" is dropped).
// Duplicates are removed; scheme and host are lowercased.
func ValidateOrigins(origins []string) ([]string, error) {
	if len(origins) > MaxOrigins {
		return nil, core.Invalid("allowed_origins", "must have at most %d entries", MaxOrigins)
	}
	out := make([]string, 0, len(origins))
	seen := map[string]bool{}
	for i, raw := range origins {
		o, err := normalizeOrigin(raw)
		if err != nil {
			return nil, core.Invalid(fmt.Sprintf("allowed_origins[%d]", i), "%s", err)
		}
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out, nil
}

func normalizeOrigin(raw string) (string, error) {
	t := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	bad := fmt.Errorf("must be an origin like https://example.com (scheme, host and optional port only), not %q", raw)
	if t == "" || len(t) > MaxOriginLength || hasControl(t) {
		return "", bad
	}
	u, err := url.Parse(t)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" ||
		strings.Contains(t, "?") || strings.Contains(t, "#") {
		return "", bad
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// originOf returns the origin (scheme://host[:port]) of an absolute URL,
// or "" when it has none.
func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }
