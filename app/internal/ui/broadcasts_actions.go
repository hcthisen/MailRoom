package ui

// Broadcast actions (B-2, B-5, B-7, B-9): test send, schedule, unschedule,
// send now (confirmed, with the dry-run recipient count), pause, resume
// (a circuit-breaker pause needs the confirm checkbox), cancel and delete
// (confirmed) and duplicate. Every action posts the version the page was
// loaded at, which the service checks like an If-Match.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"mailroom/internal/broadcasts"
)

func (u *UI) registerBroadcastActions() {
	post := http.MethodPost
	p := broadcastsPath + "/{id}"
	u.handle(Action{Method: post, Path: p + "/test", OperationID: "send-broadcast-test", Uses: []string{"list-contacts", "get-contact"}, Handler: u.broadcastTest})
	u.handle(Action{Method: post, Path: p + "/schedule", OperationID: "schedule-broadcast", Uses: []string{"send-broadcast"}, Handler: u.broadcastSchedule})
	u.handle(Action{Method: post, Path: p + "/unschedule", OperationID: "unschedule-broadcast", Handler: u.broadcastUnschedule})
	u.handle(Action{Method: post, Path: p + "/send", OperationID: "send-broadcast", Handler: u.broadcastSend})
	u.handle(Action{Method: post, Path: p + "/pause", OperationID: "pause-broadcast", Handler: u.broadcastPause})
	u.handle(Action{Method: post, Path: p + "/resume", OperationID: "resume-broadcast", Handler: u.broadcastResume})
	u.handle(Action{Method: post, Path: p + "/cancel", OperationID: "cancel-broadcast", Uses: []string{"get-broadcast-stats"},
		Handler: u.broadcastCancel})
	u.handle(Action{Method: post, Path: p + "/duplicate", OperationID: "duplicate-broadcast", Handler: u.broadcastDuplicate})
	u.handle(Action{Method: post, Path: p + "/delete", OperationID: "delete-broadcast", Uses: []string{"get-broadcast"},
		Handler: u.broadcastDelete})
}

// broadcastActionFailed ends an action whose service call failed: a user
// error (conflict, stale version, validation) becomes an error flash on
// the detail page; anything else (not found, internal) is returned.
func (u *UI) broadcastActionFailed(w http.ResponseWriter, r *http.Request, id int64, err error) error {
	f := EmptyForm()
	if f.Fail(err) == 0 {
		return err
	}
	msg := f.Message
	if msg == "" {
		for field, m := range f.Errors {
			msg = field + ": " + m
		}
	}
	u.Flash(r, FlashError, msg)
	u.Redirect(w, r, broadcastURL(id))
	return nil
}

// broadcastAction runs a one-step transition and flashes ok on success.
func (u *UI) broadcastAction(w http.ResponseWriter, r *http.Request, ok string,
	do func(ctx context.Context, id, version int64) (broadcasts.Broadcast, error)) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	if _, err := do(r.Context(), id, formVersion(f)); err != nil {
		return u.broadcastActionFailed(w, r, id, err)
	}
	u.Flash(r, FlashSuccess, ok)
	u.Redirect(w, r, broadcastURL(id))
	return nil
}

// splitAddresses splits the test-send box (commas, semicolons, spaces or
// lines) into addresses; the service validates and de-duplicates them.
func splitAddresses(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
}

// broadcastTest queues a test to 1-5 addresses (send-broadcast-test).
func (u *UI) broadcastTest(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	var contactID int64
	c, err := u.lookupContact(r.Context(), "contact", f.Get("contact"))
	switch {
	case err == nil && c != nil:
		contactID = c.ID
	case err != nil && f.Fail(err) == 0:
		return err
	}
	to := splitAddresses(f.Get("to"))
	if len(to) == 0 {
		f.AddError("to", "enter at least one address")
	}
	var res broadcasts.BroadcastTestResult
	if !f.HasErrors() {
		res, err = u.d.Broadcasts.SendTest(r.Context(), id, to, contactID)
		if err == nil {
			u.Flash(r, FlashSuccess, fmt.Sprintf("Test queued to %s. Tests are skipped for opted-out or suppressed addresses and don't count in the stats.",
				strings.Join(res.To, ", ")))
			u.Redirect(w, r, broadcastURL(id))
			return nil
		}
		if failBroadcastForm(f, err) == 0 {
			return err
		}
	}
	// The page's test box is the editor's (send-test-draft); this API-parity
	// action reports its problems as a flash.
	u.Flash(r, FlashError, "The test was not sent: "+formProblem(f))
	u.Redirect(w, r, broadcastURL(id))
	return nil
}

// broadcastSchedule schedules a draft at a local date and time in the
// settings time zone (schedule-broadcast).
func (u *UI) broadcastSchedule(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	at, err := u.ParseInputTime("at", f.Get("at"))
	if err == nil && !Confirmed(r) {
		// Like Send now: confirm with the exact number of recipients first
		// (the send dry run checks the audience, sender and footer too).
		if !at.After(u.clock.Now()) {
			err = fieldError("at", "must be in the future")
		} else {
			var res broadcasts.BroadcastSendResult
			if res, err = u.d.Broadcasts.Send(r.Context(), id, formVersion(f), true); err == nil {
				b := res.Broadcast
				subject, warnings, err := u.confirmFacts(r.Context(), b)
				if err != nil {
					return err
				}
				details := append([]string{
					plural(int(res.Recipients), "recipient", "recipients") + " right now: " + recipientsLabel(b.Audience) +
						". Who gets it is decided when it starts, so contacts who join before then get it too.",
					subject,
				}, warnings...)
				if res.Recipients == 0 {
					details = append(details, "Nobody would receive it right now. Check the audience.")
				}
				return u.RenderConfirm(w, r, Confirm{
					Title:   fmt.Sprintf("Schedule %s for %s?", b.Name, u.fmtTime(&at)),
					Message: fmt.Sprintf("It starts by itself at %s (%s). Until then you can still edit or unschedule it.", u.fmtTime(&at), u.location().String()),
					Details: details, Action: broadcastURL(id) + "/schedule", Fields: f.Values,
					ConfirmLabel: "Schedule for " + plural(int(res.Recipients), "recipient", "recipients"),
					CancelURL:    broadcastURL(id), Nav: "broadcasts",
					Crumbs: []Crumb{{"Broadcasts", broadcastsPath}, {b.Name, broadcastURL(id)}},
				})
			}
		}
	}
	if err == nil {
		var b broadcasts.Broadcast
		if b, err = u.d.Broadcasts.Schedule(r.Context(), id, at, formVersion(f)); err == nil {
			u.Flash(r, FlashSuccess, "Scheduled for "+u.fmtTime(b.ScheduledAt)+" ("+u.location().String()+"). It starts by itself then.")
			u.Redirect(w, r, broadcastURL(id))
			return nil
		}
	}
	st := failBroadcastForm(f, err)
	if st == 0 {
		return err
	}
	if f.Err("at") == "" {
		// A conflict, a stale version or a sender or audience problem is
		// not about the date field: say it at the top of the page.
		return u.broadcastActionFailed(w, r, id, err)
	}
	return u.renderBroadcast(w, r, st, id, func(d *broadcastShowData) { d.ScheduleForm = f })
}

// broadcastUnschedule moves a scheduled broadcast back to draft.
func (u *UI) broadcastUnschedule(w http.ResponseWriter, r *http.Request) error {
	return u.broadcastAction(w, r, "Unscheduled: the broadcast is a draft again.", u.d.Broadcasts.Unschedule)
}

// broadcastSend starts a broadcast now. The first step runs the send dry
// run and shows the recipient count; the confirmed second step sends.
func (u *UI) broadcastSend(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	v := formVersion(f)
	if !Confirmed(r) {
		res, err := u.d.Broadcasts.Send(ctx, id, v, true)
		if err != nil {
			return u.broadcastActionFailed(w, r, id, err)
		}
		b := res.Broadcast
		subject, warnings, err := u.confirmFacts(ctx, b)
		if err != nil {
			return err
		}
		details := append([]string{
			plural(int(res.Recipients), "recipient", "recipients") + ": " + recipientsLabel(b.Audience),
			subject,
		}, warnings...)
		if len(res.MissingTags) > 0 {
			details = append(details, "Warning: the segment refers to tags that don't exist ("+strings.Join(res.MissingTags, ", ")+
				"). Rules on them match nobody (or everybody, for \"doesn't have\"). Check the segment before you send.")
		}
		if res.Recipients == 0 {
			details = append(details, "Nobody would receive it. Check the audience.")
		}
		label := "Send now to " + plural(int(res.Recipients), "recipient", "recipients")
		if len(warnings) > 0 {
			label = "Send anyway to " + plural(int(res.Recipients), "recipient", "recipients")
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:   "Send " + b.Name + " now?",
			Message: "Sending starts right away and can't be undone. You can pause it while it sends; contacts who unsubscribe or bounce before their email goes out are skipped.",
			Details: details, Action: broadcastURL(id) + "/send", Fields: f.Values,
			ConfirmLabel: label, CancelURL: broadcastURL(id), Danger: true, Nav: "broadcasts",
			Crumbs: []Crumb{{"Broadcasts", broadcastsPath}, {b.Name, broadcastURL(id)}},
		})
	}
	res, err := u.d.Broadcasts.Send(ctx, id, v, false)
	if err != nil {
		return u.broadcastActionFailed(w, r, id, err)
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("Sending started: %s being queued. The numbers below update as it goes.",
		plural(int(res.Recipients), "recipient is", "recipients are")))
	u.Redirect(w, r, broadcastURL(id)+"/stats")
	return nil
}

// recipientsLabel says who an audience sends to right now.
func recipientsLabel(a broadcasts.BroadcastAudience) string {
	const tail = " whose address isn't suppressed, counted just now"
	if a.Type == broadcasts.AudienceSegment {
		name := a.SegmentName
		if name == "" {
			name = fmt.Sprintf("%d", a.SegmentID)
		}
		return "the active contacts in the segment " + name + tail
	}
	return "every active contact" + tail
}

// broadcastPause pauses a sending broadcast.
func (u *UI) broadcastPause(w http.ResponseWriter, r *http.Request) error {
	return u.broadcastAction(w, r, "Paused. Queued emails wait until you resume or cancel.", u.d.Broadcasts.Pause)
}

// broadcastResume continues a paused broadcast. One paused by the circuit
// breaker needs the confirm_resume checkbox (the service's confirm).
func (u *UI) broadcastResume(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	_, err = u.d.Broadcasts.Resume(r.Context(), id, f.Checked("confirm_resume"), formVersion(f))
	if err == nil {
		u.Flash(r, FlashSuccess, "Resumed: sending continues.")
		u.Redirect(w, r, broadcastURL(id))
		return nil
	}
	st := failBroadcastForm(f, err)
	if st == 0 {
		return err
	}
	if f.Err("confirm_resume") == "" {
		return u.broadcastActionFailed(w, r, id, err)
	}
	f.Errors["confirm_resume"] = "Tick this box to confirm you checked the bounce and complaint numbers."
	return u.renderBroadcast(w, r, st, id, func(d *broadcastShowData) { d.ResumeForm = f })
}

// broadcastCancel cancels a draft, scheduled or paused broadcast after a
// confirmation that says how many queued emails will be skipped.
func (u *UI) broadcastCancel(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	if !Confirmed(r) {
		b, err := u.d.Broadcasts.Get(ctx, id)
		if err != nil {
			return err
		}
		st, err := u.d.Broadcasts.Stats(ctx, id)
		if err != nil {
			return err
		}
		details := []string{"Status now: " + b.Status}
		if st.Queued > 0 {
			details = append(details, plural(int(st.Queued), "queued email", "queued emails")+" will not be sent.")
		}
		if st.Sent > 0 {
			details = append(details, plural(int(st.Sent), "email was", "emails were")+" already sent; they stay in the stats.")
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:   "Cancel " + b.Name + "?",
			Message: "A cancelled broadcast can't be restarted. You can duplicate it later to make a new draft with the same content.",
			Details: details, Action: broadcastURL(id) + "/cancel", Fields: f.Values,
			ConfirmLabel: "Cancel broadcast", CancelURL: broadcastURL(id), Danger: true, Nav: "broadcasts",
			Crumbs: []Crumb{{"Broadcasts", broadcastsPath}, {b.Name, broadcastURL(id)}},
		})
	}
	if _, err := u.d.Broadcasts.Cancel(ctx, id, formVersion(f)); err != nil {
		return u.broadcastActionFailed(w, r, id, err)
	}
	u.Flash(r, FlashSuccess, "Broadcast cancelled.")
	u.Redirect(w, r, broadcastURL(id))
	return nil
}

// broadcastDuplicate makes a new draft from any broadcast and opens its
// edit form.
func (u *UI) broadcastDuplicate(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	b, err := u.d.Broadcasts.Duplicate(r.Context(), id)
	if err != nil {
		return u.broadcastActionFailed(w, r, id, err)
	}
	u.Flash(r, FlashSuccess, "Copy created as a draft. Check the content and the audience before you send it.")
	u.Redirect(w, r, broadcastURL(b.ID)+"/edit")
	return nil
}

// broadcastDelete deletes a draft or cancelled broadcast after a
// confirmation.
func (u *UI) broadcastDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	if !Confirmed(r) {
		b, err := u.d.Broadcasts.Get(ctx, id)
		if err != nil {
			return err
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:   "Delete " + b.Name + "?",
			Message: "The broadcast and its email are deleted for good. This can't be undone.",
			Details: []string{"Status: " + b.Status, "Subject: " + b.Email.Subject},
			Action:  broadcastURL(id) + "/delete", Fields: f.Values,
			ConfirmLabel: "Delete broadcast", CancelURL: broadcastURL(id), Danger: true, Nav: "broadcasts",
			Crumbs: []Crumb{{"Broadcasts", broadcastsPath}, {b.Name, broadcastURL(id)}},
		})
	}
	if err := u.d.Broadcasts.Delete(ctx, id, formVersion(f)); err != nil {
		return u.broadcastActionFailed(w, r, id, err)
	}
	u.Flash(r, FlashSuccess, "Broadcast deleted.")
	u.Redirect(w, r, broadcastsPath)
	return nil
}
