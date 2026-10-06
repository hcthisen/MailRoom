package ui

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"mailroom/internal/sequences"
)

// Enrollment pages (S-2, S-3, S-8): the list with a manual exit, and bulk
// enrolment by pasted addresses with a dry run.

func (u *UI) registerEnrollments() {
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/{id}/enrollments", OperationID: "list-sequence-enrollments",
		Uses: []string{"get-sequence"}, Handler: u.enrList})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/enrollments/{enrollment_id}/exit",
		OperationID: "unenroll-sequence-contact", Uses: []string{"get-sequence"}, Handler: u.enrExit})
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/{id}/enroll", OperationID: "get-sequence", Handler: u.enrNew})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/enroll", OperationID: "enroll-sequence-contacts",
		Uses: []string{"get-sequence"}, MaxBodyBytes: enrMaxBody, Handler: u.enrCreate})
}

// enrMaxBody fits sequences.MaxEnrollBatch addresses in a form (the
// confirmation re-posts them), percent-encoded.
const enrMaxBody = sequences.MaxEnrollBatch*3*(254+3) + 64<<10

var enrStatusOptions = []Option{
	{"", "All"}, {sequences.EnrollmentActive, "Active"}, {sequences.EnrollmentCompleted, "Completed"}, {sequences.EnrollmentExited, "Exited"},
}

type enrListData struct {
	Tabs    template.HTML
	Seq     sequences.Sequence
	Items   []sequences.SequenceEnrollment
	Pager   Pager
	Filter  *Form
	Options []Option
	Status  string
}

// enrList lists a sequence's enrollments, newest first, with a status
// filter.
func (u *UI) enrList(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	seq, err := u.seqGet(ctx, r)
	if err != nil {
		return err
	}
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", sequences.EnrollmentActive, sequences.EnrollmentCompleted, sequences.EnrollmentExited:
	default:
		status = ""
	}
	list, err := u.d.Sequences.ListEnrollments(ctx, seq.ID, sequences.EnrollmentFilter{Status: status, BeforeID: p.After, Limit: p.Fetch()})
	if err != nil {
		return err
	}
	items, pager := Paginate(r, p, list, func(e sequences.SequenceEnrollment) int64 { return e.ID })
	d := enrListData{Seq: seq, Items: items, Pager: pager, Filter: EmptyForm().Set("status", status),
		Options: enrStatusOptions, Status: status}
	if d.Tabs, err = seqTabs(seq, "enrollments"); err != nil {
		return err
	}
	return u.Page(w, r, "automation/seq_enrollments", View{Title: "Enrollments", Subtitle: seq.Name, Nav: "sequences",
		Crumbs: seqCrumbs(seq), Data: d})
}

// enrExit ends an active enrollment after a confirmation
// (unenroll-sequence-contact).
func (u *UI) enrExit(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	seq, err := u.seqGet(ctx, r)
	if err != nil {
		return err
	}
	eid, err := PathID(r, "enrollment_id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	back := seqURL(seq.ID, "/enrollments")
	if !Confirmed(r) {
		who := "this contact"
		if e := f.Get("email"); e != "" {
			who = e
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:   "End this enrollment?",
			Message: fmt.Sprintf("%s gets no further emails from %q, and steps already queued for it are not sent.", who, seq.Name),
			Details: []string{"Enrolling the contact again later follows the sequence's re-entry policy (" +
				strings.ReplaceAll(seq.Reentry, "_", " ") + ")."},
			Action: seqURL(seq.ID, "/enrollments/"+strconv.FormatInt(eid, 10)+"/exit"), Fields: f.Values,
			ConfirmLabel: "End enrollment", CancelURL: back, Danger: true, Nav: "sequences", Crumbs: seqCrumbs(seq),
		})
	}
	e, err := u.d.Sequences.Unenroll(ctx, seq.ID, eid)
	if err != nil {
		return err
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("The enrollment of %s ended.", e.Email))
	u.Redirect(w, r, back)
	return nil
}

type enrFormData struct {
	Tabs    template.HTML
	Seq     sequences.Sequence
	Form    *Form
	Steps   []Option
	Sources []Option
	Result  *sequences.EnrollResult // a dry run that would enrol nobody
	Skipped []string
}

var enrSourceOptions = []Option{
	{"admin", "Added by hand (source admin)"},
	{"migration", "Moved from another tool mid-drip (source migration)"},
}

func (u *UI) enrView(w http.ResponseWriter, r *http.Request, status int, d enrFormData) error {
	d.Steps = nil
	for _, st := range d.Seq.Steps {
		d.Steps = append(d.Steps, Option{strconv.Itoa(st.Position), fmt.Sprintf("Step %d: %s", st.Position, truncate(50, st.Subject))})
	}
	d.Sources = enrSourceOptions
	tabs, err := seqTabs(d.Seq, "enroll")
	if err != nil {
		return err
	}
	d.Tabs = tabs
	return u.PageStatus(w, r, status, "automation/seq_enroll", View{Title: "Enrol contacts", Subtitle: d.Seq.Name,
		Nav: "sequences", Crumbs: seqCrumbs(d.Seq), Data: d})
}

// enrNew is the bulk enrolment form.
func (u *UI) enrNew(w http.ResponseWriter, r *http.Request) error {
	seq, err := u.seqGet(r.Context(), r)
	if err != nil {
		return err
	}
	return u.enrView(w, r, http.StatusOK, enrFormData{Seq: seq, Form: EmptyForm().Set("at_step", "1").Set("source", "admin")})
}

// enrSkipped describes the skipped counts of a dry run.
func enrSkipped(m map[string]int) []string {
	labels := map[string]string{
		sequences.SkipNotFound:      "no contact with this address",
		sequences.SkipNotActive:     "not active (pending, unsubscribed, bounced, complained or suppressed)",
		sequences.SkipAlreadyActive: "already in this sequence",
		sequences.SkipReentry:       "the re-entry policy doesn't allow another round",
		sequences.SkipExitTag:       "has one of the exit tags",
		sequences.SkipNoSteps:       "the sequence has no steps yet",
		sequences.SkipUnsubscribed:  "stopped this sequence from its unsubscribe page",
	}
	keys := make([]string, 0, len(m))
	for k, n := range m {
		if n > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		l := labels[k]
		if l == "" {
			l = strings.ReplaceAll(k, "_", " ")
		}
		out = append(out, fmt.Sprintf("%s skipped: %s", plural(m[k], "contact", "contacts"), l))
	}
	return out
}

// enrFields puts errors about the request on the form's inputs.
func enrFields(emails []string) func(string, string) (string, string) {
	fold := autoFoldIndex("emails", emails)
	return func(field, msg string) (string, string) {
		if field == "contact_ids" {
			if strings.HasPrefix(msg, "give either") {
				return "emails", "enter at least one email address"
			}
			return "emails", msg
		}
		return fold(field, msg)
	}
}

// enrCreate enrols the pasted addresses (enroll-sequence-contacts): a
// dry run first, shown on a confirmation page with what would happen,
// then the enrolment itself.
func (u *UI) enrCreate(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	seq, err := u.seqGet(ctx, r)
	if err != nil {
		return err
	}
	f := NewForm(r)
	d := enrFormData{Seq: seq, Form: f}
	emails := autoSplitList(f.Get("emails"))
	req := sequences.EnrollRequest{Emails: emails, Source: f.Get("source")}
	if req.Source != "migration" {
		req.Source = "admin"
	}
	if s := f.Get("at_step"); s != "" {
		if req.AtStep, err = strconv.Atoi(s); err != nil {
			f.AddError("at_step", "choose a step")
		}
	}
	start, err := u.ParseInputTime("start_at", f.Get("start_at"))
	if err != nil {
		f.Fail(err)
	} else if !start.IsZero() {
		req.StartAt = &start
	}
	if f.HasErrors() {
		return u.enrView(w, r, http.StatusUnprocessableEntity, d)
	}

	confirmed := Confirmed(r)
	res, err := u.d.Sequences.EnrollContacts(ctx, seq.ID, req, !confirmed)
	if err != nil {
		if code := f.Fail(autoRetarget(err, enrFields(emails))); code != 0 {
			return u.enrView(w, r, code, d)
		}
		return err
	}
	if !confirmed {
		if res.WouldEnroll == 0 {
			d.Result, d.Skipped = &res, enrSkipped(res.Skipped)
			return u.enrView(w, r, http.StatusOK, d)
		}
		fields := f.Values
		fields.Set("emails", strings.Join(emails, ", "))
		details := append([]string{plural(res.WouldEnroll, "contact is", "contacts are") + " enrolled."}, enrSkipped(res.Skipped)...)
		at := max(req.AtStep, 1)
		when := "now"
		if req.StartAt != nil {
			when = u.fmtTime(*req.StartAt)
		}
		details = append(details, fmt.Sprintf("They start at step %d, due %s plus its delay.", at, when))
		if len(emails) > sequences.EnrollJobThreshold {
			details = append(details, "This many contacts are enrolled in the background; the result appears in the enrollments list as they are added.")
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:        fmt.Sprintf("Enrol %s?", plural(res.WouldEnroll, "contact", "contacts")),
			Message:      fmt.Sprintf("This is what enrolling these addresses in %q does:", seq.Name),
			Details:      details,
			Action:       seqURL(seq.ID, "/enroll"),
			Fields:       fields,
			ConfirmLabel: "Enrol " + plural(res.WouldEnroll, "contact", "contacts"),
			CancelURL:    seqURL(seq.ID, "/enroll"),
			Nav:          "sequences", Crumbs: seqCrumbs(seq),
		})
	}
	switch {
	case res.Job != nil:
		u.Flash(r, FlashInfo, fmt.Sprintf("Enrolling %s in the background (job %d). Reload the enrollments list to follow it.",
			plural(len(emails), "address", "addresses"), res.Job.ID))
	default:
		msg := plural(res.Enrolled, "contact", "contacts") + " enrolled."
		if sk := enrSkipped(res.Skipped); len(sk) > 0 {
			msg += " " + strings.Join(sk, "; ") + "."
		}
		u.Flash(r, FlashSuccess, msg)
	}
	u.Redirect(w, r, seqURL(seq.ID, "/enrollments"))
	return nil
}
