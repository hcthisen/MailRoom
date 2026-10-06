package content

import (
	"errors"
	"html"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	nethtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// placeholderSet maps the placeholders of mergeTemplate.placeholders back
// to their merge fields.
type placeholderSet struct {
	prefix string
	fields []*mergeField
	rx     *regexp.Regexp // prefix(\d+)z
}

// replace replaces every placeholder in s by fn of its field, in one pass
// (substituted values are never scanned again).
func (p *placeholderSet) replace(s string, fn func(f *mergeField) string) string {
	if len(p.fields) == 0 || !strings.Contains(s, p.prefix) {
		return s
	}
	return p.rx.ReplaceAllStringFunc(s, func(m string) string {
		i, err := strconv.Atoi(m[len(p.prefix) : len(m)-1])
		if err != nil || i >= len(p.fields) {
			return m
		}
		return fn(p.fields[i])
	})
}

// urlAttrs are the attributes whose value is a URL. Merge values placed in
// them are URL-checked or URL-encoded (see substituteURL).
var urlAttrs = map[string]bool{
	"href": true, "src": true, "background": true, "action": true,
	"formaction": true, "poster": true, "cite": true, "xlink:href": true,
}

// substituteHTML replaces the placeholders in src, an HTML fragment or
// document, by the merge values, according to where each one stands:
//
//   - in text, comments and ordinary attributes: HTML-escaped;
//   - at the start of a URL attribute (href="{{attr.website}}"): only a
//     URL with the http, https, mailto or tel scheme (or none) is used;
//     anything else (javascript:, data:, ...) is replaced by the fallback
//     when that is safe, else by "#". {{unsubscribe_url}} is our own URL
//     and used as it is;
//   - after "?" or "#" in a URL attribute: query-escaped;
//   - elsewhere in a URL attribute (a path): path-escaped, keeping "/".
//
// An <a> whose href contains a merge field is marked NoTrackAttr: its URL
// differs per recipient, and tracking it would store every recipient's
// merged URL (personal data) in links, one row each.
func (p *placeholderSet) substituteHTML(src string, v values) string {
	if len(p.fields) == 0 || !strings.Contains(src, p.prefix) {
		return src
	}
	escaped := func(f *mergeField) string { return html.EscapeString(f.resolve(v)) }
	var b strings.Builder
	b.Grow(len(src) + len(src)/8)
	z := nethtml.NewTokenizer(strings.NewReader(src))
	offset := 0
	for {
		tt := z.Next()
		if tt == nethtml.ErrorToken {
			if !errors.Is(z.Err(), io.EOF) {
				b.WriteString(p.replace(src[offset:], escaped))
			}
			return b.String()
		}
		raw := string(z.Raw())
		offset += len(raw)
		if !strings.Contains(raw, p.prefix) {
			b.WriteString(raw)
			continue
		}
		if tt != nethtml.StartTagToken && tt != nethtml.SelfClosingTagToken {
			b.WriteString(p.replace(raw, escaped))
			continue
		}
		tok := z.Token()
		if tag, ok := p.substituteTag(&tok, v); ok {
			b.WriteString(tag)
		} else {
			b.WriteString(p.replace(raw, escaped))
		}
	}
}

// substituteTag substitutes the placeholders in the attribute values of
// tok and serialises it. ok is false when a placeholder stands where
// the tag can't be rebuilt around it (tag name, attribute name); the
// caller then falls back to escaping in the raw tag.
func (p *placeholderSet) substituteTag(tok *nethtml.Token, v values) (string, bool) {
	if strings.Contains(tok.Data, p.prefix) {
		return "", false
	}
	hrefField, noTrack := false, false
	for i, a := range tok.Attr {
		if strings.Contains(a.Key, p.prefix) {
			return "", false
		}
		key := strings.ToLower(a.Key)
		if key == NoTrackAttr {
			noTrack = true
		}
		if !strings.Contains(a.Val, p.prefix) {
			continue
		}
		if urlAttrs[key] {
			tok.Attr[i].Val = p.substituteURL(a.Val, v)
			if key == "href" {
				hrefField = true
			}
			continue
		}
		// tok.String escapes attribute values: insert them raw.
		tok.Attr[i].Val = p.replace(a.Val, func(f *mergeField) string { return f.resolve(v) })
	}
	if tok.DataAtom == atom.A && hrefField && !noTrack {
		tok.Attr = append(tok.Attr, nethtml.Attribute{Key: NoTrackAttr})
	}
	return tok.String(), true
}

// substituteURL substitutes the placeholders in the URL attribute value
// val (unescaped); see substituteHTML for the rules.
func (p *placeholderSet) substituteURL(val string, v values) string {
	lead := len(val) - len(strings.TrimLeft(val, " \t\n\r\f"))
	locs := p.rx.FindAllStringSubmatchIndex(val, -1)
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		i, err := strconv.Atoi(val[loc[2]:loc[3]])
		if err != nil || i >= len(p.fields) {
			continue
		}
		f := p.fields[i]
		b.WriteString(val[last:loc[0]])
		last = loc[1]
		s := f.resolve(v)
		switch {
		case loc[0] == lead:
			if f.Name != fieldUnsubscribe && !safeURL(s) {
				s = "#"
				if f.Fallback != "" && safeURL(f.Fallback) {
					s = f.Fallback
				}
			}
		case strings.ContainsAny(val[:loc[0]], "?#"):
			s = url.QueryEscape(s)
		default:
			s = strings.ReplaceAll(url.PathEscape(s), "%2F", "/")
		}
		b.WriteString(s)
	}
	b.WriteString(val[last:])
	return b.String()
}

// safeURL reports whether s may start a link or image URL: empty, relative
// (no scheme) or with the http, https, mailto or tel scheme. Values with
// control characters, which browsers strip to reveal a scheme, are not.
func safeURL(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "", "http", "https", "mailto", "tel":
		return true
	}
	return false
}
