package content

import (
	"html/template"
	"strings"

	"mailroom/internal/core"
)

// replyTemplate is the HTML of a reply from the inbox: a plain personal
// email (no layout, logo, footer or tracking), with the quoted original.
var replyTemplate = template.Must(template.New("reply").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Subject}}</title></head>
<body style="margin:0;padding:16px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;color:#111827;">
{{- range .Body}}
<p style="margin:0 0 12px;">{{range $i, $l := .}}{{if $i}}<br>{{end}}{{$l}}{{end}}</p>
{{- end}}
{{- if .Quote}}
<p style="margin:16px 0 4px;color:#6b7280;">{{.QuoteHeader}}</p>
<blockquote style="margin:0;padding:0 0 0 12px;border-left:3px solid #d1d5db;color:#4b5563;">
{{- range .Quote}}
<p style="margin:0 0 8px;">{{range $i, $l := .}}{{if $i}}<br>{{end}}{{$l}}{{end}}</p>
{{- end}}
</blockquote>
{{- end}}
</body></html>`))

// renderReply renders a reply (core.TemplateReply) from its message data:
// subject, body and the optional quote. The body is the operator's plain
// text; html/template escapes it.
func renderReply(data map[string]any) (core.Rendered, error) {
	subject := singleLine(dataString(data, core.DataReplySubject))
	body := strings.TrimSpace(dataString(data, core.DataReplyBody))
	if subject == "" {
		return core.Rendered{}, invalid("data.subject", "is required for a reply")
	}
	if body == "" {
		return core.Rendered{}, invalid("data.body", "is required for a reply")
	}
	quote := strings.TrimSpace(dataString(data, core.DataReplyQuote))
	quoteHead := singleLine(dataString(data, core.DataReplyQuoteHead))
	d := struct {
		Subject     string
		Body, Quote [][]string
		QuoteHeader string
	}{Subject: subject, Body: replyParagraphs(body), QuoteHeader: quoteHead}
	if quote != "" {
		d.Quote = replyParagraphs(quote)
	}
	var h strings.Builder
	if err := replyTemplate.Execute(&h, d); err != nil {
		return core.Rendered{}, err
	}
	text := normalizeText(body)
	if quote != "" {
		text += "\n\n"
		if quoteHead != "" {
			text += quoteHead + "\n"
		}
		for _, l := range strings.Split(normalizeText(quote), "\n") {
			text += strings.TrimRight("> "+l, " ") + "\n"
		}
	}
	return core.Rendered{Subject: subject, HTML: h.String(), Text: strings.TrimRight(text, "\n") + "\n"}, nil
}

// replyParagraphs splits text into paragraphs of lines, keeping the
// operator's line breaks (unlike paragraphs, empty lines inside are kept
// as paragraph breaks only).
func replyParagraphs(text string) [][]string {
	var out [][]string
	for _, p := range strings.Split(normalizeText(text), "\n\n") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		lines := strings.Split(p, "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " \t")
		}
		out = append(out, lines)
	}
	return out
}

func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
