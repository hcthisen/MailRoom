package sending

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/outbox"
	"mailroom/internal/testutil"
)

// TestEligibilityMatrix checks every contact kind against every contact
// status: exactly the combinations core.CanReceive allows are sent.
func TestEligibilityMatrix(t *testing.T) {
	e := newEnv(t)
	bID, emailID := e.broadcast("sending")
	type tc struct {
		kind   core.MessageKind
		status core.ContactStatus
		id     int64
	}
	var cases []tc
	for i, st := range core.ContactStatuses {
		for j, kind := range []core.MessageKind{core.KindBroadcast, core.KindSequence, core.KindOptin, core.KindTest} {
			c := testutil.InsertContact(t, e.db, fmt.Sprintf("c%d-%d@example.org", i, j), st)
			var id int64
			switch kind {
			case core.KindBroadcast:
				id = e.broadcastMessage(bID, emailID, c)
			case core.KindSequence:
				id = e.sequenceMessage(c, "active")
			case core.KindOptin:
				id = e.optinMessage(c)
			case core.KindTest:
				id = e.testMessage(c.Email, c.ID)
			}
			cases = append(cases, tc{kind, st, id})
		}
	}
	e.sendDue()
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/%s", c.kind, c.status), func(t *testing.T) {
			r := e.row(c.id)
			if core.CanReceive(c.status, c.kind) {
				if r.Status != "sent" || r.SESID == "" {
					t.Fatalf("want sent, got %+v", r)
				}
				return
			}
			if r.Status != "skipped" || r.Error != "contact status is "+string(c.status) {
				t.Fatalf("want skipped, got %+v", r)
			}
		})
	}
	sent := 0
	for _, c := range cases {
		if core.CanReceive(c.status, c.kind) {
			sent++
		}
	}
	if e.provider.Count() != sent {
		t.Fatalf("provider sends = %d, want %d", e.provider.Count(), sent)
	}
}

// TestEligibilitySuppressionAndEnrollment covers the suppressions table
// (for every kind but alert), erased contacts and enrollment states.
func TestEligibilitySuppressionAndEnrollment(t *testing.T) {
	e := newEnv(t)
	bID, emailID := e.broadcast("sending")
	sup := func(email string) core.Contact {
		c := testutil.InsertContact(t, e.db, email, core.StatusActive)
		testutil.InsertSuppression(t, e.db, email, "manual")
		return c
	}
	pending := testutil.InsertContact(t, e.db, "pending-sup@example.org", core.StatusPending)
	testutil.InsertSuppression(t, e.db, pending.Email, "manual")
	erased := testutil.InsertContact(t, e.db, "erased@example.org", core.StatusActive)
	erasedMsg := e.broadcastMessage(bID, emailID, erased)
	testutil.Exec(t, e.db, `UPDATE messages SET contact_id = NULL, to_email = NULL WHERE id = ?`, erasedMsg)
	testutil.Exec(t, e.db, `DELETE FROM contacts WHERE id = ?`, erased.ID)
	gone := testutil.InsertContact(t, e.db, "gone@example.org", core.StatusActive)
	goneMsg := e.broadcastMessage(bID, emailID, gone)
	// A later contact keeps SQLite from reusing the deleted rowid.
	testutil.InsertContact(t, e.db, "filler@example.org", core.StatusActive)
	testutil.Exec(t, e.db, `PRAGMA foreign_keys = OFF`)
	testutil.Exec(t, e.db, `DELETE FROM contacts WHERE id = ?`, gone.ID)
	testutil.Exec(t, e.db, `PRAGMA foreign_keys = ON`)

	testutil.InsertSuppression(t, e.db, "owner@example.org", "manual")
	alertErr := e.svc.Alert(e.ctx, nil, "k1", "Title", "Body")
	if alertErr != nil {
		t.Fatal(alertErr)
	}
	var alertID int64
	if err := e.db.Read.QueryRow(`SELECT id FROM messages WHERE kind = 'alert'`).Scan(&alertID); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		id     int64
		status string
		reason string
	}{
		{"broadcast to suppressed address", e.broadcastMessage(bID, emailID, sup("s1@example.org")), "skipped", SkipSuppressed},
		{"sequence to suppressed address", e.sequenceMessage(sup("s2@example.org"), "active"), "skipped", SkipSuppressed},
		{"optin to suppressed address", e.optinMessage(pending), "skipped", SkipSuppressed},
		{"test to suppressed address", e.testMessage("s3@example.org", sup("s3@example.org").ID), "skipped", SkipSuppressed},
		{"test to suppressed plain address", func() int64 {
			testutil.InsertSuppression(t, e.db, "plain@example.org", "erased")
			return e.testMessage("plain@example.org", 0)
		}(), "skipped", SkipSuppressed},
		{"test to unsuppressed plain address", e.testMessage("friend@example.org", 0), "sent", ""},
		{"test to bounced contact", e.testMessage("b@example.org", testutil.InsertContact(t, e.db, "b@example.org", core.StatusBounced).ID), "skipped", "contact status is bounced"},
		{"alert to suppressed address", alertID, "sent", ""},
		{"erased contact", erasedMsg, "skipped", SkipContactErased},
		{"deleted contact", goneMsg, "skipped", SkipContactDeleted},
		{"exited enrollment", e.sequenceMessage(testutil.InsertContact(t, e.db, "x1@example.org", core.StatusActive), "exited"), "skipped", SkipEnrollmentExited},
		{"completed enrollment (last step)", e.sequenceMessage(testutil.InsertContact(t, e.db, "x2@example.org", core.StatusActive), "completed"), "sent", ""},
		{"removed enrollment", func() int64 {
			id := e.sequenceMessage(testutil.InsertContact(t, e.db, "x3@example.org", core.StatusActive), "active")
			testutil.Exec(t, e.db, `UPDATE messages SET enrollment_id = NULL WHERE id = ?`, id)
			return id
		}(), "skipped", SkipEnrollmentRemoved},
	}
	e.sendDue()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.row(tt.id)
			if r.Status != tt.status || r.Error != tt.reason {
				t.Fatalf("got %+v, want %s %q", r, tt.status, tt.reason)
			}
		})
	}
}

// TestFinalCheckRightBeforeSend: a contact that unsubscribes while the
// message is being rendered is not emailed.
func TestFinalCheckRightBeforeSend(t *testing.T) {
	e := newEnv(t)
	bID, emailID := e.broadcast("sending")
	c := testutil.InsertContact(t, e.db, "late@example.org", core.StatusActive)
	id := e.broadcastMessage(bID, emailID, c)
	e.svc.renderer = renderHook{e.renderer, func() {
		testutil.Exec(t, e.db, `UPDATE contacts SET status = 'unsubscribed' WHERE id = ?`, c.ID)
	}}
	e.sendDue()
	if r := e.row(id); r.Status != "skipped" || r.Error != "contact status is unsubscribed" || e.provider.Calls() != 0 {
		t.Fatalf("row = %+v, calls = %d", r, e.provider.Calls())
	}
}

type renderHook struct {
	core.Renderer
	fn func()
}

func (r renderHook) Render(ctx context.Context, in core.RenderInput) (core.Rendered, error) {
	out, err := r.Renderer.Render(ctx, in)
	r.fn()
	return out, err
}

func TestSendOutcomeAndHeaders(t *testing.T) {
	e := newEnv(t)
	bID, emailID := e.broadcast("sending")
	testutil.Exec(t, e.db, `UPDATE broadcasts SET from_name = 'Special Edition', reply_to = 'editor@example.com' WHERE id = ?`, bID)
	c := testutil.InsertContact(t, e.db, "alice@example.org", core.StatusActive)
	id := e.broadcastMessage(bID, emailID, c)
	// The contact corrected their address after the snapshot.
	testutil.Exec(t, e.db, `UPDATE contacts SET email = 'alice.new@example.org' WHERE id = ?`, c.ID)
	testID := e.testMessage("owner@example.org", 0)
	e.sendDue()

	r := e.row(id)
	if r.Status != "sent" || r.SESID == "" || r.Subject != fmt.Sprintf("Subject %d", id) || r.Attempts != 1 {
		t.Fatalf("row = %+v", r)
	}
	var to, sentAt string
	if err := e.db.Read.QueryRow(`SELECT to_email, sent_at FROM messages WHERE id = ?`, id).Scan(&to, &sentAt); err != nil || to != "alice.new@example.org" || sentAt != db.FormatTime(e.clock.Now()) {
		t.Fatalf("to = %q sent_at = %q err %v", to, sentAt, err)
	}
	sent := e.provider.SentTo("alice.new@example.org")
	if len(sent) != 1 {
		t.Fatalf("sent = %+v", e.provider.Sent())
	}
	m := sent[0]
	if m.From != "news@example.com" || m.ReplyTo != "editor@example.com" || m.Tags["kind"] != "broadcast" || m.Tags["message_id"] != fmt.Sprint(id) {
		t.Fatalf("outgoing = %+v", m)
	}
	msg := parseMessage(t, m.Raw)
	h := msg.Header
	if got := h.Get("From"); got != `"Special Edition" <news@example.com>` {
		t.Fatalf("From = %q", got)
	}
	if h.Get("List-Unsubscribe") != fmt.Sprintf("<https://newsletter.example.com/u/tok%d>", id) ||
		h.Get("List-Unsubscribe-Post") != "List-Unsubscribe=One-Click" {
		t.Fatalf("list headers = %v", h)
	}
	if got, want := h.Get("Feedback-ID"), fmt.Sprintf("b%d:broadcast:mailroom:example.com", bID); got != want {
		t.Fatalf("Feedback-ID = %q, want %q", got, want)
	}
	if !strings.HasPrefix(h.Get("Message-ID"), fmt.Sprintf("<mr.%d.", id)) || !strings.HasSuffix(h.Get("Message-ID"), "@example.com>") {
		t.Fatalf("Message-ID = %q", h.Get("Message-ID"))
	}
	in := e.renderer.input(t, id)
	if !in.Marketing || in.Contact == nil || in.Contact.ID != c.ID || in.EmailID != emailID || in.ToEmail != "alice.new@example.org" {
		t.Fatalf("render input = %+v", in)
	}

	// A test of an email: no List-Unsubscribe; settings sender.
	tm := e.provider.SentTo("owner@example.org")[0]
	th := parseMessage(t, tm.Raw).Header
	if th.Get("List-Unsubscribe") != "" || th.Get("Reply-To") != "<hello@example.com>" || th.Get("From") != `"Example News" <news@example.com>` {
		t.Fatalf("test headers = %v", th)
	}
	// A test send is never rendered as marketing: no working unsubscribe
	// link or tracking tied to the contact whose data it shows.
	if last := e.renderer.input(t, testID); last.Marketing || last.MessageID != testID {
		t.Fatalf("test render input = %+v", last)
	}
}

func TestRenderAndConfigFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *env)
		code  string
	}{
		{"render error", func(e *env) { e.renderer.err = errors.New("email 9 not found") }, "render"},
		{"no unsubscribe URL", func(e *env) { e.renderer.noUnsub = true }, "render"},
		// Written behind the settings service (it validates the domain), before
		// anything was read, so its cache is empty.
		{"from off the domain", func(e *env) {
			testutil.Exec(e.t, e.db, `UPDATE settings SET value = json_set(value, '$.sender.from_email', 'x@other.org') WHERE key = 'app'`)
		}, "config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.setup(e)
			bID, emailID := e.broadcast("sending")
			id := e.broadcastMessage(bID, emailID, testutil.InsertContact(t, e.db, "a@example.org", core.StatusActive))
			e.sendDue()
			r := e.row(id)
			if r.Status != "failed" || e.provider.Calls() != 0 {
				t.Fatalf("row = %+v calls %d", r, e.provider.Calls())
			}
			if n := e.count(`SELECT count(*) FROM events WHERE message_id = ? AND type = 'send_error' AND subtype = ?`, id, tt.code); n != 1 {
				t.Fatalf("send_error events = %d (%s)", n, r.Error)
			}
		})
	}
}

func TestRetryBackoffThenFail(t *testing.T) {
	e := newEnv(t)
	e.provider.FailAlways(&core.ProviderError{Retryable: true, Code: "TooManyRequestsException", Message: "slow down"})
	id := e.testMessage("owner@example.org", 0)
	for i, delay := range retryBackoff {
		if n := e.sendDue(); n != 1 {
			t.Fatalf("attempt %d: claimed %d", i+1, n)
		}
		r := e.row(id)
		want := db.FormatTime(e.clock.Now().Add(delay))
		if r.Status != "queued" || r.Attempts != i+1 || r.Next != want || !strings.Contains(r.Error, "slow down") {
			t.Fatalf("after attempt %d: %+v, want next %s", i+1, r, want)
		}
		e.clock.Advance(delay - time.Second)
		if n := e.sendDue(); n != 0 {
			t.Fatalf("sent before the backoff elapsed")
		}
		e.clock.Advance(time.Second)
	}
	e.sendDue()
	r := e.row(id)
	if r.Status != "failed" || r.Attempts != MaxAttempts || !strings.Contains(r.Error, "gave up after 5 attempts") {
		t.Fatalf("final = %+v", r)
	}
	if e.provider.Calls() != MaxAttempts || e.count(`SELECT count(*) FROM events WHERE message_id = ? AND type = 'send_error'`, id) != 1 {
		t.Fatalf("calls = %d", e.provider.Calls())
	}
}

func TestProviderErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		status   string
		attempts int
	}{
		{"permanent", &core.ProviderError{Code: "MessageRejected", Message: "bad content"}, "failed", 1},
		{"wrapped permanent", fmt.Errorf("ses: %w", &core.ProviderError{Code: "MailFromDomainNotVerifiedException"}), "failed", 1},
		// The provider may have accepted it: never retried (at most once).
		{"unclassified error: delivery unknown", errNetwork, "failed", 1},
		{"timeout: delivery unknown", context.DeadlineExceeded, "failed", 1},
		{"delivery unknown from SES", &core.ProviderError{Code: CodeDeliveryUnknown, Message: "no response"}, "failed", 1},
		{"retryable", &core.ProviderError{Retryable: true, Code: "InternalFailure"}, "queued", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.provider.FailNext(tt.err)
			id := e.testMessage("owner@example.org", 0)
			e.sendDue()
			r := e.row(id)
			if r.Status != tt.status || r.Attempts != tt.attempts {
				t.Fatalf("row = %+v", r)
			}
			wantEvents := 0
			if tt.status == "failed" {
				wantEvents = 1
			}
			if n := e.count(`SELECT count(*) FROM events WHERE message_id = ? AND type = 'send_error'`, id); n != wantEvents {
				t.Fatalf("events = %d", n)
			}
		})
	}
}

func TestGlobalPauseAndAutoResume(t *testing.T) {
	e := newEnv(t)
	e.provider.FailNext(&core.ProviderError{Code: CodeSendingPaused, Message: "SendingPausedException: sending paused for account"})
	first := e.testMessage("owner@example.org", 0)
	second := e.testMessage("friend@example.org", 0)
	e.sendDue()

	if r := e.row(first); r.Status != "queued" || r.Attempts != 0 {
		t.Fatalf("paused message = %+v", r)
	}
	if r := e.row(second); r.Status != "queued" || e.provider.Calls() != 1 {
		t.Fatalf("second = %+v, calls %d", r, e.provider.Calls())
	}
	st := e.state()
	if !st.Paused || st.Source != PauseSES || st.Since == nil || !strings.Contains(st.Reason, "SendingPausedException") {
		t.Fatalf("state = %+v", st)
	}
	alerts := func(key string) int {
		return e.count(`SELECT count(*) FROM messages WHERE kind = 'alert' AND json_extract(data, '$.key') = ?`, key)
	}
	if alerts(AlertSESPaused) != 1 {
		t.Fatal("pause alert not queued")
	}
	// Paused: nothing is claimed, not even alerts.
	if n := e.sendDue(); n != 0 {
		t.Fatalf("claimed %d while paused", n)
	}
	// The poller still sees sending disabled: no second alert.
	e.account.set(false, 14)
	if _, err := e.svc.PollAccount(e.ctx); err != nil {
		t.Fatal(err)
	}
	if alerts(AlertSESPaused) != 1 || !e.state().Paused {
		t.Fatalf("alerts = %d state %+v", alerts(AlertSESPaused), e.state())
	}
	// Sending enabled again: auto-resume with an alert, then everything goes out.
	e.account.set(true, 14)
	if _, err := e.svc.PollAccount(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.state().Paused || alerts(AlertSESResumed) != 1 {
		t.Fatalf("state = %+v", e.state())
	}
	e.sendDue()
	if e.row(first).Status != "sent" || e.row(second).Status != "sent" || e.provider.Count() != 4 {
		t.Fatalf("after resume: %+v %+v sent %d", e.row(first), e.row(second), e.provider.Count())
	}
}

func TestPollerPausesAndManualPauseSticks(t *testing.T) {
	e := newEnv(t)
	e.account.set(false, 14)
	a, err := e.svc.PollAccount(e.ctx)
	if err != nil || a.SendingEnabled || a.EnforcementStatus != "SHUTDOWN" || a.MaxSendRate != 14 {
		t.Fatalf("account = %+v %v", a, err)
	}
	st := e.state()
	if !st.Paused || st.Source != PauseSES || !strings.Contains(st.Reason, "SHUTDOWN") {
		t.Fatalf("state = %+v", st)
	}
	stored, err := e.svc.StoredAccount(e.ctx, nil)
	if err != nil || stored.Max24hSend != 50000 || stored.SentLast24h != 12 || !stored.ProductionAccess || !stored.FetchedAt.Equal(e.clock.Now()) {
		t.Fatalf("stored = %+v %v", stored, err)
	}

	// A manual pause is not lifted by the poller.
	if _, err := e.svc.Resume(e.ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Pause(e.ctx, "list check", false); err != nil {
		t.Fatal(err)
	}
	e.account.set(true, 14)
	if _, err := e.svc.PollAccount(e.ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.state(); !st.Paused || st.Source != PauseManual || st.Reason != "list check" {
		t.Fatalf("manual pause lifted: %+v", st)
	}

	// A failing poll keeps the last snapshot and reports the error.
	e.account.err = errNetwork
	if _, err := e.svc.PollAccount(e.ctx); err == nil {
		t.Fatal("want error")
	}
	if e.svc.currentAccount().MaxSendRate != 14 {
		t.Fatal("snapshot lost")
	}
}

func TestPausedBroadcastUntouched(t *testing.T) {
	e := newEnv(t)
	paused, pEmail := e.broadcast("paused")
	sending, sEmail := e.broadcast("sending")
	var pausedIDs []int64
	for i := range 3 {
		pausedIDs = append(pausedIDs, e.broadcastMessage(paused, pEmail, testutil.InsertContact(t, e.db, fmt.Sprintf("p%d@example.org", i), core.StatusActive)))
	}
	live := e.broadcastMessage(sending, sEmail, testutil.InsertContact(t, e.db, "live@example.org", core.StatusActive))
	seq := e.sequenceMessage(testutil.InsertContact(t, e.db, "seq@example.org", core.StatusActive), "active")

	if n := e.sendDue(); n != 2 {
		t.Fatalf("claimed %d, want 2", n)
	}
	for _, id := range pausedIDs {
		if r := e.row(id); r.Status != "queued" || r.Attempts != 0 {
			t.Fatalf("paused broadcast message touched: %+v", r)
		}
		if ok, err := e.svc.claim(e.ctx, id); ok || err != nil {
			t.Fatalf("claim of paused broadcast message = %v %v", ok, err)
		}
	}
	if e.row(live).Status != "sent" || e.row(seq).Status != "sent" {
		t.Fatal("live messages not sent")
	}
	// Resuming the broadcast releases its messages.
	testutil.Exec(t, e.db, `UPDATE broadcasts SET status = 'sending' WHERE id = ?`, paused)
	if n := e.sendDue(); n != 3 {
		t.Fatalf("after resume claimed %d", n)
	}
}

func TestCompleteBroadcasts(t *testing.T) {
	e := newEnv(t)
	done, dEmail := e.broadcast("sending")
	short, sEmail := e.broadcast("sending")
	partial, pEmail := e.broadcast("sending")
	empty, _ := e.broadcast("sending")
	notDone, _ := e.broadcast("sending")
	pending, qEmail := e.broadcast("sending")
	// done: snapshot finished, all its messages end up sent or skipped.
	// short: snapshot finished with fewer rows than counted (contacts left in
	// between); it must still complete. empty: snapshot finished, 0 rows.
	for _, id := range []int64{done, short, empty, pending} {
		testutil.Exec(t, e.db, `UPDATE broadcasts SET snapshot_done_at = '2026-01-01T00:00:00Z' WHERE id = ?`, id)
	}
	testutil.Exec(t, e.db, `UPDATE broadcasts SET recipients_total = 2 WHERE id = ?`, done)
	testutil.Exec(t, e.db, `UPDATE broadcasts SET recipients_total = 5 WHERE id = ?`, short)
	// partial: every counted row is there, but the snapshot is not done.
	testutil.Exec(t, e.db, `UPDATE broadcasts SET recipients_total = 1 WHERE id = ?`, partial)
	e.broadcastMessage(done, dEmail, testutil.InsertContact(t, e.db, "d1@example.org", core.StatusActive))
	e.broadcastMessage(done, dEmail, testutil.InsertContact(t, e.db, "d2@example.org", core.StatusUnsubscribed)) // skipped counts as done
	e.broadcastMessage(short, sEmail, testutil.InsertContact(t, e.db, "s1@example.org", core.StatusActive))
	e.broadcastMessage(partial, pEmail, testutil.InsertContact(t, e.db, "p1@example.org", core.StatusActive))
	later := e.broadcastMessage(pending, qEmail, testutil.InsertContact(t, e.db, "q1@example.org", core.StatusActive))
	testutil.Exec(t, e.db, `UPDATE messages SET next_attempt_at = ? WHERE id = ?`, db.FormatTime(e.clock.Now().Add(time.Hour)), later)
	e.sendDue()

	status := func(id int64) (string, int) {
		var st string
		var v int
		if err := e.db.Read.QueryRow(`SELECT status, version FROM broadcasts WHERE id = ?`, id).Scan(&st, &v); err != nil {
			t.Fatal(err)
		}
		return st, v
	}
	for _, tt := range []struct {
		name string
		id   int64
		want string
	}{
		{"done", done, "sent"}, {"short snapshot", short, "sent"}, {"empty snapshot", empty, "sent"},
		{"snapshot not done", partial, "sending"}, {"no snapshot yet", notDone, "sending"}, {"message still queued", pending, "sending"},
	} {
		if st, _ := status(tt.id); st != tt.want {
			t.Errorf("%s: status %s, want %s", tt.name, st, tt.want)
		}
	}
	if _, v := status(done); v != 2 {
		t.Fatalf("version = %d", v)
	}
	if e.count(`SELECT count(*) FROM broadcasts WHERE id = ? AND finished_at IS NOT NULL`, done) != 1 {
		t.Fatal("finished_at not set")
	}
}

func TestRecoverInterrupted(t *testing.T) {
	e := newEnv(t)
	a := e.testMessage("owner@example.org", 0)
	b := e.testMessage("friend@example.org", 0)
	testutil.Exec(t, e.db, `UPDATE messages SET status = 'sending' WHERE id = ?`, a)
	n, err := e.svc.RecoverInterrupted(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("recovered %d %v", n, err)
	}
	if r := e.row(a); r.Status != "failed" || r.Error != InterruptedError {
		t.Fatalf("a = %+v", r)
	}
	if e.row(b).Status != "queued" || e.count(`SELECT count(*) FROM events WHERE message_id = ? AND subtype = 'interrupted'`, a) != 1 {
		t.Fatal("recovery touched the wrong rows")
	}
	// The worker recovers at start too, and never sends the interrupted one.
	testutil.Exec(t, e.db, `UPDATE messages SET status = 'sending' WHERE id = ?`, b)
	runWorker(t, e, func() bool { return e.row(b).Status != "sending" })
	if e.row(b).Status != "failed" || e.provider.Calls() != 0 {
		t.Fatalf("b = %+v calls %d", e.row(b), e.provider.Calls())
	}
}

// runWorker runs the sender worker until done() holds (or 10 s), then
// stops it and waits for it to return.
func runWorker(t *testing.T, e *env, done func() bool) {
	t.Helper()
	stop := startWorker(t, e)
	waitFor(t, done)
	stop()
}

// startWorker starts the sender worker and returns a func that stops it
// and waits for it to return.
func startWorker(t *testing.T, e *env) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(e.ctx)
	errc := make(chan error, 1)
	w := e.svc.Workers()[0]
	go func() { errc <- w.Run(ctx) }()
	return func() {
		t.Helper()
		cancel()
		if err := <-errc; err != nil {
			t.Fatalf("worker: %v", err)
		}
	}
}

// waitFor polls done for up to 10 s.
func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestNoDoubleSend runs the worker (4 goroutines) while other goroutines
// repeatedly dispatch the same IDs and call SendDue: every message is sent
// exactly once.
func TestNoDoubleSend(t *testing.T) {
	e := newEnv(t)
	e.setRateOverride(1000)
	bID, emailID := e.broadcast("sending")
	const n = 200
	var ids []int64
	for i := range n {
		ids = append(ids, e.broadcastMessage(bID, emailID, testutil.InsertContact(t, e.db, fmt.Sprintf("r%d@example.org", i), core.StatusActive)))
	}
	// Start the worker first: its startup recovery assumes no other sender
	// in the process has claimed anything yet.
	stop := startWorker(t, e)
	waitFor(t, func() bool { return e.provider.Count() > 0 })
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ids {
				id := ids[(i+g*7)%len(ids)]
				if err := e.svc.process(e.ctx, id); err != nil {
					t.Errorf("process: %v", err)
				}
			}
			if _, err := e.svc.SendDue(e.ctx); err != nil {
				t.Errorf("SendDue: %v", err)
			}
		}()
	}
	wg.Wait()
	waitFor(t, func() bool {
		return e.count(`SELECT count(*) FROM messages WHERE status IN ('queued','sending')`) == 0
	})
	stop()
	if e.provider.Count() != n || e.provider.Calls() != n {
		t.Fatalf("sent %d (calls %d), want %d", e.provider.Count(), e.provider.Calls(), n)
	}
	seen := map[string]bool{}
	for _, m := range e.provider.Sent() {
		if seen[m.To] {
			t.Fatalf("%s emailed twice", m.To)
		}
		seen[m.To] = true
	}
	if e.count(`SELECT count(*) FROM messages WHERE status = 'sent' AND ses_message_id IS NOT NULL`) != n {
		t.Fatal("not every message recorded as sent")
	}
}

// TestWorkerRespectsRate: with rate 5/s and a fake clock that only moves
// when the bucket waits, 25 sends take at least (25-5)/5 = 4 s.
func TestWorkerRespectsRate(t *testing.T) {
	e := newEnv(t)
	e.setRateOverride(5)
	start := e.clock.Now()
	for i := range 25 {
		e.testMessage(fmt.Sprintf("t%d@example.org", i), 0)
	}
	runWorker(t, e, func() bool { return e.provider.Count() == 25 })
	if got := e.clock.Now().Sub(start); got < 4*time.Second {
		t.Fatalf("25 sends at 5/s took %v of clock time", got)
	}
	if r := e.svc.bucket.Rate(); r != 5 {
		t.Fatalf("rate = %v", r)
	}
}

func TestWorkers(t *testing.T) {
	e := newEnv(t)
	ws := e.svc.Workers()
	if len(ws) != 2 || ws[0].Name() != "sender" || ws[1].Name() != "ses-account-poller" {
		t.Fatalf("workers = %v", ws)
	}
	e.svc.account = nil
	if ws := e.svc.Workers(); len(ws) != 1 {
		t.Fatalf("without SES account API: %d workers", len(ws))
	}
	// Only one sender may run at a time.
	e.svc.running.Lock()
	defer e.svc.running.Unlock()
	if err := ws[0].Run(e.ctx); err == nil {
		t.Fatal("second sender must refuse to run")
	}
}

func TestPollerWorker(t *testing.T) {
	e := newEnv(t)
	e.svc.acctInterval = time.Millisecond
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- (&accountPoller{s: e.svc}).Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.account.mu.Lock()
		calls := e.account.calls
		e.account.mu.Unlock()
		if calls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poller did not poll")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The snapshot feeds the rate: 0.8 x 14.
	e.svc.refreshRate(e.ctx)
	if r := e.svc.bucket.Rate(); r < 11.19 || r > 11.21 {
		t.Fatalf("rate = %v", r)
	}
}

// providerHook runs fn before each send.
type providerHook struct {
	core.EmailProvider
	fn func()
}

func (p providerHook) Send(ctx context.Context, m core.OutgoingEmail) (string, error) {
	p.fn()
	return p.EmailProvider.Send(ctx, m)
}

// TestEraseDuringSend: an erase that lands while SES is called must not be
// undone by recording the send.
func TestEraseDuringSend(t *testing.T) {
	e := newEnv(t)
	bID, emailID := e.broadcast("sending")
	c := testutil.InsertContact(t, e.db, "erase-me@example.org", core.StatusActive)
	id := e.broadcastMessage(bID, emailID, c)
	e.svc.provider = providerHook{e.provider, func() {
		testutil.Exec(t, e.db, `UPDATE messages SET to_email = NULL, contact_id = NULL WHERE id = ?`, id)
	}}
	e.sendDue()
	var to sql.NullString
	if err := e.db.Read.QueryRow(`SELECT to_email FROM messages WHERE id = ?`, id).Scan(&to); err != nil || to.Valid {
		t.Fatalf("to_email = %v %v", to, err)
	}
	if e.row(id).Status != "sent" {
		t.Fatalf("row = %+v", e.row(id))
	}
}

// TestAddressChangeDuringRender: the message is rendered again for the new
// address instead of going to the old one.
func TestAddressChangeDuringRender(t *testing.T) {
	e := newEnv(t)
	bID, emailID := e.broadcast("sending")
	c := testutil.InsertContact(t, e.db, "old@example.org", core.StatusActive)
	id := e.broadcastMessage(bID, emailID, c)
	once := sync.Once{}
	e.svc.renderer = renderHook{e.renderer, func() {
		once.Do(func() { testutil.Exec(t, e.db, `UPDATE contacts SET email = 'new@example.org' WHERE id = ?`, c.ID) })
	}}
	e.sendDue()
	if r := e.row(id); r.Status != "queued" || r.Attempts != 0 || e.provider.Calls() != 0 {
		t.Fatalf("row = %+v", r)
	}
	e.clock.Advance(dbRetryDelay)
	e.sendDue()
	if len(e.provider.SentTo("new@example.org")) != 1 || e.provider.Count() != 1 {
		t.Fatalf("sent = %+v", e.provider.Sent())
	}
}

// TestTestMessageSenderOverride: a test message carrying sender overrides
// in its data (a broadcast's from_name and reply_to, B-7) is sent with
// them; a Reply-To off the sending domain fails it (D-7).
func TestTestMessageSenderOverride(t *testing.T) {
	e := newEnv(t)
	emailID := testutil.InsertEmail(t, e.db, "Test", "Hello")
	withOverride := e.enqueue(outbox.MessageSpec{Kind: core.KindTest, EmailID: emailID, ToEmail: "owner@example.org",
		Data: map[string]any{"test": true, core.DataFromName: "Special Edition", core.DataReplyTo: ""}})
	offDomain := e.enqueue(outbox.MessageSpec{Kind: core.KindTest, EmailID: emailID, ToEmail: "other@example.org",
		Data: map[string]any{"test": true, core.DataReplyTo: "someone@elsewhere.org"}})
	e.sendDue()

	sent := e.provider.SentTo("owner@example.org")
	if len(sent) != 1 {
		t.Fatalf("sent = %+v", e.provider.Sent())
	}
	h := parseMessage(t, sent[0].Raw).Header
	if h.Get("From") != `"Special Edition" <news@example.com>` || h.Get("Reply-To") != "" || sent[0].ReplyTo != "" {
		t.Fatalf("headers = %v", h)
	}
	if r := e.row(withOverride); r.Status != "sent" {
		t.Fatalf("override row = %+v", r)
	}
	if r := e.row(offDomain); r.Status != "failed" || len(e.provider.SentTo("other@example.org")) != 0 {
		t.Fatalf("off-domain row = %+v", r)
	}
}

// TestTestSendsFollowTheRecipientsStatus: a test message carries no
// contact (content.SendTestAs), yet it is skipped when a contact with its
// address unsubscribed, bounced, complained or was suppressed, and sent to
// active and pending contacts and to addresses no contact has.
func TestTestSendsFollowTheRecipientsStatus(t *testing.T) {
	e := newEnv(t)
	ids := map[core.ContactStatus]int64{}
	for _, st := range core.ContactStatuses {
		c := testutil.InsertContact(t, e.db, "t-"+string(st)+"@example.org", st)
		ids[st] = e.testMessage(c.Email, 0)
	}
	stranger := e.testMessage("stranger@example.org", 0)
	e.sendDue()
	for st, id := range ids {
		r := e.row(id)
		want := "skipped"
		if st == core.StatusActive || st == core.StatusPending {
			want = "sent"
		}
		if r.Status != want {
			t.Errorf("test to a %s contact: %+v, want %s", st, r, want)
		}
		if want == "skipped" && r.Error != "contact status is "+string(st) {
			t.Errorf("test to a %s contact: error %q", st, r.Error)
		}
	}
	if r := e.row(stranger); r.Status != "sent" {
		t.Fatalf("test to an unknown address: %+v", r)
	}
}

// TestRejectedKeyPausesInsteadOfFailing: when SES rejects the access key
// (a rotated key the process still uses), the message is re-queued, all
// sending pauses with one alert, and nothing is failed; a successful
// account poll with the new key lifts the pause and the queue goes out.
func TestRejectedKeyPausesInsteadOfFailing(t *testing.T) {
	e := newEnv(t)
	e.provider.FailNext(&core.ProviderError{Code: CodeCredentialsRejected, Message: "InvalidClientTokenId: The security token included in the request is invalid."})
	first := e.testMessage("owner@example.org", 0)
	second := e.testMessage("friend@example.org", 0)
	e.sendDue()
	for _, id := range []int64{first, second} {
		if r := e.row(id); r.Status != "queued" || r.Attempts != 0 {
			t.Fatalf("message %d = %+v, want queued", id, r)
		}
	}
	st := e.state()
	if !st.Paused || st.Source != PauseSES || !strings.Contains(st.Reason, "access key") {
		t.Fatalf("state = %+v", st)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE kind = 'alert' AND json_extract(data, '$.key') = ?`, AlertSESCredentials); n != 1 {
		t.Fatalf("credential alerts = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE status = 'failed'`); n != 0 {
		t.Fatalf("failed messages = %d", n)
	}
	e.account.set(true, 14)
	if _, err := e.svc.PollAccount(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.state().Paused {
		t.Fatalf("still paused: %+v", e.state())
	}
	e.sendDue()
	if e.row(first).Status != "sent" || e.row(second).Status != "sent" {
		t.Fatalf("after resume: %+v %+v", e.row(first), e.row(second))
	}
}
