package broadcasts_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"mailroom/internal/broadcasts"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

func TestStarterStartsScheduled(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("c", 3, core.StatusActive, "")
	b := hs.create("B", "")
	later := hs.create("Later", "")
	at := hs.clock.Now().Add(time.Hour)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/schedule", b.ID), `{"at":"`+at.Format(time.RFC3339)+`"}`).wantStatus(t, 200)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/schedule", later.ID), `{"at":"`+at.Add(time.Hour).Format(time.RFC3339)+`"}`).wantStatus(t, 200)

	if err := hs.svc.Tick(hs.ctx); err != nil {
		t.Fatal(err)
	}
	if got := hs.get(b.ID); got.Status != broadcasts.StatusScheduled {
		t.Fatalf("started early: %s", got.Status)
	}
	hs.clock.Advance(time.Hour)
	started, err := hs.svc.StartDue(hs.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || started[0] != b.ID {
		t.Fatalf("started = %v", started)
	}
	got := hs.get(b.ID)
	if got.Status != broadcasts.StatusSending || got.StartedAt == nil || !got.StartedAt.Equal(hs.clock.Now()) || got.SnapshotJobID == nil {
		t.Fatalf("broadcast = %+v", got)
	}
	if hs.get(later.ID).Status != broadcasts.StatusScheduled {
		t.Fatal("later broadcast started")
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'broadcast.start' AND actor_type = 'system' AND target_id = ?`, fmt.Sprint(b.ID)); n != 1 {
		t.Fatalf("audit = %d", n)
	}
	if res := decodeSnapshot(t, hs.waitJob(*got.SnapshotJobID)); !res.Done || res.RecipientsTotal != 3 {
		t.Fatalf("snapshot = %+v", res)
	}
	// A second tick does not start it again.
	if started, err := hs.svc.StartDue(hs.ctx); err != nil || len(started) != 0 {
		t.Fatalf("second start = %v, %v", started, err)
	}
}

func TestStarterWorkerRuns(t *testing.T) {
	hs := newHarness(t)
	b := hs.create("B", "")
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/schedule", b.ID), `{"at":"`+hs.clock.Now().Add(time.Minute).Format(time.RFC3339)+`"}`).wantStatus(t, 200)
	hs.clock.Advance(time.Minute)
	w := hs.svc.StarterWorker()
	if w.Name() != "broadcast-starter" {
		t.Fatalf("name = %s", w.Name())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for hs.get(b.ID).Status != broadcasts.StatusSending {
		if time.Now().After(deadline) {
			t.Fatal("worker did not start the broadcast")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// sentMessages inserts n sent broadcast messages for new contacts and
// returns their IDs.
func (hs *harness) sentMessages(broadcastID, emailID int64, prefix string, n int) []int64 {
	hs.t.Helper()
	contacts := hs.contacts(prefix, n, core.StatusActive, "")
	now := db.FormatTime(hs.clock.Now())
	ids := make([]int64, n)
	for i, c := range contacts {
		var id int64
		err := hs.d.Write.QueryRow(`INSERT INTO messages (kind, broadcast_id, email_id, contact_id, to_email, status, next_attempt_at, queued_at, sent_at, updated_at)
			VALUES ('broadcast', ?, ?, ?, ?, 'sent', ?, ?, ?, ?) RETURNING id`,
			broadcastID, emailID, c, fmt.Sprintf("%s%d@example.net", prefix, i), now, now, now, now).Scan(&id)
		if err != nil {
			hs.t.Fatal(err)
		}
		ids[i] = id
	}
	return ids
}

func (hs *harness) event(messageID int64, typ, subtype string) {
	hs.t.Helper()
	testutil.Exec(hs.t, hs.d, `INSERT INTO events (message_id, type, subtype, created_at) VALUES (?, ?, ?, ?)`,
		messageID, typ, subtype, db.FormatTime(hs.clock.Now()))
}

func TestCircuitBreaker(t *testing.T) {
	tests := []struct {
		name                 string
		sent, hard, soft, cp int
		trip                 bool
	}{
		{"below min messages", 199, 50, 0, 5, false},
		{"bounce rate at threshold", 200, 10, 0, 0, false},
		{"bounce rate above", 200, 11, 0, 0, true},
		{"transient bounces don't count", 200, 0, 40, 0, false},
		{"complaint rate above", 1000, 0, 0, 2, true},
		{"complaint rate at threshold", 1000, 0, 0, 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t)
			b := hs.create("B", "")
			hs.setStatus(b.ID, broadcasts.StatusSending, "")
			ids := hs.sentMessages(b.ID, b.Email.ID, "m", tc.sent)
			for i := range tc.hard {
				hs.event(ids[i], "bounce", "permanent")
			}
			for i := range tc.soft {
				hs.event(ids[tc.hard+i], "bounce", "transient")
			}
			for i := range tc.cp {
				hs.event(ids[len(ids)-1-i], "complaint", "")
			}
			if err := hs.svc.Tick(hs.ctx); err != nil {
				t.Fatal(err)
			}
			got := hs.get(b.ID)
			alerts := hs.alerter.all()
			if !tc.trip {
				if got.Status != broadcasts.StatusSending || len(alerts) != 0 {
					t.Fatalf("tripped: %s, alerts %v", got.Status, alerts)
				}
				return
			}
			if got.Status != broadcasts.StatusPaused || got.PausedReason != broadcasts.PauseCircuitBreaker {
				t.Fatalf("broadcast = %s/%s", got.Status, got.PausedReason)
			}
			if len(alerts) != 1 || alerts[0].key != fmt.Sprintf("circuit_breaker:%d", b.ID) || !alerts[0].inTx || alerts[0].body == "" {
				t.Fatalf("alerts = %+v", alerts)
			}
		})
	}
}

func TestCircuitBreakerResumeNeedsConfirm(t *testing.T) {
	hs := newHarness(t)
	min := 100
	if _, err := hs.settings.Update(hs.ctx, settings.Patch{CircuitBreaker: &settings.CircuitBreakerPatch{MinMessages: &min}}, 0); err != nil {
		t.Fatal(err)
	}
	b := hs.create("B", "")
	hs.setStatus(b.ID, broadcasts.StatusSending, "")
	ids := hs.sentMessages(b.ID, b.Email.ID, "m", 100)
	for i := range 10 {
		hs.event(ids[i], "bounce", "permanent")
	}
	paused, err := hs.svc.CheckCircuitBreakers(hs.ctx)
	if err != nil || len(paused) != 1 {
		t.Fatalf("paused = %v, %v", paused, err)
	}
	path := fmt.Sprintf("/broadcasts/%d/resume", b.ID)
	hs.do("POST", path, "").wantError(t, http.StatusUnprocessableEntity, "validation", "confirm")
	hs.do("POST", path, `{"confirm":false}`).wantError(t, http.StatusUnprocessableEntity, "validation", "confirm")
	if got := hs.get(b.ID); got.Status != broadcasts.StatusPaused {
		t.Fatalf("status = %s", got.Status)
	}
	// The owner looks at the stats for a while before resuming. The
	// messages below are then stamped with the resume instant itself,
	// which the (inclusive) baseline must count.
	hs.clock.Advance(time.Minute)
	var got broadcasts.Broadcast
	hs.do("POST", path, `{"confirm":true}`).wantStatus(t, 200).decode(t, &got)
	if got.Status != broadcasts.StatusSending || got.PausedReason != "" {
		t.Fatalf("resumed = %+v", got)
	}
	// The breaker now judges only what is sent after the resume.
	if paused, err := hs.svc.CheckCircuitBreakers(hs.ctx); err != nil || len(paused) != 0 {
		t.Fatalf("re-tripped at once: %v, %v", paused, err)
	}
	hs.sentMessages(b.ID, b.Email.ID, "clean", 100)
	if paused, _ := hs.svc.CheckCircuitBreakers(hs.ctx); len(paused) != 0 {
		t.Fatalf("clean batch tripped: %v", paused)
	}
	// A late bounce of a message sent before the resume doesn't count.
	hs.event(ids[50], "bounce", "permanent")
	hs.clock.Advance(time.Second)
	more := hs.sentMessages(b.ID, b.Email.ID, "bad", 100)
	for i := range 10 {
		hs.event(more[i], "bounce", "permanent")
	}
	// 10 of 200 sent since the resume = 5 %: at the threshold, no trip.
	if paused, _ := hs.svc.CheckCircuitBreakers(hs.ctx); len(paused) != 0 {
		t.Fatalf("tripped at the threshold (old messages counted?): %v", paused)
	}
	hs.event(more[10], "bounce", "permanent")
	// 11 of 200 = 5.5 %.
	if paused, _ := hs.svc.CheckCircuitBreakers(hs.ctx); len(paused) != 1 {
		t.Fatalf("second trip = %v", paused)
	}
	if n := len(hs.alerter.all()); n != 2 {
		t.Fatalf("alerts = %d, want 2", n)
	}
	// A manual pause resumes without confirm.
	hs.setStatus(b.ID, broadcasts.StatusPaused, broadcasts.PauseManual)
	hs.do("POST", path, "").wantStatus(t, 200)
}

func TestRecoverSnapshots(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("c", 3, core.StatusActive, "")
	b := hs.create("B", "")
	j := hs.send(b.ID)
	testutil.Exec(t, hs.d, `UPDATE jobs SET status = 'failed', error = 'database is locked' WHERE id = ?`, j.ID)
	recovered, err := hs.svc.RecoverSnapshots(hs.ctx)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recovered = %v, %v", recovered, err)
	}
	got := hs.get(b.ID)
	if got.SnapshotJobID == nil || *got.SnapshotJobID == j.ID {
		t.Fatalf("snapshot job = %v", got.SnapshotJobID)
	}
	// A queued replacement is not replaced again.
	if again, _ := hs.svc.RecoverSnapshots(hs.ctx); len(again) != 0 {
		t.Fatalf("recovered twice: %v", again)
	}
	if res := decodeSnapshot(t, hs.waitJob(*got.SnapshotJobID)); !res.Done || res.RecipientsTotal != 3 {
		t.Fatalf("snapshot = %+v", res)
	}
}

func TestCancelSkipsQueued(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("c", 5, core.StatusActive, "")
	b := hs.create("B", "")
	hs.sendAndSnapshot(b.ID)
	testutil.Exec(t, hs.d, `UPDATE messages SET status = 'sent', sent_at = queued_at WHERE id IN (SELECT id FROM messages WHERE broadcast_id = ? ORDER BY id LIMIT 2)`, b.ID)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/cancel", b.ID), "").wantError(t, http.StatusConflict, "conflict", "")
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/pause", b.ID), "").wantStatus(t, 200)
	var got broadcasts.Broadcast
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/cancel", b.ID), "").wantStatus(t, 200).decode(t, &got)
	if got.Status != broadcasts.StatusCancelled || got.FinishedAt == nil {
		t.Fatalf("cancelled = %+v", got)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ? AND status = 'skipped' AND error = 'cancelled'`, b.ID); n != 3 {
		t.Fatalf("skipped = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ? AND status = 'sent'`, b.ID); n != 2 {
		t.Fatalf("sent = %d", n)
	}
}

func TestSendDryRun(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("vip", 4, core.StatusActive, "vip")
	hs.contacts("x", 3, core.StatusUnsubscribed, "vip")
	hs.contacts("plain", 2, core.StatusActive, "")
	testutil.InsertSuppression(t, hs.d, "vip1@example.net", "manual")
	seg := hs.segment("VIPs", "vip")
	b := hs.create("B", fmt.Sprintf(`{"type":"segment","segment_id":%d}`, seg))
	path := fmt.Sprintf("/broadcasts/%d/send?dry_run=true", b.ID)
	var res broadcasts.BroadcastSendResult
	hs.do("POST", path, "").wantStatus(t, http.StatusOK).decode(t, &res)
	// The broadcast is reported as it is: no phantom snapshot job, no
	// would-be status or version.
	if !res.DryRun || res.Recipients != 3 || res.Job != nil || res.Broadcast.Status != broadcasts.StatusDraft ||
		res.Broadcast.SnapshotJobID != nil || res.Broadcast.Version != b.Version || len(res.MissingTags) != 0 {
		t.Fatalf("dry run = %+v", res)
	}
	if got := hs.get(b.ID); got.Status != broadcasts.StatusDraft || got.SnapshotJobID != nil {
		t.Fatalf("dry run changed the broadcast: %+v", got)
	}
	if n := hs.count(`SELECT count(*) FROM jobs`) + hs.count(`SELECT count(*) FROM messages`) + hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'broadcast.send'`); n != 0 {
		t.Fatalf("dry run left %d rows", n)
	}
	// A tag the segment refers to disappears: the dry run says so.
	testutil.Exec(t, hs.d, `DELETE FROM tags WHERE name = 'vip'`)
	hs.do("POST", path, "").wantStatus(t, http.StatusOK).decode(t, &res)
	if res.Recipients != 0 || len(res.MissingTags) != 1 || res.MissingTags[0] != "vip" {
		t.Fatalf("dry run after tag delete = %+v", res)
	}
	// Everyone: 4 + 2 active minus one suppressed.
	all := hs.create("All", "")
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/send?dry_run=true", all.ID), "").wantStatus(t, http.StatusOK).decode(t, &res)
	if res.Recipients != 5 {
		t.Fatalf("all recipients = %d", res.Recipients)
	}
	// The real send reports the same count and a job.
	var real broadcasts.BroadcastSendResult
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/send", all.ID), "", "If-Match", `"v1"`).wantStatus(t, http.StatusAccepted).decode(t, &real)
	if real.DryRun || real.Recipients != 5 || real.Job == nil || real.Job.Type != broadcasts.JobSnapshot {
		t.Fatalf("send = %+v", real)
	}
	if got := hs.get(all.ID); got.SnapshotJobID == nil || *got.SnapshotJobID != real.Job.ID {
		t.Fatalf("snapshot job = %v", got.SnapshotJobID)
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'broadcast.send' AND actor_name = 'setup-agent'`); n != 1 {
		t.Fatalf("audit = %d", n)
	}
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/send", b.ID), "", "If-Match", `"v7"`).wantError(t, http.StatusPreconditionFailed, "precondition_failed", "")
}
