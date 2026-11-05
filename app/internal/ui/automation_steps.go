package ui

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/sequences"
)

// Sequence step pages (S-1, S-7, S-10): add, edit (email editor and
// delay), reorder with up/down, delete, and send a test.

func (u *UI) registerSteps() {
	stepBody := int64(content.MaxBodyBytes) + 64<<10
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/{id}/steps/new", OperationID: "get-sequence", Handler: u.stepNew})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/steps", OperationID: "add-sequence-step",
		MaxBodyBytes: stepBody, Handler: u.stepCreate})
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/{id}/steps/{step_id}", OperationID: "get-sequence",
		Uses: []string{"get-email", "get-sequence-stats"}, Handler: u.stepEdit})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/steps/{step_id}", OperationID: "update-sequence-step",
		MaxBodyBytes: stepBody, Handler: u.stepUpdate})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/steps/{step_id}/delete", OperationID: "delete-sequence-step",
		Uses: []string{"get-sequence", "get-sequence-stats"}, Handler: u.stepDelete})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/steps/{step_id}/move", OperationID: "reorder-sequence-steps",
		Uses: []string{"get-sequence", "get-sequence-stats"}, Handler: u.stepMove})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/{id}/steps/{step_id}/test", OperationID: "send-sequence-step-test",
		Uses: []string{"get-sequence", "get-email"}, Handler: u.stepTest})
}

// stepURL is the UI address of a step.
func stepURL(seqID, stepID int64, suffix string) string {
	return seqURL(seqID, "/steps/"+strconv.FormatInt(stepID, 10)+suffix)
}

type stepFormData struct {
	Seq       sequences.Sequence
	Step      *sequences.SequenceStep // nil on the add page
	Stats     *sequences.SequenceStepStats
	Form      *Form
	TestForm  *Form
	Units     []Option
	Positions []Option
	Owner     string // where a test goes by default
}

// stepView renders the add or edit page.
func (u *UI) stepView(w http.ResponseWriter, r *http.Request, status int, d stepFormData) error {
	d.Units = autoDelayOptions(d.Form.Get("delay_unit"))
	if d.TestForm == nil {
		d.TestForm = EmptyForm()
	}
	if u.d.Settings != nil {
		if st, err := u.d.Settings.Get(r.Context()); err == nil {
			d.Owner = st.OwnerEmail
		}
	}
	v := View{Nav: "sequences", Crumbs: seqCrumbs(d.Seq), Data: d}
	if d.Step == nil {
		v.Title = fmt.Sprintf("Add step %d", len(d.Seq.Steps)+1)
		d.Positions = []Option{{"", "At the end"}}
		for _, st := range d.Seq.Steps {
			d.Positions = append(d.Positions, Option{strconv.Itoa(st.Position), fmt.Sprintf("Before step %d (%s)", st.Position, truncate(40, st.Subject))})
		}
		v.Data = d
	} else {
		v.Title = fmt.Sprintf("Step %d", d.Step.Position)
		v.Subtitle = d.Step.Subject
	}
	return u.PageStatus(w, r, status, "automation/seq_step", v)
}

// stepFields puts the service's field names on the step form's inputs:
// the email content is "email.subject" etc. when adding, and the delay is
// entered as a number and a unit.
func stepFields(field, msg string) (string, string) {
	field = strings.TrimPrefix(field, "email.")
	if field == "delay_seconds" {
		field = "delay_value"
	}
	return field, msg
}

// stepNew is the add-step form.
func (u *UI) stepNew(w http.ResponseWriter, r *http.Request) error {
	seq, err := u.seqGet(r.Context(), r)
	if err != nil {
		return err
	}
	f := EmptyForm().Set("delay_value", "1").Set("delay_unit", "d").Set("format", content.FormatHTML).
		Set("version", strconv.FormatInt(seq.Version, 10))
	if len(seq.Steps) == 0 {
		f.Set("delay_value", "0")
	}
	return u.stepView(w, r, http.StatusOK, stepFormData{Seq: seq, Form: f})
}

// stepCreate adds a step with a new email (add-sequence-step).
func (u *UI) stepCreate(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	seq, err := u.seqGet(ctx, r)
	if err != nil {
		return err
	}
	f := NewForm(r)
	delay, ok := autoParseDelay(f)
	pos := 0
	if s := f.Get("position"); s != "" {
		if pos, err = strconv.Atoi(s); err != nil {
			f.AddError("position", "choose a position")
			ok = false
		}
	}
	if !ok {
		return u.stepView(w, r, http.StatusUnprocessableEntity, stepFormData{Seq: seq, Form: f})
	}
	in := sequences.StepInput{DelaySeconds: delay, Position: pos, Email: &sequences.StepEmail{
		Subject: f.Get("subject"), Preheader: f.Get("preheader"), Format: f.Get("format"), Body: f.Raw("body"),
	}}
	_, step, err := u.d.Sequences.AddStep(ctx, seq.ID, in, autoVersion(f))
	if err != nil {
		if code := f.Fail(autoRetarget(err, stepFields)); code != 0 {
			return u.stepView(w, r, code, stepFormData{Seq: seq, Form: f})
		}
		return err
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("Step %d added.", step.Position))
	u.Redirect(w, r, seqURL(seq.ID, "#steps"))
	return nil
}

// stepLoad loads the sequence and step of the request.
func (u *UI) stepLoad(r *http.Request) (sequences.Sequence, sequences.SequenceStep, error) {
	seq, err := u.seqGet(r.Context(), r)
	if err != nil {
		return seq, sequences.SequenceStep{}, err
	}
	stepID, err := PathID(r, "step_id")
	if err != nil {
		return seq, sequences.SequenceStep{}, err
	}
	for _, st := range seq.Steps {
		if st.ID == stepID {
			return seq, st, nil
		}
	}
	return seq, sequences.SequenceStep{}, fmt.Errorf("step %d: %w", stepID, core.ErrNotFound)
}

// stepEditData is the edit page's data from the stored step and email.
func (u *UI) stepEditData(r *http.Request, seq sequences.Sequence, st sequences.SequenceStep) (stepFormData, error) {
	ctx := r.Context()
	e, err := u.d.Content.GetEmail(ctx, nil, st.EmailID)
	if err != nil {
		return stepFormData{}, err
	}
	value, unit := autoSplitDelay(st.DelaySeconds)
	f := EmptyForm().Set("subject", e.Subject).Set("preheader", e.Preheader).Set("format", e.Format).Set("body", e.Body).
		Set("delay_value", value).Set("delay_unit", unit).Set("version", strconv.FormatInt(seq.Version, 10))
	d := stepFormData{Seq: seq, Step: &st, Form: f}
	if stats, err := u.d.Sequences.Stats(ctx, seq.ID); err == nil {
		for _, s := range stats.Steps {
			if s.StepID == st.ID {
				d.Stats = &s
			}
		}
	}
	return d, nil
}

// stepEdit is the edit-step form with the test send.
func (u *UI) stepEdit(w http.ResponseWriter, r *http.Request) error {
	seq, st, err := u.stepLoad(r)
	if err != nil {
		return err
	}
	d, err := u.stepEditData(r, seq, st)
	if err != nil {
		return err
	}
	return u.stepView(w, r, http.StatusOK, d)
}

// stepUpdate saves a step's delay and email content
// (update-sequence-step).
func (u *UI) stepUpdate(w http.ResponseWriter, r *http.Request) error {
	seq, st, err := u.stepLoad(r)
	if err != nil {
		return err
	}
	f := NewForm(r)
	delay, ok := autoParseDelay(f)
	d := stepFormData{Seq: seq, Step: &st, Form: f}
	if !ok {
		return u.stepView(w, r, http.StatusUnprocessableEntity, d)
	}
	subject, preheader, format, body := f.Get("subject"), f.Get("preheader"), f.Get("format"), f.Raw("body")
	_, saved, err := u.d.Sequences.UpdateStep(r.Context(), seq.ID, st.ID, sequences.StepPatch{
		DelaySeconds: &delay, Subject: &subject, Preheader: &preheader, Format: &format, Body: &body,
	}, autoVersion(f))
	if err != nil {
		if code := f.Fail(autoRetarget(err, stepFields)); code != 0 {
			return u.stepView(w, r, code, d)
		}
		return err
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("Step %d saved. Changes apply to emails not sent yet.", saved.Position))
	u.Redirect(w, r, seqURL(seq.ID, "#steps"))
	return nil
}

// stepTest queues a test of the step's email (send-sequence-step-test)
// to the addresses given, or to the owner's address.
func (u *UI) stepTest(w http.ResponseWriter, r *http.Request) error {
	seq, st, err := u.stepLoad(r)
	if err != nil {
		return err
	}
	tf := NewForm(r)
	to := autoSplitList(tf.Get("to"))
	res, err := u.d.Sequences.TestStep(r.Context(), seq.ID, st.ID, to, 0)
	if err != nil {
		code := tf.Fail(autoRetarget(err, autoFoldIndex("to", to)))
		if code == 0 {
			return err
		}
		u.Flash(r, FlashError, "The test was not sent: "+formProblem(tf))
		u.Redirect(w, r, stepURL(seq.ID, st.ID, ""))
		return nil
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("Test of step %d queued to %s. Tests have no working unsubscribe or tracking links.",
		st.Position, strings.Join(res.To, ", ")))
	u.Redirect(w, r, stepURL(seq.ID, st.ID, ""))
	return nil
}

// ---------------------------------------------------------------- delete and reorder

// stepChoice is the confirmation page of a step change that asks what
// happens to active enrollments (S-7).
type stepChoice struct {
	Title        string
	Message      string
	Details      []string
	Action       string
	Fields       map[string]string // hidden fields
	Choice       string            // name of the radio group ("" = no choice needed)
	Options      []Option
	Default      string
	ConfirmLabel string
	CancelURL    string
	Danger       bool
}

func (u *UI) stepChoicePage(w http.ResponseWriter, r *http.Request, seq sequences.Sequence, c stepChoice) error {
	return u.Page(w, r, "automation/seq_step_confirm", View{Title: c.Title, Nav: "sequences", Crumbs: seqCrumbs(seq), Data: c})
}

// stepDelete deletes a step after a confirmation that, when active
// enrollments wait for it, asks whether to move them to the next step or
// end them (delete-sequence-step, on_active).
func (u *UI) stepDelete(w http.ResponseWriter, r *http.Request) error {
	seq, st, err := u.stepLoad(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	if !Confirmed(r) {
		stats, err := u.d.Sequences.Stats(ctx, seq.ID)
		if err != nil {
			return err
		}
		waiting := 0
		for _, s := range stats.Steps {
			if s.StepID == st.ID {
				waiting = s.Waiting
			}
		}
		c := stepChoice{
			Title:        fmt.Sprintf("Delete step %d?", st.Position),
			Message:      fmt.Sprintf("Step %d (%q) is removed and the steps after it move up. Its email is kept under Content, Emails (delete it there if you no longer need it), and emails already queued for it are still sent.", st.Position, st.Subject),
			Action:       stepURL(seq.ID, st.ID, "/delete"),
			Fields:       map[string]string{"version": strconv.FormatInt(seq.Version, 10)},
			ConfirmLabel: "Delete step", CancelURL: seqURL(seq.ID, "#steps"), Danger: true,
		}
		if waiting > 0 {
			c.Details = []string{plural(waiting, "active enrollment is", "active enrollments are") + " waiting for this step."}
			c.Choice, c.Default = "on_active", sequences.OnActiveAdvance
			c.Options = []Option{
				{sequences.OnActiveAdvance, "Move them on: they get the following step when this one was due (or complete, if it is the last)"},
				{sequences.OnActiveExit, "End their enrollment: they get no further steps from this sequence"},
			}
		}
		return u.stepChoicePage(w, r, seq, c)
	}
	res, err := u.d.Sequences.DeleteStep(ctx, seq.ID, st.ID, f.Get("on_active"), autoVersion(f))
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("Step %d deleted.", st.Position)
	switch {
	case res.Exited > 0:
		msg += " " + plural(res.Exited, "enrollment was", "enrollments were") + " ended."
	case res.Advanced > 0:
		msg += " " + plural(res.Advanced, "enrollment moved", "enrollments moved") + " to the following step"
		if res.Completed > 0 {
			msg += fmt.Sprintf(" (%d completed)", res.Completed)
		}
		msg += "."
	}
	u.Flash(r, FlashSuccess, msg)
	u.Redirect(w, r, seqURL(seq.ID, "#steps"))
	return nil
}

// stepMove moves a step one place up or down (reorder-sequence-steps).
// With active enrollments it first asks whether they keep their next step
// or end (S-7).
func (u *UI) stepMove(w http.ResponseWriter, r *http.Request) error {
	seq, st, err := u.stepLoad(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	ids := make([]int64, 0, len(seq.Steps))
	for _, s := range seq.Steps {
		ids = append(ids, s.ID)
	}
	i := slices.Index(ids, st.ID)
	j := i - 1
	if f.Get("dir") == "down" {
		j = i + 1
	}
	if j < 0 || j >= len(ids) {
		u.Flash(r, FlashInfo, fmt.Sprintf("Step %d is already the %s step.", st.Position, map[bool]string{true: "first", false: "last"}[j < 0]))
		u.Redirect(w, r, seqURL(seq.ID, "#steps"))
		return nil
	}
	ids[i], ids[j] = ids[j], ids[i]
	if !Confirmed(r) {
		stats, err := u.d.Sequences.Stats(ctx, seq.ID)
		if err != nil {
			return err
		}
		if stats.Active > 0 {
			return u.stepChoicePage(w, r, seq, stepChoice{
				Title: fmt.Sprintf("Move step %d %s?", st.Position, map[bool]string{true: "up", false: "down"}[j < i]),
				Message: fmt.Sprintf("%q becomes step %d. %s in progress.", st.Subject, j+1,
					plural(stats.Active, "enrollment is", "enrollments are")),
				Action: stepURL(seq.ID, st.ID, "/move"),
				Fields: map[string]string{"version": strconv.FormatInt(seq.Version, 10), "dir": f.Get("dir")},
				Choice: "on_active", Default: sequences.OnActiveKeep,
				Options: []Option{
					{sequences.OnActiveKeep, "Keep their place: each gets its next step by ID and continues from its new position, skipping steps it already received"},
					{sequences.OnActiveEnd, "Move them to the end: they complete now and get no further steps"},
				},
				ConfirmLabel: "Move step", CancelURL: seqURL(seq.ID, "#steps"),
			})
		}
	}
	res, err := u.d.Sequences.ReorderSteps(ctx, seq.ID, ids, f.Get("on_active"), autoVersion(f))
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("%q is now step %d.", st.Subject, j+1)
	if res.Completed > 0 {
		msg += " " + plural(res.Completed, "enrollment was", "enrollments were") + " completed."
	}
	u.Flash(r, FlashSuccess, msg)
	u.Redirect(w, r, seqURL(seq.ID, "#steps"))
	return nil
}
