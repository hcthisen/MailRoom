package ui

import (
	"net/http"
	"strings"

	"mailroom/internal/content"
	"mailroom/internal/core"
)

// PreviewEmailPath is the action the email editor's live preview posts
// to (template "email-editor").
const PreviewEmailPath = Prefix + "/preview/email"

// registerFoundation registers the actions of the foundation: sign-in,
// static files and the email editor preview.
func (u *UI) registerFoundation() {
	u.registerAuth()
	u.handle(Action{Method: http.MethodGet, Path: Prefix + "/static/{name}", Public: true,
		UIOnly: "embedded CSS and JavaScript of the UI itself", Handler: u.serveStatic})
	u.handle(Action{Method: http.MethodPost, Path: PreviewEmailPath, OperationID: "render-email",
		MaxBodyBytes: content.MaxBodyBytes + 64<<10, Handler: u.previewEmail})
}

// previewData is the email-preview fragment's data.
type previewData struct {
	Subject   string
	From      string // the sender name the inbox row shows
	Preheader string // the preview text, rendered
	HTML      string // rendered into the sandboxed iframe's srcdoc (escaped by html/template)
	Text      string
	Error     string
}

// previewEmail renders the editor's unsaved content (content.RenderDraft,
// the method behind POST /emails/render) into the preview panel. Merge
// fields use their fallbacks. Validation errors are shown in the panel.
func (u *UI) previewEmail(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	format := f.Get("format")
	if format == "" {
		format = content.FormatMarkdown
	}
	// "Preview as": the contact picked in the preview panel (an ID), so
	// merge fields show real data instead of their fallbacks.
	pc := f.Get("preview_contact")
	if pc == "0" {
		pc = ""
	}
	c, err := u.lookupContact(r.Context(), "preview_contact", pc)
	// An empty draft still shows the layout, with placeholders.
	subject, body := f.Get("subject"), f.Raw("body")
	if strings.TrimSpace(subject) == "" {
		subject = "(No subject yet)"
	}
	if strings.TrimSpace(body) == "" {
		format, body = content.FormatHTML, `<p style="color:#9ca3af">Your content appears here as you write.</p>`
	}
	var out core.Rendered
	if err == nil {
		out, err = u.d.Content.RenderDraft(r.Context(), content.Draft{
			Subject: subject, Preheader: f.Get("preheader"), Format: format, Body: body,
		}, c, nil)
	}
	var d previewData
	if err != nil {
		if f.Fail(err) == 0 {
			return err
		}
		d.Error = f.Message
		for field, msg := range f.Errors {
			d.Error = fieldLabel(field) + ": " + msg
		}
	} else {
		d = previewData{Subject: out.Subject, HTML: out.HTML, Text: out.Text, From: f.Get("from_name")}
		d.Preheader, _ = content.PreviewLine("preheader", f.Get("preheader"), c)
		if d.From == "" {
			if st, err := u.d.Settings.Get(r.Context()); err == nil {
				d.From = st.Sender.FromName
			}
		}
	}
	return u.Partial(w, r, "shared/preview", "email-preview", View{Data: d})
}
