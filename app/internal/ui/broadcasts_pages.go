package ui

// Broadcasts pages (PRD §6.5 B-1..B-9, A-2): the list, the new/edit form
// with the email editor and the audience picker, and the detail page with
// the send, schedule, test and preview controls. The actions themselves
// are in broadcasts_actions.go, the stats page in broadcasts_stats.go and
// the base layout editor in broadcasts_layout.go.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/broadcasts"
	"mailroom/internal/contacts"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/segments"
	"mailroom/internal/sending"
)

// broadcastsPath is the list page; broadcastURL(id) the detail page.
const broadcastsPath = Prefix + "/broadcasts"

func broadcastURL(id int64) string { return fmt.Sprintf("%s/%d", broadcastsPath, id) }

func init() {
	registerGroup("broadcasts", func(u *UI) {
		get, post := http.MethodGet, http.MethodPost
		p := broadcastsPath
		u.handle(Action{Method: get, Path: p, OperationID: "list-broadcasts", Handler: u.broadcastsList})
		u.handle(Action{Method: get, Path: p + "/new", OperationID: "list-segments", Uses: []string{"get-settings", "list-broadcasts", "get-broadcast", "list-emails", "get-email"},
			Handler: u.broadcastNew})
		u.handle(Action{Method: post, Path: p, OperationID: "create-broadcast", Uses: []string{"list-segments", "get-settings"},
			Handler: u.broadcastCreate})
		u.handle(Action{Method: get, Path: p + "/audience-count", OperationID: "get-segment-count", Uses: []string{"preview-segment"},
			Handler: u.broadcastAudienceCount})
		u.handle(Action{Method: get, Path: p + "/{id}", OperationID: "get-broadcast",
			Uses: []string{"get-broadcast-stats", "get-settings", "get-sending-state"}, Handler: u.broadcastShow})
		u.handle(Action{Method: get, Path: p + "/{id}/edit", OperationID: "get-broadcast", Uses: []string{"list-segments", "get-settings"},
			Handler: u.broadcastEdit})
		u.handle(Action{Method: post, Path: p + "/{id}/edit", OperationID: "update-broadcast", Uses: []string{"list-segments", "get-settings"},
			Handler: u.broadcastUpdate})
		u.handle(Action{Method: post, Path: p + "/{id}/preview", OperationID: "preview-email", Uses: []string{"get-broadcast", "get-contact", "list-contacts"},
			Handler: u.broadcastPreview})
		u.registerBroadcastActions()
		u.registerBroadcastStats()
		u.registerLayout()
	})
}

// ---------------------------------------------------------------- list

// broadcastStatuses are the list filter tabs, in lifecycle order.
var broadcastStatuses = []Option{
	{"", "All"},
	{broadcasts.StatusDraft, "Drafts"},
	{broadcasts.StatusScheduled, "Scheduled"},
	{broadcasts.StatusSending, "Sending"},
	{broadcasts.StatusPaused, "Paused"},
	{broadcasts.StatusSent, "Sent"},
	{broadcasts.StatusCancelled, "Cancelled"},
}

type broadcastListData struct {
	Items    []broadcasts.Broadcast
	Pager    Pager
	Status   string
	Statuses []Option
}

// broadcastsList is the Broadcasts page: newest first, filtered by status.
func (u *UI) broadcastsList(w http.ResponseWriter, r *http.Request) error {
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	rows, err := u.d.Broadcasts.List(r.Context(), broadcasts.ListFilter{Status: status, BeforeID: p.After, Limit: p.Fetch()})
	if err != nil {
		return err
	}
	items, pager := Paginate(r, p, rows, func(b broadcasts.Broadcast) int64 { return b.ID })
	return u.Page(w, r, "broadcasts/index", View{
		Title: "Broadcasts", Subtitle: "One email to everyone active or to a segment, now or at a set time.", Nav: "broadcasts",
		Data: broadcastListData{Items: items, Pager: pager, Status: status, Statuses: broadcastStatuses},
	})
}

// ---------------------------------------------------------------- form

// broadcastFormData is the data of the new and edit pages.
type broadcastFormData struct {
	Form      *Form
	Broadcast *broadcasts.Broadcast // nil on the new page
	Action    string
	Audiences []Option
	// The settings' sender, shown as the defaults of the overrides.
	DefaultFromName string
	DefaultReplyTo  string
	FromEmail       string
	Domain          string
	// "Start from" choices: only on a fresh new page.
	FromBroadcasts, FromEmails []starter
}

// audienceValue is the picker value of an audience: "all" or "segment:<id>".
func audienceValue(a broadcasts.BroadcastAudience) string {
	if a.Type == broadcasts.AudienceSegment {
		return "segment:" + strconv.FormatInt(a.SegmentID, 10)
	}
	return broadcasts.AudienceAll
}

// parseAudience reads the picker value back.
func parseAudience(v string) (broadcasts.BroadcastAudience, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == broadcasts.AudienceAll {
		return broadcasts.BroadcastAudience{Type: broadcasts.AudienceAll}, nil
	}
	if s, ok := strings.CutPrefix(v, "segment:"); ok {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil && id > 0 {
			return broadcasts.BroadcastAudience{Type: broadcasts.AudienceSegment, SegmentID: id}, nil
		}
	}
	return broadcasts.BroadcastAudience{}, fieldError("audience", "choose everyone active or one of the segments")
}

// broadcastForm prefills the edit form from b.
func broadcastForm(b broadcasts.Broadcast) *Form {
	f := EmptyForm().
		Set("name", b.Name).
		Set("audience", audienceValue(b.Audience)).
		Set("subject", b.Email.Subject).
		Set("preheader", b.Email.Preheader).
		Set("format", b.Email.Format).
		Set("body", b.Email.Body).
		Set("version", strconv.FormatInt(b.Version, 10))
	if b.FromName != nil {
		f.Set("from_name", *b.FromName)
	}
	switch {
	case b.ReplyTo == nil:
	case *b.ReplyTo == "":
		f.Set("no_reply_to", "1")
	default:
		f.Set("reply_to", *b.ReplyTo)
	}
	return f
}

// broadcastField maps a service field name to the form field it belongs
// to: the email fields carry an "email." prefix, the audience is one
// picker, test addresses are one text box.
func broadcastField(k string) string {
	switch {
	case strings.HasPrefix(k, "email."):
		return strings.TrimPrefix(k, "email.")
	case strings.HasPrefix(k, "audience"):
		return "audience"
	case strings.HasPrefix(k, "to["):
		return "to"
	case k == "confirm":
		return "confirm_resume"
	}
	return k
}

// failBroadcastForm is Form.Fail with the service's field names mapped to
// the form's (broadcastField).
func failBroadcastForm(f *Form, err error) int {
	st := f.Fail(err)
	if st == 0 {
		return 0
	}
	renamed := false
	for k, msg := range f.Errors {
		nk := broadcastField(k)
		if nk == k {
			continue
		}
		delete(f.Errors, k)
		if strings.HasPrefix(k, "to[") {
			if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(k, "to["), "]")); err == nil {
				msg = fmt.Sprintf("address %d %s", n+1, msg)
			}
		}
		if _, dup := f.Errors[nk]; !dup {
			f.Errors[nk] = msg
		}
		renamed = true
	}
	if renamed {
		f.Message = "Please correct the highlighted field."
	}
	return st
}

// formVersion is the ETag version an edit or action form carries (0: no
// check).
func formVersion(f *Form) int64 {
	v, _ := strconv.ParseInt(f.Get("version"), 10, 64)
	return max(v, 0)
}

// broadcastFormPage renders the new or edit page.
func (u *UI) broadcastFormPage(w http.ResponseWriter, r *http.Request, status int, f *Form, b *broadcasts.Broadcast) error {
	ctx := r.Context()
	st, err := u.d.Settings.Get(ctx)
	if err != nil {
		return err
	}
	opts := []Option{{broadcasts.AudienceAll, "Everyone active"}}
	segs, err := u.d.Segments.List(ctx, 0, 500)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, s := range segs {
		v := "segment:" + strconv.FormatInt(s.ID, 10)
		seen[v] = true
		opts = append(opts, Option{v, "Segment: " + s.Name})
	}
	if b != nil && b.Audience.Type == broadcasts.AudienceSegment && !seen[audienceValue(b.Audience)] {
		opts = append(opts, Option{audienceValue(b.Audience), "Segment: " + defaultString(b.Audience.SegmentName, "#"+strconv.FormatInt(b.Audience.SegmentID, 10))})
	}
	d := broadcastFormData{
		Form: f, Broadcast: b, Action: broadcastsPath, Audiences: opts,
		DefaultFromName: st.Sender.FromName, DefaultReplyTo: st.Sender.ReplyTo, FromEmail: st.Sender.FromEmail, Domain: st.Domain,
	}
	if b == nil && status == http.StatusOK && r.Method == http.MethodGet && r.URL.Query().Get("from") == "" {
		if d.FromBroadcasts, d.FromEmails, err = u.broadcastStarters(ctx); err != nil {
			return err
		}
	}
	v := View{Title: "New broadcast", Nav: "broadcasts", Crumbs: []Crumb{{"Broadcasts", broadcastsPath}}, Data: d}
	if b != nil {
		d.Action = broadcastURL(b.ID) + "/edit"
		v.Data = d
		v.Title = "Edit " + b.Name
		v.Crumbs = append(v.Crumbs, Crumb{b.Name, broadcastURL(b.ID)})
		if b.Status == broadcasts.StatusScheduled {
			v.Subtitle = "Scheduled for " + u.fmtTime(b.ScheduledAt) + ". It stays scheduled while you edit it."
		}
	}
	return u.PageStatus(w, r, status, "broadcasts/form", v)
}

// broadcastNew is the empty form.
func (u *UI) broadcastNew(w http.ResponseWriter, r *http.Request) error {
	f := EmptyForm().Set("audience", broadcasts.AudienceAll).Set("format", content.FormatHTML)
	if a := r.URL.Query().Get("audience"); a != "" {
		f.Set("audience", a)
	}
	if from := r.URL.Query().Get("from"); from != "" {
		if err := u.prefillFrom(r.Context(), f, from); err != nil {
			return err
		}
	}
	return u.broadcastFormPage(w, r, http.StatusOK, f, nil)
}

// senderInput reads the from and reply-to overrides of the form.
// replyTo: nil = the settings default, "" = none.
func senderInput(f *Form) (fromName string, replyTo *string) {
	fromName = f.Get("from_name")
	switch {
	case f.Checked("no_reply_to"):
		none := ""
		replyTo = &none
	case f.Get("reply_to") != "":
		v := f.Get("reply_to")
		replyTo = &v
	}
	return fromName, replyTo
}

// broadcastCreate creates a draft (create-broadcast) and opens it.
func (u *UI) broadcastCreate(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	aud, err := parseAudience(f.Get("audience"))
	if err == nil {
		fromName, replyTo := senderInput(f)
		var b broadcasts.Broadcast
		b, err = u.d.Broadcasts.Create(r.Context(), broadcasts.CreateInput{
			Name: f.Get("name"), Audience: aud, FromName: fromName, ReplyTo: replyTo,
			Email: broadcasts.EmailContent{Subject: f.Get("subject"), Preheader: f.Get("preheader"), Format: f.Get("format"), Body: f.Raw("body")},
		}, false)
		if err == nil {
			u.Flash(r, FlashSuccess, "Draft saved. Send yourself a test, then schedule it or send it now.")
			u.Redirect(w, r, broadcastURL(b.ID))
			return nil
		}
	}
	if st := failBroadcastForm(f, err); st != 0 {
		return u.broadcastFormPage(w, r, st, f, nil)
	}
	return err
}

// broadcastEdit is the edit form of a draft or scheduled broadcast.
func (u *UI) broadcastEdit(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	b, err := u.d.Broadcasts.Get(r.Context(), id)
	if err != nil {
		return err
	}
	if !broadcastEditable(b.Status) {
		u.Flash(r, FlashInfo, "This broadcast is "+b.Status+"; only drafts and scheduled broadcasts can be changed. Duplicate it to send a changed copy.")
		u.Redirect(w, r, broadcastURL(id))
		return nil
	}
	return u.broadcastFormPage(w, r, http.StatusOK, broadcastForm(b), &b)
}

// broadcastUpdate saves the edit form (update-broadcast), with the
// version the form was loaded at as If-Match.
func (u *UI) broadcastUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	aud, err := parseAudience(f.Get("audience"))
	if err == nil {
		fromName, replyTo := senderInput(f)
		name, subject, preheader, format, body := f.Get("name"), f.Get("subject"), f.Get("preheader"), f.Get("format"), f.Raw("body")
		in := broadcasts.UpdateInput{
			Name: &name, Audience: &aud, FromName: &fromName, ReplyTo: replyTo, DefaultReplyTo: replyTo == nil,
			Email: &broadcasts.EmailContentPatch{Subject: &subject, Preheader: &preheader, Body: &body},
		}
		if format != "" {
			in.Email.Format = &format
		}
		if _, err = u.d.Broadcasts.Update(ctx, id, in, formVersion(f), false); err == nil {
			u.Flash(r, FlashSuccess, "Changes saved.")
			u.Redirect(w, r, broadcastURL(id))
			return nil
		}
	}
	st := failBroadcastForm(f, err)
	if st == 0 {
		return err
	}
	b, gerr := u.d.Broadcasts.Get(ctx, id)
	if gerr != nil {
		return gerr
	}
	if st == http.StatusPreconditionFailed {
		f.Set("version", strconv.FormatInt(b.Version, 10))
	}
	return u.broadcastFormPage(w, r, st, f, &b)
}

// audienceCountData is the live recipient count under the audience picker.
type audienceCountData struct {
	Segment     bool
	Active      int64
	MissingTags []string
	Error       string
}

// broadcastAudienceCount is the picker's live count: the active contacts
// of the segment (get-segment-count) or of everyone (preview-segment with
// an empty filter). The send confirmation shows the exact number, which
// also leaves out suppressed addresses.
func (u *UI) broadcastAudienceCount(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var d audienceCountData
	aud, err := parseAudience(r.URL.Query().Get("audience"))
	if err == nil {
		var c segments.SegmentCounts
		if aud.Type == broadcasts.AudienceSegment {
			d.Segment = true
			c, err = u.d.Segments.CountSegment(ctx, aud.SegmentID)
		} else {
			c, err = u.d.Segments.Count(ctx, segments.SegmentFilter{})
		}
		d.Active, d.MissingTags = c.Active, c.MissingTags
	}
	if err != nil {
		f := EmptyForm()
		switch {
		case errors.Is(err, core.ErrNotFound):
			d.Error = "This segment no longer exists."
		case f.Fail(err) != 0:
			d.Error = "This audience can't be counted: " + f.Message
			for _, m := range f.Errors {
				d.Error = "This audience can't be counted: " + m
			}
		default:
			return err
		}
	}
	return u.Partial(w, r, "broadcasts/form", "audience-count", View{Data: d})
}

// ---------------------------------------------------------------- detail

func broadcastEditable(status string) bool {
	return status == broadcasts.StatusDraft || status == broadcasts.StatusScheduled
}

// broadcastShowData is the detail page's data.
type broadcastShowData struct {
	B     broadcasts.Broadcast
	Stats broadcasts.BroadcastStats

	Editable, CanSchedule, CanUnschedule, CanSend, CanPause, CanResume, CanCancel, CanDelete bool
	BreakerPaused, SnapshotFailed                                                            bool
	Started                                                                                  bool // has left draft/scheduled

	Sending         sending.State
	DefaultFromName string
	DefaultReplyTo  string
	FromEmail       string
	// ReplyTo is the Reply-To address the broadcast uses (its own or the
	// default), "" for none.
	ReplyTo string

	TestForm     *Form
	ScheduleForm *Form
	ResumeForm   *Form

	Checklist   *broadcastChecklist // draft and scheduled only
	OwnFromName string              // the broadcast's own From name, "" for the default
}

// broadcastShow is the detail page.
func (u *UI) broadcastShow(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	return u.renderBroadcast(w, r, http.StatusOK, id, nil)
}

// renderBroadcast renders the detail page of id with status; tune may put
// a form with errors in place (a failed test send, schedule or resume).
func (u *UI) renderBroadcast(w http.ResponseWriter, r *http.Request, status int, id int64, tune func(*broadcastShowData)) error {
	ctx := r.Context()
	b, err := u.d.Broadcasts.Get(ctx, id)
	if err != nil {
		return err
	}
	stats, err := u.d.Broadcasts.Stats(ctx, id)
	if err != nil {
		return err
	}
	st, err := u.d.Settings.Get(ctx)
	if err != nil {
		return err
	}
	state, err := u.d.Sending.GetState(ctx, nil)
	if err != nil {
		return err
	}
	s := b.Status
	d := broadcastShowData{
		B: b, Stats: stats, Sending: state,
		Editable:      broadcastEditable(s),
		CanSchedule:   s == broadcasts.StatusDraft,
		CanUnschedule: s == broadcasts.StatusScheduled,
		CanSend:       broadcastEditable(s),
		CanPause:      s == broadcasts.StatusSending,
		CanResume:     s == broadcasts.StatusPaused,
		CanCancel:     s == broadcasts.StatusScheduled || s == broadcasts.StatusPaused,
		CanDelete:     s == broadcasts.StatusDraft || s == broadcasts.StatusCancelled,
		BreakerPaused: s == broadcasts.StatusPaused && b.PausedReason == broadcasts.PauseCircuitBreaker,
		SnapshotFailed: s == broadcasts.StatusPaused &&
			b.PausedReason == broadcasts.PauseSnapshotFailed,
		Started:         !broadcastEditable(s),
		DefaultFromName: st.Sender.FromName, DefaultReplyTo: st.Sender.ReplyTo, FromEmail: st.Sender.FromEmail,
		TestForm:     EmptyForm().Set("to", st.OwnerEmail),
		ScheduleForm: EmptyForm(),
		ResumeForm:   EmptyForm(),
	}
	if b.FromName != nil {
		d.OwnFromName = *b.FromName
	}
	d.ReplyTo = st.Sender.ReplyTo
	if b.ReplyTo != nil {
		d.ReplyTo = *b.ReplyTo
	}
	if broadcastEditable(s) {
		if d.Checklist, err = u.checklist(ctx, b); err != nil {
			return err
		}
	}
	at := b.ScheduledAt
	if at == nil {
		// Tomorrow at 09:00 in the settings time zone: a deliberate choice,
		// never a time that is about to pass.
		now := u.clock.Now().In(u.location())
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 9, 0, 0, 0, u.location())
		at = &next
	}
	d.ScheduleForm.Set("at", u.fmtInputTime(at))
	if tune != nil {
		tune(&d)
	}
	return u.PageStatus(w, r, status, "broadcasts/show", View{
		Title: b.Name, Nav: "broadcasts", Crumbs: []Crumb{{"Broadcasts", broadcastsPath}}, Data: d,
	})
}

// lookupContact resolves the contact whose data a preview or test uses,
// given by its email address (list-contacts with email=, the exact
// lookup) or its ID (get-contact). "" is none: merge fields show their
// fallbacks. Problems are validation errors on field.
func (u *UI) lookupContact(ctx context.Context, field, raw string) (*core.Contact, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var c contacts.Contact
	if strings.Contains(raw, "@") {
		cs, err := u.d.Contacts.List(ctx, contacts.ListFilter{Email: raw, Limit: 1})
		if errors.Is(err, core.ErrValidation) {
			return nil, fieldError(field, "is not a valid email address")
		}
		if err != nil {
			return nil, err
		}
		if len(cs) == 0 {
			return nil, fieldError(field, fmt.Sprintf("no contact has the address %s", raw))
		}
		c = cs[0]
	} else {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return nil, fieldError(field, "must be a contact's email address (or ID)")
		}
		c, err = u.d.Contacts.Get(ctx, id)
		if errors.Is(err, core.ErrNotFound) {
			return nil, fieldError(field, fmt.Sprintf("contact %d does not exist", id))
		}
		if err != nil {
			return nil, err
		}
	}
	return &core.Contact{
		ID: c.ID, Email: c.Email, FirstName: c.FirstName, LastName: c.LastName,
		Attributes: c.Attributes, Tags: c.Tags, Status: c.Status,
		StatusChangedAt: c.StatusChangedAt, UnsubscribeRequestedAt: c.UnsubscribeRequestedAt,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Version: c.Version,
	}, nil
}

// broadcastPreview renders the saved email of a broadcast as it would be
// sent (preview-email), with a real contact's data when an ID is given
// (B-7; by email address or ID), into the detail page's preview panel.
func (u *UI) broadcastPreview(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	b, err := u.d.Broadcasts.Get(ctx, id)
	if err != nil {
		return err
	}
	var d previewData
	c, err := u.lookupContact(ctx, "contact", r.PostFormValue("preview_contact"))
	if err == nil {
		var out core.Rendered
		out, err = u.d.Content.RenderDraft(ctx, content.Draft{
			Subject: b.Email.Subject, Preheader: b.Email.Preheader, Format: b.Email.Format, Body: b.Email.Body,
		}, c, nil)
		d = previewData{Subject: out.Subject, HTML: out.HTML, Text: out.Text}
	}
	if err != nil {
		f := EmptyForm()
		if f.Fail(err) == 0 {
			return err
		}
		d = previewData{Error: f.Message}
		for field, msg := range f.Errors {
			d.Error = field + ": " + msg
		}
	}
	return u.Partial(w, r, "broadcasts/show", "email-preview", View{Data: d})
}

// broadcastChecklist is the pre-send checklist of a draft or scheduled
// broadcast (like Mailchimp's): recipients, sender, subject as a sample
// contact sees it, personalisation gaps and whether a test went out.
type broadcastChecklist struct {
	Recipients    int
	Subject       string // rendered for SampleName's contact (or with fallbacks)
	SampleName    string
	Gaps          []mergeGap
	LastTest      *time.Time
	TestStatus    string // the latest test message's status
	TestDelivered bool   // it actually went out (sent or delivered)
	Todo          int
	Ready         bool
}

// mergeGap is a merge field used without a fallback that some contacts
// have no value for.
type mergeGap struct {
	Field, Label   string
	Missing, Total int64
}

// checklist builds the checklist of b.
func (u *UI) checklist(ctx context.Context, b broadcasts.Broadcast) (*broadcastChecklist, error) {
	c := &broadcastChecklist{Subject: b.Email.Subject}
	var counts segments.SegmentCounts
	var err error
	f := contacts.ListFilter{Status: core.StatusActive, Limit: previewContactLimit}
	if b.Audience.Type == broadcasts.AudienceSegment {
		counts, err = u.d.Segments.CountSegment(ctx, b.Audience.SegmentID)
		f.SegmentID = b.Audience.SegmentID
	} else {
		counts, err = u.d.Segments.Count(ctx, segments.SegmentFilter{})
	}
	if err != nil && !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrValidation) {
		return nil, err
	}
	c.Recipients = int(counts.Active)

	// The subject as a real recipient sees it, preferably one with a name.
	if cs, err := u.d.Contacts.List(ctx, f); err == nil && len(cs) > 0 {
		pick := cs[0]
		for _, x := range cs {
			if x.FirstName != "" {
				pick = x
				break
			}
		}
		sample := &core.Contact{ID: pick.ID, Email: pick.Email, FirstName: pick.FirstName, LastName: pick.LastName, Attributes: pick.Attributes, Status: pick.Status}
		d := content.Draft{Subject: b.Email.Subject, Preheader: b.Email.Preheader, Format: b.Email.Format, Body: b.Email.Body}
		if out, err := u.d.Content.RenderDraft(ctx, d, sample, nil); err == nil {
			c.Subject = out.Subject
			c.SampleName = strings.TrimSpace(pick.FirstName + " " + pick.LastName)
			if c.SampleName == "" {
				c.SampleName = pick.Email
			}
		}
	}

	if gaps := content.UnguardedFields(content.Draft{Subject: b.Email.Subject, Preheader: b.Email.Preheader, Body: b.Email.Body}); len(gaps) > 0 {
		fields, err := u.d.Content.MergeFields(ctx)
		if err != nil {
			return nil, err
		}
		var total int64
		have := map[string]int64{}
		for _, mf := range fields {
			have[mf.Name] = mf.Contacts
			if mf.Name == "email" {
				total = mf.Contacts
			}
		}
		for _, g := range gaps {
			if missing := total - have[g]; missing > 0 {
				label := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(g, "attr."), "_", " "))
				c.Gaps = append(c.Gaps, mergeGap{Field: g, Label: label, Missing: missing, Total: total})
			}
		}
	}
	if c.LastTest, c.TestStatus, err = u.d.Content.LastTest(ctx, b.Email.ID, b.Email.Subject); err != nil {
		return nil, err
	}
	c.TestDelivered = c.TestStatus == "sent" || c.TestStatus == "delivered"
	if c.Recipients == 0 {
		c.Todo++
	}
	if !c.TestDelivered {
		c.Todo++
	}
	c.Todo += len(c.Gaps)
	c.Ready = c.Todo == 0
	return c, nil
}

// confirmFacts are the checklist facts the send and schedule confirmations
// repeat: the subject as a sample recipient sees it, and every open
// warning (personalisation gaps, no delivered test).
func (u *UI) confirmFacts(ctx context.Context, b broadcasts.Broadcast) (subject string, warnings []string, err error) {
	c, err := u.checklist(ctx, b)
	if err != nil {
		return "", nil, err
	}
	subject = "Subject: " + c.Subject
	if c.SampleName != "" {
		subject += " (as " + c.SampleName + " sees it)"
	}
	for _, g := range c.Gaps {
		warnings = append(warnings, fmt.Sprintf("Warning: %d of %d contacts have no %s, and the email has no fallback for it. They would see a gap, e.g. \"Hi ,\".", g.Missing, g.Total, g.Label))
	}
	if !c.TestDelivered {
		warnings = append(warnings, "Warning: No test of this email has been delivered yet.")
	}
	if !u.sesReady() {
		warnings = append(warnings, "Warning: Email sending isn't connected yet (Amazon SES). Nothing goes out until it is set up; then it starts at once.")
	}
	return subject, warnings, nil
}
