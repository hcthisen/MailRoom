package ui

// The email editor's helpers (template "email-editor", static/editor.js):
// the contacts a preview can show, the merge-field menu, the image
// picker, Markdown → visual conversion and the "Send test" button, which
// works before the content is saved.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"mailroom/internal/broadcasts"
	"mailroom/internal/contacts"
	"mailroom/internal/content"
	"mailroom/internal/core"
)

const editorPath = Prefix + "/editor"

// previewContactLimit is how many contacts the "Preview as" picker offers.
const previewContactLimit = 25

func (u *UI) registerEditor() {
	get, post := http.MethodGet, http.MethodPost
	u.handle(Action{Method: get, Path: editorPath + "/contacts", OperationID: "list-contacts", Handler: u.editorContacts})
	u.handle(Action{Method: get, Path: editorPath + "/fields", OperationID: "list-merge-fields", Handler: u.editorFields})
	u.handle(Action{Method: get, Path: editorPath + "/media", OperationID: "list-media", Handler: u.editorMedia})
	u.handle(Action{Method: post, Path: editorPath + "/media", OperationID: "upload-media",
		MaxBodyBytes: mediaUploadLimit, Handler: u.editorMediaUpload})
	u.handle(Action{Method: post, Path: editorPath + "/convert", OperationID: "render-email",
		MaxBodyBytes: content.MaxBodyBytes + 64<<10, Handler: u.editorConvert})
	u.handle(Action{Method: post, Path: editorPath + "/test", OperationID: "send-test-draft", Uses: []string{"get-contact"},
		MaxBodyBytes: content.MaxBodyBytes + 64<<10, Handler: u.editorTest})
}

// editorContactsData feeds the "Preview as" picker.
type editorContactsData struct {
	Contacts []contacts.Contact
	Selected int64
	Segment  bool
	Error    string
}

// editorContacts lists active contacts of the audience (?audience=all or
// segment:N; other editors: everyone) for the preview picker, newest
// first. The first one is selected unless ?selected= names another.
func (u *UI) editorContacts(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f := contacts.ListFilter{Status: core.StatusActive, Limit: previewContactLimit}
	var d editorContactsData
	if v := q.Get("audience"); v != "" {
		aud, err := parseAudience(v)
		if err != nil {
			d.Error = "Choose an audience first."
		} else if aud.Type == broadcasts.AudienceSegment {
			f.SegmentID, d.Segment = aud.SegmentID, true
		}
	}
	if d.Error == "" {
		cs, err := u.d.Contacts.List(r.Context(), f)
		switch {
		case errors.Is(err, core.ErrNotFound):
			d.Error = "This segment no longer exists."
		case err != nil:
			return err
		default:
			d.Contacts = cs
		}
	}
	if id, err := strconv.ParseInt(q.Get("preview_contact"), 10, 64); err == nil && id > 0 {
		for _, c := range d.Contacts {
			if c.ID == id {
				d.Selected = id
			}
		}
	}
	if d.Selected == 0 && len(d.Contacts) > 0 && q.Get("preview_contact") != "0" {
		// Prefer a contact with a first name: it shows personalisation best.
		d.Selected = d.Contacts[0].ID
		for _, c := range d.Contacts {
			if c.FirstName != "" {
				d.Selected = c.ID
				break
			}
		}
	}
	return u.Partial(w, r, "shared/preview", "editor-contacts", View{Data: d})
}

// editorFields is the merge-field menu.
func (u *UI) editorFields(w http.ResponseWriter, r *http.Request) error {
	fs, err := u.d.Content.MergeFields(r.Context())
	if err != nil {
		return err
	}
	return u.Partial(w, r, "shared/preview", "editor-fields", View{Data: fs})
}

// editorMediaData is the image picker.
type editorMediaData struct {
	Items []content.Media
	Error string
}

const editorMediaLimit = 60

func (u *UI) editorMedia(w http.ResponseWriter, r *http.Request) error {
	return u.renderEditorMedia(w, r, "")
}

func (u *UI) renderEditorMedia(w http.ResponseWriter, r *http.Request, msg string) error {
	items, err := u.d.Content.ListMedia(r.Context(), 0, editorMediaLimit)
	if err != nil {
		return err
	}
	images := items[:0]
	for _, m := range items {
		if strings.HasPrefix(m.ContentType, "image/") {
			images = append(images, m)
		}
	}
	return u.Partial(w, r, "shared/preview", "editor-media", View{Data: editorMediaData{Items: images, Error: msg}})
}

// editorMediaUpload uploads an image from the picker and shows the picker
// again with it first.
func (u *UI) editorMediaUpload(w http.ResponseWriter, r *http.Request) error {
	file, hdr, err := r.FormFile("file")
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) || errors.Is(err, http.ErrNotMultipart) {
			return u.renderEditorMedia(w, r, "Choose an image file first.")
		}
		return err
	}
	defer file.Close()
	if _, err := u.d.Content.UploadMedia(r.Context(), hdr.Filename, file); err != nil {
		f := EmptyForm()
		if f.Fail(err) == 0 {
			return err
		}
		msg := f.Message
		for _, m := range f.Errors {
			msg = m
		}
		return u.renderEditorMedia(w, r, "The image was not uploaded: "+msg)
	}
	return u.renderEditorMedia(w, r, "")
}

// editorConvert turns a Markdown body into visual-editor HTML. It answers
// the HTML as text/plain (editor.js reads it), or 422 with the reason.
func (u *UI) editorConvert(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	out, err := u.d.Content.MarkdownToHTMLBody(f.Raw("body"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		if f.Fail(err) == 0 {
			return err
		}
		msg := f.Message
		for _, m := range f.Errors {
			msg = m
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(msg))
		return nil
	}
	_, _ = w.Write([]byte(out))
	return nil
}

// editorTestData is the test-send result shown in the editor's test box.
type editorTestData struct {
	OK      bool
	Message string
}

// editorTest sends the editor's current, possibly unsaved content as a
// test (send-test-draft) to the addresses in test_to, with the data of the
// contact chosen in "Preview as" and the form's sender fields.
func (u *UI) editorTest(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	format := f.Get("format")
	if format == "" {
		format = content.FormatMarkdown
	}
	to := splitAddresses(f.Get("test_to"))
	var contactID int64
	if id, err := strconv.ParseInt(f.Get("preview_contact"), 10, 64); err == nil && id > 0 {
		contactID = id
	}
	var sender content.SenderOverride
	if v := f.Get("from_name"); v != "" {
		sender.FromName = &v
	}
	if f.Get("no_reply_to") != "" {
		empty := ""
		sender.ReplyTo = &empty
	} else if v := f.Get("reply_to"); v != "" {
		sender.ReplyTo = &v
	}
	d := content.Draft{Subject: f.Get("subject"), Preheader: f.Get("preheader"), Format: format, Body: f.Raw("body")}
	var addrs []string
	err := u.tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		_, addrs, err = u.d.Content.SendTestDraft(r.Context(), tx, d, to, contactID, sender)
		return err
	})
	var res editorTestData
	if err != nil {
		if f.Fail(err) == 0 {
			return err
		}
		res.Message = f.Message
		for field, m := range f.Errors {
			res.Message = fieldLabel(field) + ": " + m
		}
	} else {
		msg := fmt.Sprintf("Test queued for %s. It arrives in a minute or so.", strings.Join(addrs, ", "))
		if u.d.Config.AWSAccessKeyID == "" || u.d.Config.AWSRegion == "" {
			msg = fmt.Sprintf("Test queued for %s, but email sending isn't connected yet (Amazon SES), so it goes out once that is set up.", strings.Join(addrs, ", "))
		}
		res = editorTestData{OK: true, Message: msg}
	}
	return u.Partial(w, r, "shared/preview", "editor-test-result", View{Data: res})
}

// fieldLabel names a form field for a message.
func fieldLabel(field string) string {
	switch {
	case field == "to" || strings.HasPrefix(field, "to["):
		return "Send to"
	case field == "contact_id":
		return "Preview as"
	case field == "":
		return "Error"
	}
	return strings.ToUpper(field[:1]) + strings.ReplaceAll(field[1:], "_", " ")
}

// formProblem is a failed form's message in one line: its first field
// error ("Send to: is not a valid email address"), else its message.
func formProblem(f *Form) string {
	for field, m := range f.Errors {
		return fieldLabel(field) + ": " + m
	}
	return f.Message
}
