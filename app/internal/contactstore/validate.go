package contactstore

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/core"
)

// Field limits.
const (
	MaxTagLength       = 64
	MaxNameLength      = 200
	MaxAttributeKey    = 64
	MaxAttributesBytes = 16 << 10 // encoded JSON size
)

// NormalizeTag trims a tag name and checks it is 1–64 characters without
// control characters.
func NormalizeTag(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", core.Invalid("tags", "tag names must not be empty")
	}
	if utf8.RuneCountInString(n) > MaxTagLength {
		return "", core.Invalid("tags", "tag %q is longer than %d characters", n, MaxTagLength)
	}
	if !utf8.ValidString(n) || strings.IndexFunc(n, unicode.IsControl) >= 0 {
		return "", core.Invalid("tags", "tag %q contains invalid characters", n)
	}
	return n, nil
}

// NormalizeTags normalises each name and removes case-insensitive
// duplicates (keeping the first spelling), matching the tags table's
// COLLATE NOCASE uniqueness.
func NormalizeTags(names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, raw := range names {
		n, err := NormalizeTag(raw)
		if err != nil {
			return nil, err
		}
		k := foldASCII(n)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return out, nil
}

// foldASCII lowercases ASCII letters only, like SQLite's NOCASE collation.
func foldASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// ValidateName checks a first or last name.
func ValidateName(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) > MaxNameLength {
		return "", core.Invalid(field, "is longer than %d characters", MaxNameLength)
	}
	if strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return "", core.Invalid(field, "contains control characters")
	}
	return v, nil
}

// ValidateAttributes checks attribute keys (1–64 of [A-Za-z0-9_]) and that
// the map encodes to at most MaxAttributesBytes of JSON.
func ValidateAttributes(attrs map[string]any) error {
	for k := range attrs {
		if !validAttributeKey(k) {
			return core.Invalid("attributes", "key %q must be 1-%d letters, digits or underscores", k, MaxAttributeKey)
		}
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return core.Invalid("attributes", "must be JSON-serialisable")
	}
	if len(b) > MaxAttributesBytes {
		return core.Invalid("attributes", "must be at most %d bytes as JSON", MaxAttributesBytes)
	}
	return nil
}

func validAttributeKey(k string) bool {
	if k == "" || len(k) > MaxAttributeKey {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
