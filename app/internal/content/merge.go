package content

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// Merge field names.
const (
	fieldFirstName   = "first_name"
	fieldLastName    = "last_name"
	fieldEmail       = "email"
	fieldUnsubscribe = "unsubscribe_url"
	attrPrefix       = "attr."
)

// mergeField is one {{name | "fallback"}} occurrence.
type mergeField struct {
	Name     string
	Fallback string
}

// segment is literal text or a merge field (Field != nil).
type segment struct {
	Text  string
	Field *mergeField
}

// mergeTemplate is a parsed text with merge fields.
type mergeTemplate []segment

// vocabulary decides which field names a text may use. It returns a
// message explaining why a name is not allowed, or "" when it is.
type vocabulary func(name string) string

// emailVocabulary allows the contact fields, attributes and the
// unsubscribe URL.
func emailVocabulary(name string) string {
	switch name {
	case fieldFirstName, fieldLastName, fieldEmail, fieldUnsubscribe:
		return ""
	}
	if key, ok := strings.CutPrefix(name, attrPrefix); ok {
		if !validAttrKey(key) {
			return fmt.Sprintf("attribute key %q must be 1-64 letters, digits or underscores", key)
		}
		return ""
	}
	if strings.HasPrefix(name, "contact.") {
		// GHL merge tags ({{contact.first_name}}) are common in imported HTML.
		return fmt.Sprintf("unknown merge field %q (GoHighLevel syntax?); use first_name, last_name, email or attr.<key>", name)
	}
	return fmt.Sprintf("unknown merge field %q; use first_name, last_name, email, unsubscribe_url or attr.<key>", name)
}

// validAttrKey matches contactstore's attribute key rule.
func validAttrKey(k string) bool {
	if k == "" || len(k) > 64 {
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

// parseMerge parses src. Every "{{" must start a well-formed field whose
// name vocab allows; errors are core validation errors for field.
func parseMerge(field, src string, vocab vocabulary) (mergeTemplate, error) {
	var out mergeTemplate
	rest := src
	offset := 0
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			if rest != "" {
				out = append(out, segment{Text: rest})
			}
			return out, nil
		}
		if i > 0 {
			out = append(out, segment{Text: rest[:i]})
		}
		j := strings.Index(rest[i+2:], "}}")
		if j < 0 {
			return nil, invalidAt(field, src, offset+i, `unclosed merge field: "{{" without "}}"`)
		}
		inner := rest[i+2 : i+2+j]
		f, msg := parseField(inner)
		if msg == "" {
			msg = vocab(f.Name)
		}
		if msg != "" {
			return nil, invalidAt(field, src, offset+i, "%s", msg)
		}
		out = append(out, segment{Field: &f})
		consumed := i + 2 + j + 2
		rest = rest[consumed:]
		offset += consumed
	}
}

// parseField parses the inside of {{ }}: a name, optionally followed by
// | "fallback". It returns a message on syntax errors.
func parseField(inner string) (mergeField, string) {
	name, fb, hasFB := strings.Cut(inner, "|")
	name = strings.TrimSpace(name)
	if name == "" {
		return mergeField{}, "empty merge field {{}}"
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.') {
			return mergeField{}, fmt.Sprintf("invalid merge field {{%s}}", strings.TrimSpace(inner))
		}
	}
	f := mergeField{Name: name}
	if !hasFB {
		return f, ""
	}
	v, ok := parseQuoted(strings.TrimSpace(fb))
	if !ok {
		return mergeField{}, fmt.Sprintf(`invalid fallback in {{%s}}: write it as {{%s | "text"}}`, strings.TrimSpace(inner), name)
	}
	f.Fallback = v
	return f, ""
}

// parseQuoted parses a double-quoted string with \" and \\ escapes.
func parseQuoted(s string) (string, bool) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", false
	}
	body := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch c {
		case '\\':
			if i+1 >= len(body) || (body[i+1] != '"' && body[i+1] != '\\') {
				return "", false
			}
			i++
			b.WriteByte(body[i])
		case '"':
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// invalidAt returns a validation error for field; when src has more than
// one line the message names the line of offset.
func invalidAt(field, src string, offset int, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if strings.Contains(src, "\n") {
		msg = fmt.Sprintf("line %d: %s", strings.Count(src[:offset], "\n")+1, msg)
	}
	return invalid(field, "%s", msg)
}

// values resolves merge field names; ok is false when there is no value
// (the fallback is used then).
type values func(name string) (string, bool)

// resolve returns the value of f, or its fallback when the value is
// missing or empty.
func (f *mergeField) resolve(v values) string {
	if s, ok := v(f.Name); ok && s != "" {
		return s
	}
	return f.Fallback
}

// execute renders t, HTML-escaping values when escape is set. Literal text
// is copied as it is.
func (t mergeTemplate) execute(v values, escape bool) string {
	var b strings.Builder
	for _, s := range t {
		if s.Field == nil {
			b.WriteString(s.Text)
			continue
		}
		val := s.Field.resolve(v)
		if escape {
			val = html.EscapeString(val)
		}
		b.WriteString(val)
	}
	return b.String()
}

// placeholders renders t with every field replaced by a unique
// alphanumeric placeholder that Markdown and the HTML tokenizer pass
// through unchanged (also inside link URLs). The returned set substitutes
// the values into the converted output (see placeholderSet.substituteHTML).
func (t mergeTemplate) placeholders(src string) (string, *placeholderSet) {
	prefix := "mrfieldq"
	for strings.Contains(src, prefix) {
		prefix += "q"
	}
	p := &placeholderSet{prefix: prefix, rx: regexp.MustCompile(prefix + `([0-9]+)z`)}
	var b strings.Builder
	for _, s := range t {
		if s.Field == nil {
			b.WriteString(s.Text)
			continue
		}
		fmt.Fprintf(&b, "%s%dz", prefix, len(p.fields))
		p.fields = append(p.fields, s.Field)
	}
	return b.String(), p
}

// count returns how often t uses the field name.
func (t mergeTemplate) count(name string) int {
	n := 0
	for _, s := range t {
		if s.Field != nil && s.Field.Name == name {
			n++
		}
	}
	return n
}
