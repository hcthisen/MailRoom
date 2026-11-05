package ui

// The Inbox (PRD §6.16, N-1..N-6): email received for the inbound domain, read
// in the admin, answered with a reply, archived or deleted.

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"mailroom/internal/core"
	"mailroom/internal/inbound"
)

const inboxPath = Prefix + "/inbox"

func init() {
	registerGroup("inbox", func(u *UI) {
		if u.d.Inbound == nil {
			return
		}
		get, post := http.MethodGet, http.MethodPost
		u.handle(Action{Method: get, Path: inboxPath, OperationID: "list-inbound", Uses: []string{"get-inbound-counts"}, Handler: u.inboxList})
		u.handle(Action{Method: get, Path: inboxPath + "/{id}", OperationID: "get-inbound",
			Uses: []string{"get-contact"}, Handler: u.inboxShow})
		u.handle(Action{Method: post, Path: inboxPath + "/{id}/flags", OperationID: "update-inbound", Handler: u.inboxFlags})
		u.handle(Action{Method: post, Path: inboxPath + "/{id}/delete", OperationID: "delete-inbound", Handler: u.inboxDelete})
		u.handle(Action{Method: post, Path: inboxPath + "/{id}/reply", OperationID: "reply-inbound", Uses: []string{"get-inbound"}, Handler: u.inboxReply})
		u.handle(Action{Method: get, Path: inboxPath + "/{id}/attachments/{position}", OperationID: "get-inbound-attachment",
			Handler: u.inboxAttachment})
	})
}

// inboxBoxes are the inbox tabs: query value and label.
var inboxBoxes = [][2]string{{"inbox", "Inbox"}, {"unread", "Unread"}, {"archived", "Archived"}, {"all", "All"}}

type inboxListData struct {
	Counts   inbound.Counts
	Box      string
	Tabs     []SubnavItem
	Query    string
	Items    []inbound.Summary
	Pager    Pager
	Domain   string
	Disabled bool
}

// inboxList is the inbox (list-inbound), newest first.
func (u *UI) inboxList(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	q := r.URL.Query()
	box := q.Get("box")
	if box == "" {
		box = "inbox"
	}
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	counts, err := u.d.Inbound.Counts(ctx)
	if err != nil {
		return err
	}
	d := inboxListData{Counts: counts, Box: box, Query: q.Get("q"), Domain: counts.Domain, Disabled: !counts.Enabled}
	for _, b := range inboxBoxes {
		label := b[1]
		if b[0] == "unread" && counts.Unread > 0 {
			label += " (" + strconv.Itoa(counts.Unread) + ")"
		}
		v := url.Values{"box": {b[0]}}
		if d.Query != "" {
			v.Set("q", d.Query)
		}
		d.Tabs = append(d.Tabs, SubnavItem{Label: label, URL: inboxPath + "?" + v.Encode(), Current: b[0] == box})
	}
	items, err := u.d.Inbound.List(ctx, inbound.ListFilter{Box: box, Query: d.Query, BeforeID: p.After, Limit: p.Fetch()})
	if err != nil {
		if errors.Is(err, core.ErrValidation) {
			return core.ErrNotFound
		}
		return err
	}
	d.Items, d.Pager = Paginate(r, p, items, func(m inbound.Summary) int64 { return m.ID })
	sub := "Email sent to addresses on your domain."
	if counts.Domain != "" {
		sub = "Email sent to any address on " + counts.Domain + "."
	}
	return u.Page(w, r, "inbox/list", View{Title: "Inbox", Subtitle: sub, Nav: "inbox", Data: d})
}

type inboxShowData struct {
	M          inbound.Message
	Display    inbound.Display
	ShowImages bool
	Contact    *contactCard
	ReplyForm  *Form
	ReplyTo    string
	Version    string
}

type contactCard struct {
	ID     int64
	Name   string
	Email  string
	Status string
	Tags   []string
}

// inboxShow shows one received message (get-inbound). The page marks it
// read with an htmx POST to the flags action once it is shown, since a
// GET must not change data.
func (u *UI) inboxShow(w http.ResponseWriter, r *http.Request) error {
	return u.inboxRenderShow(w, r, http.StatusOK, nil)
}

func (u *UI) inboxRenderShow(w http.ResponseWriter, r *http.Request, status int, reply *Form) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	m, err := u.d.Inbound.Get(ctx, id)
	if err != nil {
		return err
	}
	d := inboxShowData{M: m, ShowImages: r.URL.Query().Get("images") == "1", ReplyForm: reply,
		Version: strconv.FormatInt(m.Version, 10)}
	if m.HTML != "" {
		d.Display = u.d.Inbound.DisplayHTML(ctx, m, d.ShowImages)
	}
	d.ReplyTo = m.ReplyTo
	if d.ReplyTo == "" {
		d.ReplyTo = m.FromEmail
	}
	if d.ReplyForm == nil {
		d.ReplyForm = EmptyForm()
		d.ReplyForm.Values.Set("quote", "1")
	}
	if m.ContactID != nil && u.d.Contacts != nil {
		if c, err := u.d.Contacts.Get(ctx, *m.ContactID); err == nil {
			name := c.FirstName
			if c.LastName != "" {
				name += " " + c.LastName
			}
			d.Contact = &contactCard{ID: c.ID, Name: name, Email: c.Email, Status: string(c.Status), Tags: c.Tags}
		}
	}
	title := m.Subject
	if title == "" {
		title = "(no subject)"
	}
	return u.PageStatus(w, r, status, "inbox/show", View{Title: title, Nav: "inbox",
		Crumbs: []Crumb{{"Inbox", inboxPath}}, Data: d})
}

// inboxFlags marks a message read, unread, archived or back in the inbox
// (update-inbound). htmx calls with action=read answer 204.
func (u *UI) inboxFlags(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	t, fl := true, false
	var upd inbound.Update
	msg := ""
	switch f.Get("action") {
	case "read":
		upd.Read = &t
	case "unread":
		upd.Read, msg = &fl, "Marked as unread."
	case "archive":
		upd.Archived, msg = &t, "Archived."
	case "unarchive":
		upd.Archived, msg = &fl, "Moved back to the inbox."
	default:
		return core.ErrNotFound
	}
	if _, err := u.d.Inbound.Update(r.Context(), id, upd, 0); err != nil {
		return err
	}
	if r.Header.Get("HX-Request") == "true" && f.Get("action") == "read" {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	u.Flash(r, FlashSuccess, msg)
	target := inboxPath + "/" + strconv.FormatInt(id, 10)
	if f.Get("action") != "unarchive" && f.Get("action") != "read" {
		target = inboxPath
	}
	u.Redirect(w, r, target)
	return nil
}

// inboxDelete deletes a message for good (delete-inbound) after a
// confirmation.
func (u *UI) inboxDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	v, _ := strconv.ParseInt(f.Get("version"), 10, 64)
	if !Confirmed(r) {
		m, err := u.d.Inbound.Get(r.Context(), id)
		if err != nil {
			return err
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:   "Delete this email?",
			Message: "“" + m.Subject + "” from " + m.FromEmail + " and its attachments are deleted for good. Replies you sent stay in the message log.",
			Action:  inboxPath + "/" + strconv.FormatInt(id, 10) + "/delete", Fields: f.Values, ConfirmLabel: "Delete email",
			CancelURL: inboxPath + "/" + strconv.FormatInt(id, 10), Danger: true, Nav: "inbox",
			Crumbs: []Crumb{{"Inbox", inboxPath}},
		})
	}
	if err := u.d.Inbound.Delete(r.Context(), id, v); err != nil {
		if errors.Is(err, core.ErrPrecondition) {
			u.Flash(r, FlashError, "The email changed in the meantime. Look at it again, then delete it.")
			u.Redirect(w, r, inboxPath+"/"+strconv.FormatInt(id, 10))
			return nil
		}
		return err
	}
	u.Flash(r, FlashSuccess, "The email was deleted.")
	u.Redirect(w, r, inboxPath)
	return nil
}

// inboxReply queues a reply (reply-inbound).
func (u *UI) inboxReply(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	quote := f.Checked("quote")
	res, err := u.d.Inbound.Reply(r.Context(), id, inbound.ReplyInput{Body: f.Raw("body"), Subject: f.Get("subject"), Quote: &quote}, false)
	if err != nil {
		if st := f.Fail(err); st != 0 {
			return u.inboxRenderShow(w, r, st, f)
		}
		return err
	}
	u.Flash(r, FlashSuccess, "Reply to "+res.To+" queued. It shows as sent below once it has gone out.")
	u.Redirect(w, r, inboxPath+"/"+strconv.FormatInt(id, 10))
	return nil
}

// inboxAttachment downloads an attachment (get-inbound-attachment).
// Images may be shown inline (?inline=1); everything else is a download.
func (u *UI) inboxAttachment(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	pos, err := strconv.Atoi(r.PathValue("position"))
	if err != nil || pos < 1 {
		return core.ErrNotFound
	}
	a, data, err := u.d.Inbound.AttachmentData(r.Context(), id, pos)
	if err != nil {
		return err
	}
	inbound.WriteAttachmentHeaders(w.Header().Set, a, len(data), r.URL.Query().Get("inline") == "1")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	return nil
}
