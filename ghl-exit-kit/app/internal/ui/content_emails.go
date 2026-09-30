package ui

// The Content section: the email library (E-2, E-7: the content of
// broadcasts and sequence steps, and emails no step or broadcast uses any
// more), uploaded images (E-6) and, registered by the broadcasts group,
// the base layout. Each page has the section tabs.

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"mailroom/internal/api"
	"mailroom/internal/content"
	"mailroom/internal/core"
)

// Content section paths.
const (
	emailsPath = Prefix + "/emails"
	mediaPath  = Prefix + "/media"
)

func emailURL(id int64, suffix string) string { return fmt.Sprintf("%s/%d%s", emailsPath, id, suffix) }

func init() {
	registerGroup("content", func(u *UI) {
		u.registerEmails()
		u.registerMedia()
	})
}

func (u *UI) registerEmails() {
	u.handle(Action{Method: http.MethodGet, Path: emailsPath, OperationID: "list-emails", Handler: u.emailsList})
	u.handle(Action{Method: http.MethodGet, Path: emailsPath + "/new",
		UIOnly: "the empty new-email form; saving it is create-email", Handler: u.emailNew})
	u.handle(Action{Method: http.MethodPost, Path: emailsPath, OperationID: "create-email",
		MaxBodyBytes: content.MaxBodyBytes + 64<<10, Handler: u.emailCreate})
	u.handle(Action{Method: http.MethodGet, Path: emailsPath + "/{id}", OperationID: "get-email",
		Uses: []string{"get-email-usage"}, Handler: u.emailShow})
	u.handle(Action{Method: http.MethodPost, Path: emailsPath + "/{id}", OperationID: "update-email",
		Uses: []string{"get-email-usage"}, MaxBodyBytes: content.MaxBodyBytes + 64<<10, Handler: u.emailUpdate})
	u.handle(Action{Method: http.MethodPost, Path: emailsPath + "/{id}/test", OperationID: "send-test-email",
		Uses: []string{"get-email", "get-email-usage", "list-contacts", "get-contact"}, Handler: u.emailTest})
	u.handle(Action{Method: http.MethodPost, Path: emailsPath + "/{id}/delete", OperationID: "delete-email",
		Uses: []string{"get-email", "get-email-usage"}, Handler: u.emailDelete})
}

// SubnavItem is one tab of template "subnav".
type SubnavItem struct {
	Label   string
	URL     string
	Current bool
}

// contentTabs are the tabs of the Content section, with cur ("emails",
// "media" or "layout") highlighted.
func contentTabs(cur string) []SubnavItem {
	tabs := []SubnavItem{
		{Label: "Emails", URL: emailsPath},
		{Label: "Images", URL: mediaPath},
		{Label: "Base layout", URL: layoutPath},
	}
	for i := range tabs {
		tabs[i].Current = strings.HasSuffix(tabs[i].URL, "/"+cur)
	}
	return tabs
}

// ---------------------------------------------------------------- list

type emailListData struct {
	Tabs  []SubnavItem
	Items []content.EmailSummary
	Pager Pager
	Query string
}

// emailsList lists the email library (list-emails), searchable by name
// or subject.
func (u *UI) emailsList(w http.ResponseWriter, r *http.Request) error {
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) > 200 {
		return core.Invalid("q", "must be at most 200 characters")
	}
	items, err := u.d.Content.ListEmails(r.Context(), content.ListFilter{AfterID: p.After, Limit: p.Fetch(), Query: q})
	if err != nil {
		return err
	}
	items, pager := Paginate(r, p, items, func(e content.EmailSummary) int64 { return e.ID })
	return u.Page(w, r, "content/emails", View{
		Title: "Emails", Nav: "content",
		Subtitle: "Every email Mailroom has: the content of broadcasts and sequence steps, and drafts you keep for later.",
		Data:     emailListData{Tabs: contentTabs("emails"), Items: items, Pager: pager, Query: q},
	})
}

// ---------------------------------------------------------------- edit

type emailPageData struct {
	Tabs     []SubnavItem
	Form     *Form
	Email    *content.Email // nil on the new page
	Usage    content.EmailUsage
	Locked   *content.EmailBroadcastUse // the broadcast past scheduled that owns it: no edits
	TestForm *Form
	Owner    string // settings.owner_email, the default test address
}

// emailForm fills an edit form from e.
func emailForm(e content.Email) *Form {
	return EmptyForm().
		Set("name", e.Name).
		Set("subject", e.Subject).
		Set("preheader", e.Preheader).
		Set("format", e.Format).
		Set("body", e.Body).
		Set("version", strconv.FormatInt(e.Version, 10))
}

// renderEmail renders the new page (e nil) or the edit page of e. f and
// tf are the edit and test forms (nil: from e and empty).
func (u *UI) renderEmail(w http.ResponseWriter, r *http.Request, status int, e *content.Email, f, tf *Form) error {
	ctx := r.Context()
	d := emailPageData{Tabs: contentTabs("emails"), Form: f, Email: e, TestForm: tf}
	if d.TestForm == nil {
		d.TestForm = EmptyForm()
	}
	if st, err := u.d.Settings.Get(ctx); err == nil {
		d.Owner = st.OwnerEmail
	}
	v := View{Title: "New email", Nav: "content", Crumbs: []Crumb{{"Emails", emailsPath}}}
	if e == nil {
		if d.Form == nil {
			d.Form = EmptyForm().Set("format", content.FormatMarkdown)
		}
		v.Subtitle = "A stand-alone email. Use it later as a sequence step, or keep it as a template to copy from."
		v.Data = d
		return u.PageStatus(w, r, status, "content/email", v)
	}
	usage, err := u.d.Content.EmailUsage(ctx, e.ID)
	if err != nil {
		return err
	}
	d.Usage = usage
	for i, b := range usage.Broadcasts {
		if b.Status != "draft" && b.Status != "scheduled" {
			d.Locked = &usage.Broadcasts[i]
			break
		}
	}
	if d.Form == nil {
		d.Form = emailForm(*e)
	}
	v.Title = defaultString(e.Name, e.Subject)
	v.Subtitle = "Changes apply to every future send of this email, in every step and broadcast that uses it."
	v.Data = d
	return u.PageStatus(w, r, status, "content/email", v)
}

// emailNew shows the empty editor.
func (u *UI) emailNew(w http.ResponseWriter, r *http.Request) error {
	return u.renderEmail(w, r, http.StatusOK, nil, nil, nil)
}

// emailInput reads the content fields of the editor form.
func emailInput(f *Form) content.EmailInput {
	return content.EmailInput{Name: f.Get("name"), Subject: f.Get("subject"), Preheader: f.Get("preheader"),
		Format: f.Get("format"), Body: f.Raw("body")}
}

// emailCreate saves a new email (create-email).
func (u *UI) emailCreate(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	ctx := r.Context()
	var e content.Email
	err := u.tx(ctx, func(tx *sql.Tx) error {
		var err error
		e, err = u.d.Content.CreateEmail(ctx, tx, emailInput(f))
		return err
	})
	if err != nil {
		st := f.Fail(err)
		if st == 0 {
			return err
		}
		return u.renderEmail(w, r, st, nil, f, nil)
	}
	u.Flash(r, FlashSuccess, "Email saved. Send yourself a test to see it in your inbox.")
	u.Redirect(w, r, emailURL(e.ID, ""))
	return nil
}

// emailShow shows the editor of a stored email (get-email) with what
// uses it (get-email-usage).
func (u *UI) emailShow(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	e, err := u.d.Content.GetEmail(r.Context(), nil, id)
	if err != nil {
		return err
	}
	return u.renderEmail(w, r, http.StatusOK, &e, nil, nil)
}

// emailUpdate saves the editor (update-email) with the version the page
// was loaded at as If-Match.
func (u *UI) emailUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	in := emailInput(f)
	var e content.Email
	err = u.tx(ctx, func(tx *sql.Tx) error {
		var err error
		e, err = u.d.Content.UpdateEmail(ctx, tx, id, content.EmailPatch{
			Name: &in.Name, Subject: &in.Subject, Preheader: &in.Preheader, Format: &in.Format, Body: &in.Body,
		}, formVersion(f))
		return err
	})
	if err == nil {
		u.Flash(r, FlashSuccess, "Email saved.")
		u.Redirect(w, r, emailURL(e.ID, ""))
		return nil
	}
	st := f.Fail(err)
	if st == 0 {
		return err
	}
	cur, gerr := u.d.Content.GetEmail(ctx, nil, id)
	if gerr != nil {
		return gerr
	}
	if st == http.StatusPreconditionFailed {
		f.Set("version", strconv.FormatInt(cur.Version, 10))
	}
	return u.renderEmail(w, r, st, &cur, f, nil)
}

// emailTest queues a test of the stored email (send-test-email) to up to
// 5 addresses (the owner's when empty), optionally with a contact's data
// (by email address or ID).
func (u *UI) emailTest(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	e, err := u.d.Content.GetEmail(ctx, nil, id)
	if err != nil {
		return err
	}
	tf := NewForm(r)
	to := splitAddresses(tf.Get("to"))
	var contactID int64
	c, err := u.lookupContact(ctx, "contact", tf.Get("contact"))
	switch {
	case err == nil && c != nil:
		contactID = c.ID
	case err != nil:
		if tf.Fail(err) == 0 {
			return err
		}
		return u.renderEmail(w, r, http.StatusUnprocessableEntity, &e, nil, tf)
	}
	var sent []string
	err = u.tx(ctx, func(tx *sql.Tx) error {
		var err error
		_, sent, err = u.d.Content.SendTest(ctx, tx, id, to, contactID)
		return err
	})
	if err != nil {
		st := tf.Fail(autoRetarget(err, autoFoldIndex("to", to)))
		if st == 0 {
			return err
		}
		return u.renderEmail(w, r, st, &e, nil, tf)
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("Test queued to %s. Tests are skipped for opted-out or suppressed addresses, have no working unsubscribe or tracking links and don't count in the stats.",
		strings.Join(sent, ", ")))
	u.Redirect(w, r, emailURL(id, ""))
	return nil
}

// emailUsageLines describes what uses an email, for the delete refusal.
func emailUsageLines(usage content.EmailUsage) []string {
	var out []string
	for _, s := range usage.Steps {
		out = append(out, fmt.Sprintf("Step %d of the sequence %q", s.Position, s.SequenceName))
	}
	for _, b := range usage.Broadcasts {
		out = append(out, fmt.Sprintf("The broadcast %q (%s)", b.Name, b.Status))
	}
	if usage.PendingMessages > 0 {
		out = append(out, plural(usage.PendingMessages, "message with it is", "messages with it are")+" still waiting to be sent")
	}
	return out
}

// emailDelete deletes an email (delete-email) after a confirmation. The
// first step runs the delete in a rolled-back transaction (a dry run), so
// an email still in use is refused before anything is asked.
func (u *UI) emailDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	e, err := u.d.Content.GetEmail(ctx, nil, id)
	if err != nil {
		return err
	}
	f := NewForm(r)
	ver := formVersion(f)
	del := func(dryRun bool) error {
		return api.Tx(ctx, u.d.DB, dryRun, func(tx *sql.Tx) error { return u.d.Content.DeleteEmail(ctx, tx, id, ver) })
	}
	if !Confirmed(r) {
		if err := del(true); err != nil {
			st := f.Fail(err)
			if st == 0 {
				return err
			}
			form := emailForm(e)
			form.Message = f.Message
			if st == http.StatusConflict {
				usage, uerr := u.d.Content.EmailUsage(ctx, id)
				if uerr != nil {
					return uerr
				}
				form.Message = "This email can't be deleted while it is in use: " + strings.Join(emailUsageLines(usage), "; ") +
					". Remove it from those steps and broadcasts first (a sent broadcast keeps its email for good)."
			}
			return u.renderEmail(w, r, st, &e, form, nil)
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:        fmt.Sprintf("Delete %q?", defaultString(e.Name, e.Subject)),
			Message:      "This deletes the email for good. No sequence step or broadcast uses it. It can't be undone.",
			Details:      []string{"Subject: " + e.Subject, "Last changed: " + u.fmtTime(e.UpdatedAt)},
			Action:       emailURL(id, "/delete"),
			Fields:       url.Values{"version": {strconv.FormatInt(e.Version, 10)}},
			ConfirmLabel: "Delete email",
			CancelURL:    emailURL(id, ""),
			Danger:       true,
			Nav:          "content",
			Crumbs:       []Crumb{{"Emails", emailsPath}},
		})
	}
	if err := del(false); err != nil {
		if f.Fail(err) == 0 {
			return err
		}
		u.Flash(r, FlashError, "The email was not deleted: "+f.Message)
		u.Redirect(w, r, emailURL(id, ""))
		return nil
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("Email %q deleted.", defaultString(e.Name, e.Subject)))
	u.Redirect(w, r, emailsPath)
	return nil
}
