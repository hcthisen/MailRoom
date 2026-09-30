package ui

import (
	"net/http"

	"mailroom/internal/content"
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
	Subject string
	HTML    string // rendered into the sandboxed iframe's srcdoc (escaped by html/template)
	Text    string
	Error   string
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
	out, err := u.d.Content.RenderDraft(r.Context(), content.Draft{
		Subject: f.Get("subject"), Preheader: f.Get("preheader"), Format: format, Body: f.Raw("body"),
	}, nil, nil)
	var d previewData
	if err != nil {
		if f.Fail(err) == 0 {
			return err
		}
		d.Error = f.Message
		for field, msg := range f.Errors {
			d.Error = field + ": " + msg
		}
	} else {
		d = previewData{Subject: out.Subject, HTML: out.HTML, Text: out.Text}
	}
	return u.Partial(w, r, "shared/preview", "email-preview", View{Data: d})
}
