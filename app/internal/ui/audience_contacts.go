package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/contacts"
	"mailroom/internal/core"
)

// ---------------------------------------------------------------- list

type audContactsData struct {
	Tabs     template.HTML
	Filter   *Form // the query parameters
	Contacts []contacts.Contact
	Pager    Pager
	Tags     []string
	Segments []Option
	Statuses []Option
	Filtered bool   // any filter is set
	Back     string // this page's address, for the bulk form
}

// audContactsList is the contacts list with search and filters
// (list-contacts) and the bulk tag form for selected contacts.
func (u *UI) audContactsList(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	q := r.URL.Query()
	filter := &Form{Values: q, Errors: map[string]string{}}
	d := audContactsData{Tabs: audTabs("contacts"), Filter: filter, Statuses: audStatusOptions("Any status"), Tags: u.audTagNames(ctx),
		Back: r.URL.RequestURI()}
	d.Segments = []Option{{Value: "", Label: "Any segment"}}
	if segs, err := u.d.Segments.List(ctx, 0, MaxPageSize); err == nil {
		for _, s := range segs {
			d.Segments = append(d.Segments, Option{Value: strconv.FormatInt(s.ID, 10), Label: s.Name})
		}
	}
	for _, k := range []string{"q", "status", "tag", "segment_id", "created_after", "created_before"} {
		if filter.Get(k) != "" {
			d.Filtered = true
		}
	}

	status := http.StatusOK
	list, err := u.audListContacts(r, filter)
	if err != nil {
		st := filter.Fail(err)
		if st == 0 {
			return err
		}
		status = st
		if filter.Message == "" || strings.HasPrefix(filter.Message, "Please correct") {
			filter.Message = audFormMessage(filter)
		}
	}
	d.Contacts, d.Pager = list.items, list.pager
	return u.PageStatus(w, r, status, "audience/contacts", View{Title: "Contacts", Nav: "contacts", Data: d})
}

type audContactPage struct {
	items []contacts.Contact
	pager Pager
}

func (u *UI) audListContacts(r *http.Request, filter *Form) (audContactPage, error) {
	p, err := ParseList(r)
	if err != nil {
		return audContactPage{}, err
	}
	lf := contacts.ListFilter{
		Query: filter.Get("q"), Status: core.ContactStatus(filter.Get("status")), Tag: filter.Get("tag"),
		BeforeID: p.After, Limit: p.Fetch(),
	}
	if s := filter.Get("segment_id"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id < 1 {
			return audContactPage{}, fieldError("segment_id", "is not a segment")
		}
		lf.SegmentID = id
	}
	if lf.CreatedAfter, err = u.audParseDate("created_after", filter.Get("created_after")); err != nil {
		return audContactPage{}, err
	}
	before, err := u.audParseDate("created_before", filter.Get("created_before"))
	if err != nil {
		return audContactPage{}, err
	}
	if before != nil {
		// "Created up to and including" this day.
		t := before.Add(24 * time.Hour)
		lf.CreatedBefore = &t
	}
	cs, err := u.d.Contacts.List(r.Context(), lf)
	if err != nil {
		return audContactPage{}, audQueryField(err)
	}
	items, pager := Paginate(r, p, cs, func(c contacts.Contact) int64 { return c.ID })
	return audContactPage{items: items, pager: pager}, nil
}

// audQueryField drops the "query." prefix of a list filter's validation
// error, so it lands on the filter input of the same name.
func audQueryField(err error) error {
	var ve *core.ValidationError
	if errors.As(err, &ve) && strings.HasPrefix(ve.Field, "query.") {
		return &core.ValidationError{Field: strings.TrimPrefix(ve.Field, "query."), Message: ve.Message}
	}
	return err
}

// ---------------------------------------------------------------- create

type audContactFormData struct {
	Form     *Form
	Tags     []string
	Statuses []Option
	Contact  *contacts.Contact // edit only
}

func (u *UI) audContactFormView(r *http.Request, f *Form) View {
	return View{Title: "New contact", Nav: "contacts", Crumbs: []Crumb{{"Contacts", Prefix + "/contacts"}},
		Data: audContactFormData{Form: f, Tags: u.audTagNames(r.Context()), Statuses: audCreateStatuses()}}
}

// audCreateStatuses are the initial statuses a new contact can have:
// active, or a status that blocks sending (a migrated unsubscribe).
func audCreateStatuses() []Option {
	var out []Option
	for _, o := range audStatusOptions("") {
		switch core.ContactStatus(o.Value) {
		case core.StatusActive, core.StatusUnsubscribed, core.StatusBounced, core.StatusComplained:
			out = append(out, o)
		}
	}
	return out
}

// audContactNew is the empty create form (its tag suggestions come from
// list-tags).
func (u *UI) audContactNew(w http.ResponseWriter, r *http.Request) error {
	// ?email= prefills the address (the Inbox's "Add as contact").
	return u.Page(w, r, "audience/contact_form", u.audContactFormView(r,
		EmptyForm().Set("status", "active").Set("email", r.URL.Query().Get("email"))))
}

// audContactCreate creates a contact (create-contact).
func (u *UI) audContactCreate(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	in := contacts.CreateInput{
		Email: f.Get("email"), FirstName: f.Get("first_name"), LastName: f.Get("last_name"),
		Tags: ParseTags(f.Get("tags")), Status: core.ContactStatus(f.Get("status")),
	}
	attrs, err := audParseAttributes(f.Raw("attributes"))
	if err == nil {
		for k, v := range attrs {
			if v == nil {
				delete(attrs, k)
			}
		}
		in.Attributes = attrs
		if src, text := f.Get("consent.source"), f.Get("consent.text"); src != "" || text != "" {
			in.Consent = &contacts.Consent{Source: src, Text: text}
		}
		var c contacts.Contact
		if c, err = u.d.Contacts.Create(r.Context(), in); err == nil {
			u.Flash(r, FlashSuccess, "Contact "+c.Email+" created.")
			u.Redirect(w, r, fmt.Sprintf("%s/contacts/%d", Prefix, c.ID))
			return nil
		}
	}
	if st := f.Fail(err); st != 0 {
		return u.PageStatus(w, r, st, "audience/contact_form", u.audContactFormView(r, f))
	}
	return err
}

// ---------------------------------------------------------------- detail

type audContactData struct {
	C           contacts.Contact
	Name        string
	Attrs       []audKV
	Timeline    contacts.TimelinePage
	OlderURL    string // next timeline page
	NewestURL   string // first timeline page, when on a later one
	Enrollments []contacts.Enrollment
	TagForm     *Form
	StatusForm  *Form
	Statuses    []Option // statuses it can be set to
	Tags        []string
}

// audTimelinePage is the number of timeline items per page.
const audTimelinePage = 50

// audContactShow is the contact page: details, tags, status, timeline and
// enrollments (get-contact, get-contact-timeline,
// list-contact-enrollments).
func (u *UI) audContactShow(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	return u.audRenderContact(w, r, id, http.StatusOK, nil, nil)
}

func (u *UI) audRenderContact(w http.ResponseWriter, r *http.Request, id int64, status int, tagForm, statusForm *Form) error {
	ctx := r.Context()
	c, err := u.d.Contacts.Get(ctx, id)
	if err != nil {
		return err
	}
	cursor := r.URL.Query().Get("tcursor")
	tl, err := u.d.Contacts.Timeline(ctx, id, cursor, audTimelinePage)
	if err != nil {
		var ve *core.ValidationError
		if !errors.As(err, &ve) {
			return err
		}
		// A stale cursor: start from the newest item.
		cursor = ""
		if tl, err = u.d.Contacts.Timeline(ctx, id, "", audTimelinePage); err != nil {
			return err
		}
	}
	enr, err := u.d.Contacts.Enrollments(ctx, id, 0, 50)
	if err != nil {
		return err
	}
	if tagForm == nil {
		tagForm = EmptyForm()
	}
	if statusForm == nil {
		statusForm = EmptyForm()
	}
	d := audContactData{C: c, Attrs: audAttrList(c.Attributes), Timeline: tl, Enrollments: enr,
		TagForm: tagForm, StatusForm: statusForm, Tags: u.audTagNames(ctx)}
	d.Name = strings.TrimSpace(c.FirstName + " " + c.LastName)
	base := fmt.Sprintf("%s/contacts/%d", Prefix, id)
	if tl.NextCursor != "" {
		d.OlderURL = base + "?tcursor=" + url.QueryEscape(tl.NextCursor) + "#timeline"
	}
	if cursor != "" {
		d.NewestURL = base + "#timeline"
	}
	for _, o := range audStatusOptions("") {
		if o.Value != string(c.Status) && o.Value != string(core.StatusPending) {
			d.Statuses = append(d.Statuses, o)
		}
	}
	title := c.Email
	return u.PageStatus(w, r, status, "audience/contact", View{Title: title, Subtitle: d.Name, Nav: "contacts",
		Crumbs: []Crumb{{"Contacts", Prefix + "/contacts"}}, Data: d})
}

func audContactURL(id int64) string { return fmt.Sprintf("%s/contacts/%d", Prefix, id) }

// ---------------------------------------------------------------- edit

// audContactEdit is the edit form for names and attributes (get-contact).
func (u *UI) audContactEdit(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	c, err := u.d.Contacts.Get(r.Context(), id)
	if err != nil {
		return err
	}
	f := EmptyForm().Set("first_name", c.FirstName).Set("last_name", c.LastName).
		Set("attributes", audFormatAttributes(c.Attributes)).Set("version", strconv.FormatInt(c.Version, 10))
	return u.Page(w, r, "audience/contact_form", u.audContactEditView(c, f))
}

func (u *UI) audContactEditView(c contacts.Contact, f *Form) View {
	return View{Title: "Edit " + c.Email, Nav: "contacts",
		Crumbs: []Crumb{{"Contacts", Prefix + "/contacts"}, {c.Email, audContactURL(c.ID)}},
		Data:   audContactFormData{Form: f, Contact: &c}}
}

// audContactUpdate saves names and attributes (update-contact). Removed
// attribute lines delete those keys; the version guards against
// overwriting a newer change (412).
func (u *UI) audContactUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	cur, err := u.d.Contacts.Get(ctx, id)
	if err != nil {
		return err
	}
	f := NewForm(r)
	attrs, err := audParseAttributes(f.Raw("attributes"))
	if err == nil {
		for k := range cur.Attributes {
			if _, kept := attrs[k]; !kept {
				attrs[k] = nil
			}
		}
		first, last := f.Get("first_name"), f.Get("last_name")
		_, err = u.d.Contacts.Update(ctx, id, contacts.UpdateInput{FirstName: &first, LastName: &last, Attributes: attrs}, audVersion(f))
		if err == nil {
			u.Flash(r, FlashSuccess, "Contact saved.")
			u.Redirect(w, r, audContactURL(id))
			return nil
		}
	}
	if st := f.Fail(err); st != 0 {
		return u.PageStatus(w, r, st, "audience/contact_form", u.audContactEditView(cur, f))
	}
	return err
}

// ---------------------------------------------------------------- tags and status

// audContactAddTags adds tags (add-contact-tags).
func (u *UI) audContactAddTags(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	tags := ParseTags(f.Get("tags"))
	if len(tags) == 0 {
		err = fieldError("tags", "enter at least one tag")
	} else {
		_, err = u.d.Contacts.AddTags(r.Context(), id, tags)
	}
	if err == nil {
		u.Flash(r, FlashSuccess, "Added "+plural(len(tags), "tag", "tags")+": "+strings.Join(tags, ", ")+".")
		u.Redirect(w, r, audContactURL(id))
		return nil
	}
	if st := f.Fail(err); st != 0 {
		return u.audRenderContact(w, r, id, st, f, nil)
	}
	return err
}

// audContactRemoveTag removes one tag (remove-contact-tag).
func (u *UI) audContactRemoveTag(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	tag := f.Get("tag")
	if _, err := u.d.Contacts.RemoveTag(r.Context(), id, tag); err != nil {
		return err
	}
	u.Flash(r, FlashSuccess, "Removed tag "+tag+".")
	u.Redirect(w, r, audContactURL(id))
	return nil
}

// audContactStatus changes the status with a note (set-contact-status).
// The service applies the suppression rules; a refusal is shown on the
// form.
func (u *UI) audContactStatus(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	to := core.ContactStatus(f.Get("status"))
	var c contacts.Contact
	var changed bool
	if to == "" {
		err = fieldError("status", "choose a status")
	} else {
		c, changed, err = u.d.Contacts.SetStatus(r.Context(), id, to, f.Get("note"), audVersion(f))
	}
	if err == nil {
		switch {
		case to == core.StatusUnsubscribed && c.Status != to && changed:
			u.Flash(r, FlashSuccess, "The contact stays "+audStatusLabel(string(c.Status))+
				", and its request to unsubscribe is recorded: lifting the bounce or suppression later makes it unsubscribed.")
		case to == core.StatusUnsubscribed && c.Status != to:
			u.Flash(r, FlashInfo, "The contact stays "+audStatusLabel(string(c.Status))+"; a request to unsubscribe was already recorded.")
		case !changed:
			u.Flash(r, FlashInfo, "The contact already had that status.")
		case c.Status != to:
			u.Flash(r, FlashWarning, "The contact is now "+audStatusLabel(string(c.Status))+
				": it asked to unsubscribe while it couldn't be emailed.")
		default:
			u.Flash(r, FlashSuccess, "Status changed to "+audStatusLabel(string(c.Status))+".")
		}
		u.Redirect(w, r, audContactURL(id))
		return nil
	}
	if st := f.Fail(err); st != 0 {
		return u.audRenderContact(w, r, id, st, nil, f)
	}
	return err
}

// ---------------------------------------------------------------- GDPR

// audContactExport downloads everything held about the contact as JSON
// (export-contact), the same document as the API returns.
func (u *UI) audContactExport(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ex, err := u.d.Contacts.Export(r.Context(), id)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(ex, "", "  ")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="contact-%d.json"`, id))
	_, _ = w.Write(append(b, '\n'))
	return nil
}

// audContactErase erases the contact (erase-contact). The first step is a
// dry run whose counts the confirmation page shows.
func (u *UI) audContactErase(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	if !Confirmed(r) {
		c, err := u.d.Contacts.Get(ctx, id)
		if err != nil {
			return err
		}
		res, err := u.d.Contacts.Erase(ctx, id, audVersion(f), true)
		if err != nil {
			return err
		}
		del, anon := res.Deleted, res.Anonymised
		details := []string{
			"The contact " + c.Email + " with " + plural(del.Tags, "tag", "tags") + ", " +
				plural(del.ConsentEvents, "consent event", "consent events") + ", " +
				plural(del.StatusChanges, "status change", "status changes") + " and " +
				plural(del.Enrollments, "sequence enrollment", "sequence enrollments") + " is deleted.",
			plural(anon.Messages, "email", "emails") + ", " + plural(anon.Events, "delivery event", "delivery events") + ", " +
				plural(anon.Clicks, "click", "clicks") + " and " + plural(anon.FormSubmissions, "form submission", "form submissions") +
				" are kept for statistics without the address.",
		}
		if anon.QueuedSkipped > 0 {
			details = append(details, plural(anon.QueuedSkipped, "queued email is", "queued emails are")+" cancelled.")
		}
		details = append(details, "The address's SHA-256 hash stays on the suppression list, so an import or form can't add it again.")
		return u.RenderConfirm(w, r, Confirm{
			Title:   "Erase " + c.Email + "?",
			Message: "This deletes the contact's personal data for good (GDPR erasure). It can't be undone; download the export first if you need a copy.",
			Details: details, Action: audContactURL(id) + "/erase", Fields: f.Values,
			ConfirmLabel: "Erase contact", CancelURL: audContactURL(id), Danger: true, Nav: "contacts",
			Crumbs: []Crumb{{"Contacts", Prefix + "/contacts"}, {c.Email, audContactURL(id)}},
		})
	}
	if _, err := u.d.Contacts.Erase(ctx, id, audVersion(f), false); err != nil {
		return err
	}
	u.Flash(r, FlashSuccess, "The contact was erased. Its address stays suppressed.")
	u.Redirect(w, r, Prefix+"/contacts")
	return nil
}

// ---------------------------------------------------------------- bulk tags

// audBulkTag adds or removes a tag on the selected contacts or on every
// member of a segment (bulk-tag). The first step is a dry run whose
// match count the confirmation page shows; the second submits the job.
func (u *UI) audBulkTag(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	f := NewForm(r)
	back := audBack(f, Prefix+"/contacts")
	in := contacts.BulkTagInput{Tag: f.Get("tag"), Action: f.Get("action")}
	for _, s := range f.All("contact_ids") {
		id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err == nil && id > 0 {
			in.ContactIDs = append(in.ContactIDs, id)
		}
	}
	if s := f.Get("segment_id"); s != "" {
		in.SegmentID, _ = strconv.ParseInt(s, 10, 64)
	}
	fail := func(err error) error {
		if f.Fail(err) == 0 {
			return err
		}
		u.Flash(r, FlashError, audFormMessage(f))
		u.Redirect(w, r, back)
		return nil
	}
	if in.Tag == "" {
		return fail(fieldError("tag", "enter the tag to add or remove"))
	}
	if len(in.ContactIDs) == 0 && in.SegmentID == 0 {
		return fail(fieldError("contact_ids", "select at least one contact"))
	}
	if !Confirmed(r) {
		res, err := u.d.Contacts.BulkTag(ctx, in, true)
		if err != nil {
			return fail(err)
		}
		verb, prep := "Add", "to"
		if in.Action == "remove" {
			verb, prep = "Remove", "from"
		}
		who := plural(res.Matched, "contact", "contacts")
		if in.SegmentID > 0 {
			who += " in the segment (counted now; the members are taken when the job runs)"
		}
		msg := "Adding a tag can start sequences whose trigger is that tag, or end enrollments for which it is an exit tag."
		if in.Action == "remove" {
			msg = "Removing a tag doesn't stop sequences the contacts are already in."
		}
		return u.RenderConfirm(w, r, Confirm{
			Title: fmt.Sprintf("%s tag %q %s %s?", verb, in.Tag, prep, plural(res.Matched, "contact", "contacts")), Message: msg,
			Details: []string{who + " matched."}, Action: Prefix + "/contacts/bulk-tag", Fields: f.Values,
			ConfirmLabel: verb + " tag", CancelURL: back, Danger: in.Action == "remove", Nav: "contacts",
		})
	}
	res, err := u.d.Contacts.BulkTag(ctx, in, false)
	if err != nil {
		return fail(err)
	}
	msg := fmt.Sprintf("Tagging %s runs in the background", plural(res.Matched, "contact", "contacts"))
	if res.Job != nil {
		msg += fmt.Sprintf(" (job %d)", res.Job.ID)
	}
	u.Flash(r, FlashSuccess, msg+". Reload in a moment to see the result.")
	u.Redirect(w, r, back)
	return nil
}
