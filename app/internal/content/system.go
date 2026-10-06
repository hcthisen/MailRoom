package content

import (
	"context"
	"fmt"
	"html/template"
	"strings"

	"mailroom/internal/core"
)

// systemData is the data of the system email bodies.
type systemData struct {
	Sender     string // settings sender name
	FormName   string
	ConfirmURL string
	Label      string // confirm button label
	BrandColor template.CSS
	Title      string
	Paragraphs [][]string // alert body: paragraphs of lines
	BaseURL    string
}

// systemTemplates are the bodies of the system emails. They are trusted
// templates, so html/template escapes the data. The confirm buttons are
// table-based for Outlook.
var systemTemplates = template.Must(template.New("system").Parse(`
{{- define "button" -}}
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:8px 0 24px;">
<tr>
<td align="center" style="background-color:{{.BrandColor}};border-radius:4px;">
<a href="{{.ConfirmURL}}" style="display:inline-block;padding:12px 28px;font-size:16px;font-weight:bold;color:#ffffff;text-decoration:none;border-radius:4px;">{{.Label}}</a>
</td>
</tr>
</table>
<p style="margin:0 0 16px;font-size:14px;color:#6b7280;">If the button doesn't work, copy this link into your browser:<br><a href="{{.ConfirmURL}}" style="color:#6b7280;word-break:break-all;">{{.ConfirmURL}}</a></p>
{{- end -}}

{{- define "optin_confirm" -}}
<h1 style="margin:0 0 16px;font-size:24px;line-height:1.3;color:#111827;">Please confirm your subscription</h1>
<p style="margin:0 0 16px;">Hello,</p>
<p style="margin:0 0 16px;">Please confirm that you want to receive emails from {{.Sender}}{{if .FormName}} (you signed up through &ldquo;{{.FormName}}&rdquo;){{end}}.</p>
{{template "button" .}}
<p style="margin:0;font-size:14px;color:#6b7280;">If you didn't sign up, you can ignore this email: you won't be subscribed and won't hear from us again.</p>
{{- end -}}

{{- define "resubscribe_confirm" -}}
<h1 style="margin:0 0 16px;font-size:24px;line-height:1.3;color:#111827;">Confirm that you want our emails again</h1>
<p style="margin:0 0 16px;">Hello,</p>
<p style="margin:0 0 16px;">You asked to receive emails from {{.Sender}} again. You unsubscribed earlier, so we need your confirmation first.</p>
{{template "button" .}}
<p style="margin:0;font-size:14px;color:#6b7280;">If you didn't ask for this, ignore this email: you stay unsubscribed.</p>
{{- end -}}

{{- define "alert" -}}
<h1 style="margin:0 0 16px;font-size:22px;line-height:1.3;color:#b91c1c;">Alert: {{.Title}}</h1>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 16px;">
<tr>
<td style="padding:16px;background-color:#fef2f2;border-left:4px solid #b91c1c;font-size:15px;line-height:1.5;color:#1f2937;">
{{- range .Paragraphs}}
<p style="margin:0 0 12px;">{{range $i, $l := .}}{{if $i}}<br>{{end}}{{$l}}{{end}}</p>
{{- else}}
<p style="margin:0;">No details.</p>
{{- end}}
</td>
</tr>
</table>
<p style="margin:0;font-size:13px;color:#6b7280;">This alert was sent automatically by Mailroom at <a href="{{.BaseURL}}" style="color:#6b7280;">{{.BaseURL}}</a>.</p>
{{- end -}}
`))

// labels of the confirm buttons, per template.
var buttonLabels = map[string]string{
	core.TemplateOptinConfirm:       "Confirm subscription",
	core.TemplateResubscribeConfirm: "Yes, send me emails again",
}

// renderSystem renders a system email: branded with the layout's logo and
// colour, but without unsubscribe link or click tracking (K-3). No text a
// public form visitor submitted (such as a first name) goes into it; the
// form name is the operator's.
//
// Data: optin_confirm takes confirm_url (required) and form_name;
// resubscribe_confirm takes confirm_url; alert takes title and body.
func (s *Service) renderSystem(ctx context.Context, in core.RenderInput) (core.Rendered, error) {
	st, err := s.settings.Get(ctx)
	if err != nil {
		return core.Rendered{}, err
	}
	layout, err := s.GetLayout(ctx, nil)
	if err != nil {
		return core.Rendered{}, err
	}
	sender := strings.TrimSpace(st.Sender.FromName)
	if sender == "" {
		sender = "us"
	}
	d := systemData{
		Sender:     sender,
		FormName:   singleLine(dataString(in.Data, "form_name")),
		ConfirmURL: strings.TrimSpace(dataString(in.Data, "confirm_url")),
		BrandColor: template.CSS(layout.BrandColor),
		BaseURL:    s.baseURL,
	}
	// The confirmation emails never greet by name: a public form accepts
	// any first_name, and with the name in the greeting anyone could have
	// their own text (e.g. a phishing URL) emailed from this domain to
	// any address.
	footer := st.CompanyFooter
	var subject, preheader string
	switch in.SystemTemplate {
	case core.TemplateOptinConfirm:
		subject = "Please confirm your subscription"
		if sender != "us" {
			subject += " to " + sender
		}
		preheader = "One click to confirm, and you're in."
	case core.TemplateResubscribeConfirm:
		subject = "Confirm that you want our emails again"
		preheader = "You unsubscribed earlier. Confirm to receive emails again."
	case core.TemplateAlert:
		d.Title = singleLine(dataString(in.Data, "title"))
		if d.Title == "" {
			d.Title = "Mailroom needs your attention"
		}
		d.Paragraphs = paragraphs(dataString(in.Data, "body"))
		subject = "Mailroom alert: " + d.Title
		footer = ""
	default:
		return core.Rendered{}, invalid("system_template", "unknown system template %q", in.SystemTemplate)
	}
	if label, ok := buttonLabels[in.SystemTemplate]; ok {
		if !isHTTPURL(d.ConfirmURL) {
			return core.Rendered{}, invalid("data.confirm_url", "must be an absolute http(s) URL for %s", in.SystemTemplate)
		}
		d.Label = label
	}
	var body strings.Builder
	if err := systemTemplates.ExecuteTemplate(&body, in.SystemTemplate, d); err != nil {
		return core.Rendered{}, fmt.Errorf("content: render %s: %w", in.SystemTemplate, err)
	}
	out, err := wrapDefault(layout.LayoutInput, wrapParams{
		Subject: subject, Preheader: preheader, Content: body.String(), Footer: footer,
	})
	if err != nil {
		return core.Rendered{}, err
	}
	return core.Rendered{Subject: subject, HTML: out, Text: HTMLToText(out)}, nil
}

// paragraphs splits text into paragraphs (blank lines) of lines.
func paragraphs(text string) [][]string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var out [][]string
	for _, p := range strings.Split(text, "\n\n") {
		var lines []string
		for _, l := range strings.Split(p, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) > 0 {
			out = append(out, lines)
		}
	}
	return out
}
