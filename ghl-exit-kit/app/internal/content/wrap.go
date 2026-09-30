package content

import (
	"fmt"
	"html"
	"html/template"
	"strings"
	"sync"
)

// frame is the data of the built-in layout.
type frame struct {
	Subject        string
	Preheader      template.HTML // the hidden preheader block, or ""
	HeadStyles     template.HTML // <style> elements of imported HTML documents
	LogoURL        string
	BrandColor     template.CSS
	Content        template.HTML
	Footer         template.HTML
	UnsubscribeURL string // "" for system emails: no unsubscribe link
	MsoOpen        template.HTML
	MsoClose       template.HTML
}

// Outlook (Word engine) ignores max-width, so the 600px column is wrapped
// in a fixed-width table for it only. They are passed as values because
// html/template drops comments from template text.
const (
	msoOpen  = `<!--[if mso]><table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0"><tr><td><![endif]-->`
	msoClose = `<!--[if mso]></td></tr></table><![endif]-->`
)

// frameTemplate is the built-in layout: table-based, inline-styled and
// responsive (one media query for narrow screens), which renders in every
// major client including Outlook. It is our own trusted template, so it
// uses html/template; user content arrives as pre-rendered HTML.
var frameTemplate = template.Must(template.New("layout").Parse(`<!DOCTYPE html>
<html lang="en" xmlns="http://www.w3.org/1999/xhtml">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="X-UA-Compatible" content="IE=edge">
<meta name="x-apple-disable-message-reformatting">
<meta name="color-scheme" content="light">
<meta name="supported-color-schemes" content="light">
<title>{{.Subject}}</title>
<style>
body{margin:0;padding:0;width:100%;-webkit-text-size-adjust:100%;-ms-text-size-adjust:100%;}
table,td{border-collapse:collapse;mso-table-lspace:0pt;mso-table-rspace:0pt;}
img{border:0;outline:none;text-decoration:none;-ms-interpolation-mode:bicubic;}
@media only screen and (max-width:620px){
.mr-container{width:100% !important;max-width:100% !important;}
.mr-pad{padding:24px 20px !important;}
.mr-content img{max-width:100% !important;height:auto !important;}
}
</style>
{{.HeadStyles}}</head>
<body style="margin:0;padding:0;background-color:#f3f4f6;">
{{.Preheader}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f3f4f6;">
<tr>
<td align="center" style="padding:24px 12px;">
{{.MsoOpen}}
<table role="presentation" class="mr-container" width="600" cellpadding="0" cellspacing="0" border="0" style="width:600px;max-width:600px;background-color:#ffffff;border-top:4px solid {{.BrandColor}};border-radius:6px;">
{{- if .LogoURL}}
<tr>
<td align="center" style="padding:28px 32px 0 32px;">
<img src="{{.LogoURL}}" alt="" width="160" style="display:block;width:160px;max-width:160px;height:auto;border:0;">
</td>
</tr>
{{- end}}
<tr>
<td class="mr-pad mr-content" style="padding:32px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:16px;line-height:1.6;color:#1f2937;text-align:left;">
{{.Content}}
</td>
</tr>
</table>
<table role="presentation" class="mr-container" width="600" cellpadding="0" cellspacing="0" border="0" style="width:600px;max-width:600px;">
<tr>
<td class="mr-pad" align="center" style="padding:20px 32px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:12px;line-height:1.5;color:#6b7280;text-align:center;">
{{- if .Footer}}
<p style="margin:0 0 8px;">{{.Footer}}</p>
{{- end}}
{{- if .UnsubscribeURL}}
<p style="margin:0;"><a href="{{.UnsubscribeURL}}" style="color:#6b7280;text-decoration:underline;">Unsubscribe</a> from these emails.</p>
{{- end}}
</td>
</tr>
</table>
{{.MsoClose}}
</td>
</tr>
</table>
</body>
</html>
`))

// preheaderBlock returns the hidden preheader element followed by filler
// that keeps clients from showing body text after it in the inbox preview.
// data-mr-preheader lets the text version skip it.
func preheaderBlock(text string) string {
	if text == "" {
		return ""
	}
	return `<div data-mr-preheader="" style="display:none;font-size:1px;line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;mso-hide:all;color:transparent;">` +
		html.EscapeString(text) + strings.Repeat("&#847;&zwnj;&nbsp;", 40) + "</div>\n"
}

// footerHTML escapes the company footer and keeps its line breaks.
func footerHTML(footer string) string {
	footer = strings.TrimSpace(strings.ReplaceAll(footer, "\r\n", "\n"))
	return strings.ReplaceAll(html.EscapeString(footer), "\n", "<br>")
}

// wrapParams is what a layout needs besides the layout itself.
type wrapParams struct {
	Subject        string
	Preheader      string
	HeadStyles     string
	Content        string // HTML
	Footer         string // plain text (settings.company_footer)
	UnsubscribeURL string // "" for system emails
}

// wrapDefault renders the built-in layout.
func wrapDefault(l LayoutInput, p wrapParams) (string, error) {
	color := l.BrandColor
	if !rxColor.MatchString(color) {
		color = DefaultBrandColor
	}
	var b strings.Builder
	err := frameTemplate.Execute(&b, frame{
		Subject:        p.Subject,
		Preheader:      template.HTML(preheaderBlock(p.Preheader)),
		HeadStyles:     template.HTML(p.HeadStyles),
		LogoURL:        l.LogoURL,
		BrandColor:     template.CSS(color),
		Content:        template.HTML(p.Content),
		Footer:         template.HTML(footerHTML(p.Footer)),
		UnsubscribeURL: p.UnsubscribeURL,
		MsoOpen:        msoOpen,
		MsoClose:       msoClose,
	})
	if err != nil {
		return "", fmt.Errorf("content: render layout: %w", err)
	}
	return b.String(), nil
}

// wrapCustom renders a validated custom_html layout. {{content}} and
// {{footer}} are inserted as HTML, every other field HTML-escaped. When
// the layout doesn't place {{preheader}} itself, the hidden preheader
// block is put in front of the content. A layout without {{footer}}
// (saved before ValidateLayout required it) gets the footer appended
// after the content, so no marketing email goes out without it (E-1).
func wrapCustom(l LayoutInput, p wrapParams) (string, error) {
	t, err := parseMerge("custom_html", l.CustomHTML, layoutVocabulary)
	if err != nil {
		return "", err
	}
	content := p.Content
	if t.count(layoutPreheader) == 0 {
		content = preheaderBlock(p.Preheader) + p.HeadStyles + content
	} else {
		content = p.HeadStyles + content
	}
	if t.count(layoutFooter) == 0 && strings.TrimSpace(p.Footer) != "" {
		content += `<p style="font-size:12px;line-height:1.5;color:#6b7280;margin:24px 0 0">` + footerHTML(p.Footer) + `</p>`
	}
	rawValues := map[string]string{
		layoutContent: content,
		layoutFooter:  footerHTML(p.Footer),
	}
	textValues := map[string]string{
		layoutUnsubscribe: p.UnsubscribeURL,
		layoutPreheader:   p.Preheader,
		layoutLogoURL:     l.LogoURL,
		layoutBrandColor:  l.BrandColor,
	}
	var b strings.Builder
	for _, s := range t {
		switch {
		case s.Field == nil:
			b.WriteString(s.Text)
		case rawValues[s.Field.Name] != "":
			b.WriteString(rawValues[s.Field.Name])
		case textValues[s.Field.Name] != "":
			b.WriteString(html.EscapeString(textValues[s.Field.Name]))
		default:
			b.WriteString(html.EscapeString(s.Field.Fallback))
		}
	}
	return b.String(), nil
}

// wrap renders p in the layout: custom_html when set, else the built-in
// layout. System emails (no unsubscribe URL) always use the built-in one,
// with the layout's logo and colour, because custom layouts require an
// unsubscribe link.
//
// A custom layout saved before ValidateLayout refused hidden unsubscribe
// links and footers still shows both: when it hides one, a visible block
// with the footer and an unsubscribe link is added at the end of the body
// (E-1, E-4).
func wrap(l LayoutInput, p wrapParams) (string, error) {
	if l.CustomHTML != "" && p.UnsubscribeURL != "" {
		out, err := wrapCustom(l, p)
		if err != nil {
			return "", err
		}
		pl, err := cachedPlacement(l)
		if err != nil || (pl.unsubVisible && pl.footerVisible) {
			return out, nil
		}
		var b strings.Builder
		b.WriteString(`<p style="font-size:12px;line-height:1.5;color:#6b7280;margin:24px 0 0">`)
		if !pl.footerVisible && strings.TrimSpace(p.Footer) != "" {
			b.WriteString(footerHTML(p.Footer))
			if !pl.unsubVisible {
				b.WriteString("<br>")
			}
		}
		if !pl.unsubVisible {
			b.WriteString(`<a href="` + html.EscapeString(p.UnsubscribeURL) + `" style="color:#6b7280;">Unsubscribe from these emails</a>`)
		}
		b.WriteString(`</p>`)
		if i := strings.LastIndex(strings.ToLower(out), "</body>"); i >= 0 {
			return out[:i] + b.String() + out[i:], nil
		}
		return out + b.String(), nil
	}
	return wrapDefault(l, p)
}

// placementCache holds the placement of the last custom layout checked at
// render time (a broadcast renders the same layout for every recipient).
var placementCache struct {
	sync.Mutex
	html string
	pl   layoutPlacement
}

// cachedPlacement returns placement(l), computed once per custom_html.
func cachedPlacement(l LayoutInput) (layoutPlacement, error) {
	placementCache.Lock()
	defer placementCache.Unlock()
	if placementCache.html == l.CustomHTML {
		return placementCache.pl, nil
	}
	pl, err := placement(LayoutInput{CustomHTML: l.CustomHTML})
	if err != nil {
		return pl, err
	}
	placementCache.html, placementCache.pl = l.CustomHTML, pl
	return pl, nil
}
