package ui

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"mailroom/internal/sequences"
	"mailroom/internal/settings"
)

// Sequences pages (PRD §6.4 S-1..S-10, A-2): list, create, settings,
// pause/resume and delete. Steps, enrollments and YAML import/export are
// in automation_steps.go, automation_enrollments.go and automation_yaml.go.

// seqPath is the UI address of the sequences list.
const seqPath = Prefix + "/sequences"

func init() {
	registerGroup("automation", func(u *UI) {
		u.registerSequences()
		u.registerSteps()
		u.registerEnrollments()
		u.registerSequenceYAML()
		u.registerWebforms()
	})
}

func (u *UI) registerSequences() {
	u.handle(Action{Method: http.MethodGet, Path: seqPath, OperationID: "list-sequences",
		Uses: []string{"get-sequence-stats"}, Handler: u.seqList})
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/new", OperationID: "list-tags", Handler: u.seqNew})
	u.handle(Action{Method: http.MethodPost, Path: seqPath, OperationID: "create-sequence", Handler: u.seqCreate})
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/{id}", OperationID: "get-sequence",
		Uses: []string{"get-sequence-stats", "get-settings"}, Handler: u.seqShow})
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/{id}/edit", OperationID: "get-sequence",
		Uses: []string{"list-tags"}, Handler: u.seqEdit})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}", OperationID: "update-sequence", Handler: u.seqUpdate})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/pause", OperationID: "pause-sequence",
		Handler: u.seqSetStatus(sequences.StatusPaused)})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/resume", OperationID: "resume-sequence", Uses: []string{"get-sequence"},
		Handler: u.seqSetStatus(sequences.StatusActive)})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/delete", OperationID: "delete-sequence",
		Uses: []string{"get-sequence", "get-sequence-stats"}, Handler: u.seqDelete})
}

// seqURL is the UI address of sequence id, plus suffix.
func seqURL(id int64, suffix string) string {
	return seqPath + "/" + strconv.FormatInt(id, 10) + suffix
}

// seqCrumbs are the breadcrumbs of a page below sequence seq.
func seqCrumbs(seq sequences.Sequence) []Crumb {
	return []Crumb{{"Sequences", seqPath}, {seq.Name, seqURL(seq.ID, "")}}
}

// ---------------------------------------------------------------- list

type seqListItem struct {
	Seq      sequences.Sequence
	Stats    sequences.SequenceStats
	StatsErr bool
	Window   string
}

type seqListData struct {
	Items []seqListItem
	Pager Pager
}

// seqList lists the sequences with their enrollment counts (S-9).
func (u *UI) seqList(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	list, err := u.d.Sequences.List(ctx, p.After, p.Fetch())
	if err != nil {
		return err
	}
	page, pager := Paginate(r, p, list, func(s sequences.Sequence) int64 { return s.ID })
	d := seqListData{Pager: pager}
	for _, s := range page {
		it := seqListItem{Seq: s, Window: autoWindowText(s.SendWindow)}
		if st, err := u.d.Sequences.Stats(ctx, s.ID); err != nil {
			u.log.WarnContext(ctx, "ui: sequence stats", "sequence", s.ID, "err", err)
			it.StatsErr = true
		} else {
			it.Stats = st
		}
		d.Items = append(d.Items, it)
	}
	return u.Page(w, r, "automation/seq_list", View{Title: "Sequences", Nav: "sequences",
		Subtitle: "Drip emails sent one after another, started by a tag, a form, an import or by hand.", Data: d})
}

// ---------------------------------------------------------------- settings form

type seqFormData struct {
	Tabs    template.HTML // "" on the new page
	Form    *Form
	Seq     *sequences.Sequence // nil on the new page
	Tags    []string
	Days    []Option
	Reentry []Option
	Status  []Option
	Default string // the settings' default send window, described
}

var seqReentryOptions = []Option{
	{sequences.ReentryNever, "Never: a contact goes through it at most once"},
	{sequences.ReentryAfterCompletion, "After completion: again, but only after finishing it"},
	{sequences.ReentryAlways, "Always: again whenever not enrolled right now"},
}

var seqStatusOptions = []Option{
	{sequences.StatusActive, "On: emails are sent"},
	{sequences.StatusPaused, "Off: nothing is sent, contacts can still be enrolled"},
}

// seqFormView renders the create or settings form.
func (u *UI) seqFormView(w http.ResponseWriter, r *http.Request, status int, f *Form, seq *sequences.Sequence) error {
	d := seqFormData{Form: f, Seq: seq, Tags: u.autoTagSuggestions(r.Context()), Days: autoWeekdays,
		Reentry: seqReentryOptions, Status: seqStatusOptions, Default: u.autoDefaultWindow(r.Context())}
	v := View{Title: "New sequence", Nav: "sequences", Crumbs: []Crumb{{"Sequences", seqPath}}}
	if seq != nil {
		v.Title = "Settings"
		v.Subtitle = seq.Name
		v.Crumbs = seqCrumbs(*seq)
		tabs, err := seqTabs(*seq, "settings")
		if err != nil {
			return err
		}
		d.Tabs = tabs
	}
	v.Data = d
	return u.PageStatus(w, r, status, "automation/seq_form", v)
}

// seqFormValues fills a form from a sequence.
func seqFormValues(s sequences.Sequence) *Form {
	f := EmptyForm().Set("name", s.Name).Set("status", s.Status).Set("reentry", s.Reentry).
		Set("exit_tags", strings.Join(s.ExitTags, ", ")).Set("version", strconv.FormatInt(s.Version, 10))
	if s.TriggerTag != nil {
		f.Set("trigger_tag", *s.TriggerTag)
	}
	if s.ExitAddTag != nil {
		f.Set("exit_add_tag", *s.ExitAddTag)
	}
	if s.SendWindow != nil {
		f.Set("send_window.own", "1").Set("send_window.start", s.SendWindow.Start).Set("send_window.end", s.SendWindow.End)
		for _, d := range s.SendWindow.Days {
			f.Values.Add("send_window.days", strconv.Itoa(d))
		}
	} else {
		f.Set("send_window.start", "08:00").Set("send_window.end", "20:00")
		for _, d := range []string{"1", "2", "3", "4", "5"} {
			f.Values.Add("send_window.days", d)
		}
	}
	return f
}

// seqParsed is the sequence configuration a settings form posted.
type seqParsed struct {
	Name       string
	Status     string
	TriggerTag *string
	ExitTags   []string
	ExitAddTag *string
	Reentry    string
	SendWindow *settings.SendWindow
}

// seqParseForm reads the settings form. An unreadable weekday is a
// validation error (the checkboxes only send 1-7).
func seqParseForm(f *Form) (seqParsed, error) {
	p := seqParsed{Name: f.Get("name"), Status: f.Get("status"), ExitTags: ParseTags(f.Get("exit_tags")), Reentry: f.Get("reentry")}
	if p.ExitTags == nil {
		p.ExitTags = []string{}
	}
	if t := f.Get("trigger_tag"); t != "" {
		p.TriggerTag = &t
	}
	if t := f.Get("exit_add_tag"); t != "" {
		p.ExitAddTag = &t
	}
	if f.Checked("send_window.own") {
		sw := settings.SendWindow{Days: []int{}, Start: f.Get("send_window.start"), End: f.Get("send_window.end")}
		for _, s := range f.All("send_window.days") {
			d, err := strconv.Atoi(s)
			if err != nil {
				return p, fieldError("send_window.days", "must be weekdays")
			}
			sw.Days = append(sw.Days, d)
		}
		p.SendWindow = &sw
	}
	return p, nil
}

// seqFieldNames puts errors about exit tags ("exit_tags[2]") next to the
// exit tags input.
func seqFieldNames(tags []string) func(string, string) (string, string) {
	return autoFoldIndex("exit_tags", tags)
}

// seqNew is the empty create form.
func (u *UI) seqNew(w http.ResponseWriter, r *http.Request) error {
	f := seqFormValues(sequences.Sequence{Status: sequences.StatusPaused, Reentry: sequences.ReentryNever})
	f.Values.Del("version")
	f.Values.Del("send_window.own")
	return u.seqFormView(w, r, http.StatusOK, f, nil)
}

// seqCreate creates a sequence (create-sequence).
func (u *UI) seqCreate(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	p, err := seqParseForm(f)
	if err == nil {
		var seq sequences.Sequence
		seq, err = u.d.Sequences.Create(r.Context(), sequences.SequenceInput{
			Name: p.Name, Status: p.Status, TriggerTag: p.TriggerTag, ExitTags: p.ExitTags,
			ExitAddTag: p.ExitAddTag, Reentry: p.Reentry, SendWindow: p.SendWindow,
		})
		if err == nil {
			u.Flash(r, FlashSuccess, fmt.Sprintf("Sequence %q created. Add its first step.", seq.Name))
			u.Redirect(w, r, seqURL(seq.ID, "/steps/new"))
			return nil
		}
		err = autoRetarget(err, seqFieldNames(p.ExitTags))
	}
	if st := f.Fail(err); st != 0 {
		return u.seqFormView(w, r, st, f, nil)
	}
	return err
}

// seqEdit is the settings form of a sequence.
func (u *UI) seqEdit(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	seq, err := u.d.Sequences.Get(r.Context(), id)
	if err != nil {
		return err
	}
	return u.seqFormView(w, r, http.StatusOK, seqFormValues(seq), &seq)
}

// seqUpdate saves the settings form (update-sequence, If-Match from the
// form's version).
func (u *UI) seqUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	cur, err := u.d.Sequences.Get(ctx, id)
	if err != nil {
		return err
	}
	f := NewForm(r)
	p, err := seqParseForm(f)
	if err == nil {
		patch := sequences.SequencePatch{
			Name: &p.Name, Status: &p.Status, Reentry: &p.Reentry, ExitTags: &p.ExitTags,
			TriggerTag: sequences.OptionalString{Set: true, Value: p.TriggerTag},
			ExitAddTag: sequences.OptionalString{Set: true, Value: p.ExitAddTag},
			SendWindow: settings.OptionalSendWindow{Set: true, Value: p.SendWindow},
		}
		var seq sequences.Sequence
		seq, err = u.d.Sequences.Update(ctx, id, patch, autoVersion(f))
		if err == nil {
			u.Flash(r, FlashSuccess, "Settings saved.")
			u.Redirect(w, r, seqURL(seq.ID, ""))
			return nil
		}
		err = autoRetarget(err, seqFieldNames(p.ExitTags))
	}
	if st := f.Fail(err); st != 0 {
		return u.seqFormView(w, r, st, f, &cur)
	}
	return err
}

// ---------------------------------------------------------------- detail

type seqStepView struct {
	Step  sequences.SequenceStep
	Stats sequences.SequenceStepStats
	When  string // "2 days after step 1"
	First bool
	Last  bool
}

type seqShowData struct {
	Tabs     template.HTML
	Summary  template.HTML
	Seq      sequences.Sequence
	Stats    sequences.SequenceStats
	StatsErr bool
	Steps    []seqStepView
	Window   string // own send window, described
	Default  string // the settings' default window (when it has none)
}

// seqShow is a sequence's page: settings summary, numbers and steps with
// their stats (S-9).
func (u *UI) seqShow(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	seq, err := u.d.Sequences.Get(ctx, id)
	if err != nil {
		return err
	}
	d := seqShowData{Seq: seq, Window: autoWindowText(seq.SendWindow)}
	if seq.SendWindow == nil {
		d.Default = u.autoDefaultWindow(ctx)
	}
	byStep := map[int64]sequences.SequenceStepStats{}
	if st, err := u.d.Sequences.Stats(ctx, id); err != nil {
		u.log.WarnContext(ctx, "ui: sequence stats", "sequence", id, "err", err)
		d.StatsErr = true
	} else {
		d.Stats = st
		for _, s := range st.Steps {
			byStep[s.StepID] = s
		}
	}
	for i, st := range seq.Steps {
		d.Steps = append(d.Steps, seqStepView{Step: st, Stats: byStep[st.ID], When: seqStepWhen(st),
			First: i == 0, Last: i == len(seq.Steps)-1})
	}
	if d.Tabs, err = seqTabs(seq, "overview"); err != nil {
		return err
	}
	if d.Summary, err = u.seqSummary(seq, d.Window, d.Default); err != nil {
		return err
	}
	return u.Page(w, r, "automation/seq_show", View{Title: seq.Name, Nav: "sequences",
		Crumbs: []Crumb{{"Sequences", seqPath}}, Data: d})
}

// seqStepWhen says when a step is sent: "right after enrolment", "2 days
// after step 1".
func seqStepWhen(st sequences.SequenceStep) string {
	after := "enrolment"
	if st.Position > 1 {
		after = "step " + strconv.Itoa(st.Position-1)
	}
	if st.DelaySeconds <= 0 {
		return "right after " + after
	}
	return autoDelayText(st.DelaySeconds) + " after " + after
}

// ---------------------------------------------------------------- status and delete

// seqSetStatus pauses or resumes a sequence (pause-sequence,
// resume-sequence).
func (u *UI) seqSetStatus(status string) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := PathID(r, "id")
		if err != nil {
			return err
		}
		f := NewForm(r)
		if status == sequences.StatusActive && !Confirmed(r) {
			// Switching a drip on starts real emails: say what will happen.
			cur, err := u.d.Sequences.Get(r.Context(), id)
			if err != nil {
				return err
			}
			trigger := "No tag starts it: contacts join through a form, an import or by hand."
			if cur.TriggerTag != nil && *cur.TriggerTag != "" {
				trigger = fmt.Sprintf("Every active contact who gets the tag %q from now on is enrolled.", *cur.TriggerTag)
			}
			details := []string{trigger}
			if len(cur.Steps) == 0 {
				details = append(details, "It has no emails yet, so nothing would be sent. Add a step first.")
			}
			for _, st := range cur.Steps {
				when := "right after enrolment"
				if st.DelaySeconds > 0 {
					when = "wait " + st.Delay
					if st.Position > 1 {
						when += " after the previous email"
					} else {
						when += " after enrolment"
					}
				}
				details = append(details, fmt.Sprintf("Email %d (%s): %s", st.Position, when, st.Subject))
			}
			return u.RenderConfirm(w, r, Confirm{
				Title:   fmt.Sprintf("Turn on %s?", cur.Name),
				Message: "Once it is on, its emails go out to the contacts in it as each step falls due. Contacts already enrolled continue where they are. You can pause it again at any time.",
				Details: details, Action: seqURL(id, "") + "/resume", Fields: f.Values,
				ConfirmLabel: "Turn on sequence", CancelURL: seqURL(id, ""), Nav: "sequences",
				Crumbs: []Crumb{{"Sequences", seqPath}, {cur.Name, seqURL(id, "")}},
			})
		}
		seq, err := u.d.Sequences.SetStatus(r.Context(), id, status, autoVersion(f))
		if err != nil {
			return err
		}
		if status == sequences.StatusPaused {
			u.Flash(r, FlashSuccess, fmt.Sprintf("%q is off. Nothing is sent until you switch it on again; contacts can still be enrolled.", seq.Name))
		} else {
			u.Flash(r, FlashSuccess, fmt.Sprintf("%q is on. Steps that are due go out at the next scheduler run (within a minute).", seq.Name))
		}
		u.Redirect(w, r, seqURL(id, ""))
		return nil
	}
}

// seqDelete deletes a sequence after a confirmation that says how many
// active enrollments end (delete-sequence; force when there are any).
func (u *UI) seqDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	if !Confirmed(r) {
		seq, err := u.d.Sequences.Get(ctx, id)
		if err != nil {
			return err
		}
		st, err := u.d.Sequences.Stats(ctx, id)
		if err != nil {
			return err
		}
		fields := f.Values
		fields.Set("version", strconv.FormatInt(seq.Version, 10))
		details := []string{
			plural(len(seq.Steps), "step is", "steps are") + " deleted; their emails are kept under Content, Emails, where you can delete them.",
			plural(st.Completed+st.Exited, "finished enrollment is", "finished enrollments are") + " removed from the history.",
		}
		if st.Active > 0 {
			fields.Set("force", "1")
			details = append([]string{plural(st.Active, "active enrollment ends", "active enrollments end") +
				" now: those contacts get no further steps, and emails already queued for them are not sent."}, details...)
		} else {
			fields.Del("force")
		}
		details = append(details, "Forms that start this sequence stop enrolling.")
		return u.RenderConfirm(w, r, Confirm{
			Title:        fmt.Sprintf("Delete %q?", seq.Name),
			Message:      "This deletes the sequence for good. It can't be undone.",
			Details:      details,
			Action:       seqURL(id, "/delete"),
			Fields:       fields,
			ConfirmLabel: "Delete sequence",
			CancelURL:    seqURL(id, ""),
			Danger:       true,
			Nav:          "sequences",
			Crumbs:       seqCrumbs(seq),
		})
	}
	res, err := u.d.Sequences.Delete(ctx, id, autoVersion(f), f.Checked("force"))
	if err != nil {
		return err
	}
	msg := "Sequence deleted."
	if res.ExitedEnrollments > 0 {
		msg = fmt.Sprintf("Sequence deleted; %s ended.", plural(res.ExitedEnrollments, "active enrollment", "active enrollments"))
	}
	u.Flash(r, FlashSuccess, msg)
	u.Redirect(w, r, seqPath)
	return nil
}

// seqGet loads the sequence of the request's {id}.
func (u *UI) seqGet(ctx context.Context, r *http.Request) (sequences.Sequence, error) {
	id, err := PathID(r, "id")
	if err != nil {
		return sequences.Sequence{}, err
	}
	return u.d.Sequences.Get(ctx, id)
}
