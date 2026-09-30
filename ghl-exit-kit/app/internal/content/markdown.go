package content

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// newMarkdown returns the Markdown converter: GitHub Flavored Markdown
// (tables, strikethrough, autolinks, task lists), raw HTML allowed (the
// authors are the install's admins), single line breaks kept as <br>
// (what non-technical authors expect) and XHTML-style void tags.
func newMarkdown() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(gmhtml.WithUnsafe(), gmhtml.WithHardWraps(), gmhtml.WithXHTML()),
	)
}

// markdownToHTML converts Markdown source to HTML.
func (s *Service) markdownToHTML(src string) (string, error) {
	var buf bytes.Buffer
	if err := s.md.Convert([]byte(src), &buf); err != nil {
		return "", fmt.Errorf("content: convert markdown: %w", err)
	}
	return buf.String(), nil
}

// contentStyles are the inline styles given to elements of Markdown
// output, so they look right in clients that ignore <style> blocks.
// "%s" is replaced by the brand colour.
var contentStyles = map[atom.Atom]string{
	atom.P:          "margin:0 0 16px;",
	atom.H1:         "margin:0 0 16px;font-size:26px;line-height:1.3;font-weight:bold;color:#111827;",
	atom.H2:         "margin:24px 0 12px;font-size:21px;line-height:1.3;font-weight:bold;color:#111827;",
	atom.H3:         "margin:20px 0 10px;font-size:18px;line-height:1.3;font-weight:bold;color:#111827;",
	atom.H4:         "margin:16px 0 8px;font-size:16px;line-height:1.3;font-weight:bold;color:#111827;",
	atom.A:          "color:%s;text-decoration:underline;",
	atom.Img:        "max-width:100%;height:auto;border:0;outline:none;text-decoration:none;",
	atom.Ul:         "margin:0 0 16px;padding:0 0 0 24px;",
	atom.Ol:         "margin:0 0 16px;padding:0 0 0 24px;",
	atom.Li:         "margin:0 0 6px;",
	atom.Blockquote: "margin:0 0 16px;padding:0 0 0 16px;border-left:4px solid #e5e7eb;color:#4b5563;",
	atom.Table:      "border-collapse:collapse;width:100%;margin:0 0 16px;",
	atom.Th:         "border:1px solid #e5e7eb;padding:6px 10px;text-align:left;background:#f9fafb;",
	atom.Td:         "border:1px solid #e5e7eb;padding:6px 10px;text-align:left;",
	atom.Pre:        "margin:0 0 16px;padding:12px;background:#f3f4f6;border-radius:4px;font-family:Menlo,Consolas,monospace;font-size:14px;white-space:pre-wrap;",
	atom.Code:       "font-family:Menlo,Consolas,monospace;font-size:14px;background:#f3f4f6;padding:1px 4px;border-radius:3px;",
	atom.Hr:         "border:0;border-top:1px solid #e5e7eb;margin:24px 0;",
}

// inlineStyles adds contentStyles to the start tags of src. Existing style
// attributes are kept after the defaults, so the author's styles win.
// Everything else is copied byte for byte.
func inlineStyles(src, brandColor string) string {
	var b strings.Builder
	b.Grow(len(src) + len(src)/4)
	z := html.NewTokenizer(strings.NewReader(src))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return b.String()
		}
		raw := z.Raw()
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			b.Write(raw)
			continue
		}
		tok := z.Token()
		style, ok := contentStyles[tok.DataAtom]
		if !ok {
			b.Write(raw)
			continue
		}
		if strings.Contains(style, "%s") {
			style = fmt.Sprintf(style, brandColor)
		}
		setStyle(&tok, style)
		b.WriteString(tok.String())
	}
}

// setStyle prepends style to tok's style attribute (adding one if needed).
func setStyle(tok *html.Token, style string) {
	for i, a := range tok.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, "style") {
			tok.Attr[i].Val = style + a.Val
			return
		}
	}
	tok.Attr = append(tok.Attr, html.Attribute{Key: "style", Val: style})
}

// splitDocument handles raw HTML that is a whole document (e.g. copied
// from GHL, E-7): it returns the <style> elements of its head and the
// inner HTML of its body, so the content can be wrapped in the layout.
// Fragments are returned unchanged with no styles.
func splitDocument(src string) (headStyles, body string) {
	z := html.NewTokenizer(strings.NewReader(src))
	var styles strings.Builder
	var bodyStart, bodyEnd = -1, -1
	offset := 0
	inStyle := false
	var styleStart int
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		raw := z.Raw()
		start := offset
		offset += len(raw)
		name, _ := z.TagName()
		a := atom.Lookup(name)
		switch {
		case tt == html.StartTagToken && a == atom.Body && bodyStart < 0:
			bodyStart = offset
		case tt == html.EndTagToken && a == atom.Body && bodyStart >= 0:
			bodyEnd = start
		case tt == html.StartTagToken && a == atom.Style && bodyStart < 0:
			inStyle, styleStart = true, start
		case tt == html.EndTagToken && a == atom.Style && inStyle:
			inStyle = false
			styles.WriteString(src[styleStart:offset])
			styles.WriteByte('\n')
		}
	}
	if bodyStart < 0 {
		return "", src
	}
	if bodyEnd < bodyStart {
		bodyEnd = len(src)
		if i := strings.LastIndex(strings.ToLower(src), "</html>"); i >= bodyStart {
			bodyEnd = i
		}
	}
	return styles.String(), src[bodyStart:bodyEnd]
}
