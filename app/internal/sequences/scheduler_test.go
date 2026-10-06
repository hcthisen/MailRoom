package sequences_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/sequences"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

func TestSchedulerExactlyOnceDelaysAndCompletion(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "Drip"}, 3600, 2*86400, 0)
	c := hs.contact("a@example.com")
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	start := hs.clock.Now()

	// Not due yet.
	if res := hs.run(); res != (sequences.SchedulerResult{}) {
		t.Fatalf("early run = %+v", res)
	}
	// Due an hour after enrolment; running it repeatedly queues it once.
	hs.clock.Advance(time.Hour + 5*time.Minute)
	sentAt := hs.clock.Now()
	if res := hs.run(); res.Queued != 1 {
		t.Fatalf("run = %+v", res)
	}
	for range 3 {
		if res := hs.run(); res != (sequences.SchedulerResult{}) {
			t.Fatalf("repeated run = %+v", res)
		}
	}
	if got := hs.messages(id); len(got) != 1 || got[0] != seq.Steps[0].ID {
		t.Fatalf("messages = %v", got)
	}
	// The next delay counts from the previous send, not from the enrolment.
	if got, want := hs.nextRun(id), sentAt.Add(48*time.Hour); !got.Equal(want) {
		t.Fatalf("next_run_at = %s, want %s (enrolled %s)", got, want, start)
	}
	var kind, to string
	var contactID, emailID int64
	if err := hs.d.Read.QueryRow(`SELECT kind, to_email, contact_id, email_id FROM messages WHERE enrollment_id = ?`, id).
		Scan(&kind, &to, &contactID, &emailID); err != nil {
		t.Fatal(err)
	}
	if kind != "sequence" || to != "a@example.com" || contactID != c.ID || emailID != seq.Steps[0].EmailID {
		t.Fatalf("message = %s %s %d %d", kind, to, contactID, emailID)
	}

	// Step 2 and the zero-delay step 3 go out in the same run, then it completes.
	hs.clock.Advance(48 * time.Hour)
	res := hs.run()
	if res.Queued != 1 || res.Completed != 1 {
		t.Fatalf("run = %+v", res)
	}
	if got := hs.messages(id); len(got) != 3 || got[1] != seq.Steps[1].ID || got[2] != seq.Steps[2].ID {
		t.Fatalf("messages = %v", got)
	}
	e := hs.enrollment(id)
	if e.Status != sequences.EnrollmentCompleted || e.NextRunAt.Valid || e.NextStepID.Valid {
		t.Fatalf("enrollment = %+v", e)
	}
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE id = ? AND finished_at IS NOT NULL`, id); n != 1 {
		t.Fatal("finished_at not set")
	}
	hs.clock.Advance(365 * 24 * time.Hour)
	if res := hs.run(); res != (sequences.SchedulerResult{}) {
		t.Fatalf("run after completion = %+v", res)
	}
}

func TestSchedulerStepAddedAtEndReachesActiveEnrollments(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "Drip"}, 0, 3600)
	c := hs.contact("a@example.com")
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	hs.run()
	if _, _, err := hs.svc.AddStep(hs.ctx, seq.ID, sequences.StepInput{
		Email: &sequences.StepEmail{Subject: "Bonus", Body: "Extra"}, DelaySeconds: 60,
	}, 0); err != nil {
		t.Fatal(err)
	}
	hs.clock.Advance(time.Hour)
	hs.run()
	hs.clock.Advance(time.Minute)
	hs.run()
	if got := hs.messages(id); len(got) != 3 {
		t.Fatalf("messages = %v", got)
	}
	if e := hs.enrollment(id); e.Status != sequences.EnrollmentCompleted {
		t.Fatalf("enrollment = %+v", e)
	}
}

func TestSchedulerRechecksExitConditions(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "Drip", ExitTags: []string{"customer"}, ExitAddTag: strp("drip-exited")}, 0, 0)
	a := hs.contact("a@example.com")
	b := hs.contact("b@example.com")
	ida := hs.enroll(seq.ID, a.ID, core.EnrollOptions{})
	idb := hs.enroll(seq.ID, b.ID, core.EnrollOptions{})
	// Changes that bypass the hooks (e.g. made by another process) are
	// caught before the next step.
	testutil.Exec(t, hs.d, `UPDATE contacts SET status = 'unsubscribed' WHERE id = ?`, a.ID)
	tag := testutil.InsertTag(t, hs.d, "customer")
	testutil.Exec(t, hs.d, `INSERT INTO contact_tags (contact_id, tag_id, source, added_at) VALUES (?, ?, 'x', ?)`, b.ID, tag, "2026-01-01T00:00:00.000000Z")

	res := hs.run()
	if res.Exited != 2 || res.Queued+res.Completed != 0 {
		t.Fatalf("run = %+v", res)
	}
	if e := hs.enrollment(ida); e.ExitReason != sequences.ExitNotActive {
		t.Fatalf("a = %+v", e)
	}
	if e := hs.enrollment(idb); e.ExitReason != sequences.ExitTag {
		t.Fatalf("b = %+v", e)
	}
	if !hs.hasTag(a.ID, "drip-exited") || !hs.hasTag(b.ID, "drip-exited") {
		t.Fatal("exit_add_tag missing")
	}
	if n := hs.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("%d messages queued", n)
	}
}

// setWindow configures the settings' time zone and default window.
func (hs *harness) setWindow(tz string, def *settings.SendWindow) {
	hs.t.Helper()
	if _, err := hs.settings.Update(hs.ctx, settings.Patch{Timezone: &tz, DefaultSendWindow: settings.OptionalSendWindow{Set: true, Value: def}}, 0); err != nil {
		hs.t.Fatal(err)
	}
}

func TestSendWindow(t *testing.T) {
	cph, err := time.LoadLocation("Europe/Copenhagen")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	every := []int{1, 2, 3, 4, 5, 6, 7}
	tests := []struct {
		name    string
		own     *settings.SendWindow // sequence window
		def     *settings.SendWindow // settings default
		due     time.Time            // when the step falls due (local)
		wantAt  time.Time            // when it is sent (local)
		wantUTC string               // wantAt in UTC, spelled out
	}{
		{
			name: "inside the window", own: &settings.SendWindow{Days: every, Start: "08:00", End: "20:00"},
			due: time.Date(2026, 3, 27, 12, 0, 0, 0, cph), wantAt: time.Date(2026, 3, 27, 12, 0, 0, 0, cph), wantUTC: "2026-03-27T11:00:00Z",
		},
		{
			name: "spring forward: next morning is CEST", own: &settings.SendWindow{Days: every, Start: "08:00", End: "20:00"},
			due: time.Date(2026, 3, 28, 20, 30, 0, 0, cph), wantAt: time.Date(2026, 3, 29, 8, 0, 0, 0, cph), wantUTC: "2026-03-29T06:00:00Z",
		},
		{
			name: "fall back: next morning is CET", own: &settings.SendWindow{Days: every, Start: "08:00", End: "20:00"},
			due: time.Date(2026, 10, 24, 23, 0, 0, 0, cph), wantAt: time.Date(2026, 10, 25, 8, 0, 0, 0, cph), wantUTC: "2026-10-25T07:00:00Z",
		},
		{
			name: "weekdays only: Saturday waits for Monday", own: &settings.SendWindow{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00"},
			due: time.Date(2026, 3, 28, 10, 0, 0, 0, cph), wantAt: time.Date(2026, 3, 30, 9, 0, 0, 0, cph), wantUTC: "2026-03-30T07:00:00Z",
		},
		{
			name: "settings default applies without an own window", def: &settings.SendWindow{Days: every, Start: "10:00", End: "11:00"},
			due: time.Date(2026, 3, 27, 6, 0, 0, 0, cph), wantAt: time.Date(2026, 3, 27, 10, 0, 0, 0, cph), wantUTC: "2026-03-27T09:00:00Z",
		},
		{
			name: "own window wins over the default", own: &settings.SendWindow{Days: every, Start: "05:00", End: "07:00"},
			def: &settings.SendWindow{Days: every, Start: "10:00", End: "11:00"},
			due: time.Date(2026, 3, 27, 6, 0, 0, 0, cph), wantAt: time.Date(2026, 3, 27, 6, 0, 0, 0, cph), wantUTC: "2026-03-27T05:00:00Z",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.setWindow("Europe/Copenhagen", tc.def)
			seq := hs.sequence(sequences.SequenceInput{Name: "W", SendWindow: tc.own}, 0, 3600)
			c := hs.contact("a@example.com")
			start := tc.due
			id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{StartAt: &start})
			hs.clock.Set(tc.due)
			res := hs.run()
			if tc.wantAt.Equal(tc.due) {
				if res.Queued != 1 {
					t.Fatalf("run = %+v, want queued", res)
				}
				return
			}
			if res.Deferred != 1 || res.Queued != 0 {
				t.Fatalf("run = %+v, want deferred", res)
			}
			if got := hs.nextRun(id); !got.Equal(tc.wantAt) || got.UTC().Format(time.RFC3339) != tc.wantUTC {
				t.Fatalf("next_run_at = %s, want %s (%s)", got.UTC(), tc.wantUTC, tc.wantAt)
			}
			hs.clock.Set(tc.wantAt.Add(-time.Second))
			if res := hs.run(); res.Queued != 0 {
				t.Fatalf("sent before the window opened: %+v", res)
			}
			hs.clock.Set(tc.wantAt)
			if res := hs.run(); res.Queued != 1 {
				t.Fatalf("run at opening = %+v", res)
			}
			// The next delay counts from the actual send.
			if got, want := hs.nextRun(id), tc.wantAt.Add(time.Hour); !got.Equal(want) {
				t.Fatalf("next_run_at = %s, want %s", got, want)
			}
		})
	}
}

func TestSchedulerBatches(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "Big"}, 0)
	const n = sequences.SchedulerBatch*2 + 17
	for i := range n {
		c := testutil.InsertContact(t, hs.d, "c"+strconv.Itoa(i)+"@example.com", core.StatusActive)
		testutil.InsertEnrollment(t, hs.d, seq.ID, c.ID, seq.Steps[0].ID)
	}
	res := hs.run()
	if res.Completed != n {
		t.Fatalf("run = %+v, want %d completed", res, n)
	}
	if got := hs.count(`SELECT count(*) FROM messages WHERE kind = 'sequence'`); got != n {
		t.Fatalf("messages = %d", got)
	}
}

func TestSchedulerWorker(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "W"}, 0)
	c := hs.contact("a@example.com")
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	w := hs.svc.SchedulerWorker()
	if w.Name() != "scheduler" {
		t.Fatalf("name = %s", w.Name())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for hs.enrollment(id).Status != sequences.EnrollmentCompleted {
		if time.Now().After(deadline) {
			t.Fatal("worker did not process the enrollment")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
}
