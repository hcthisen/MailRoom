package sequences_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/sequences"
	"mailroom/internal/testutil"
)

// insertHistory inserts finished/active enrollments for (seq, contact) in
// order, one minute apart.
func (hs *harness) insertHistory(seqID, contactID int64, statuses ...string) {
	hs.t.Helper()
	for i, st := range statuses {
		ts := db.FormatTime(testutil.Epoch.Add(time.Duration(i-len(statuses)) * time.Hour))
		var runAt any
		if st == sequences.EnrollmentActive {
			runAt = ts
		}
		testutil.Exec(hs.t, hs.d, `INSERT INTO enrollments (sequence_id, contact_id, next_run_at, status, source, started_at, updated_at)
			VALUES (?, ?, ?, ?, 'fixture', ?, ?)`, seqID, contactID, runAt, st, ts, ts)
	}
}

func TestReentryMatrix(t *testing.T) {
	histories := map[string][]string{
		"none":                  nil,
		"active":                {"active"},
		"completed":             {"completed"},
		"exited":                {"exited"},
		"exited_then_completed": {"exited", "completed"},
		"completed_then_exited": {"completed", "exited"},
	}
	// want[policy][history] is the skip reason, "" = enrolled.
	want := map[string]map[string]string{
		sequences.ReentryNever: {
			"none": "", "active": sequences.SkipAlreadyActive, "completed": sequences.SkipReentry, "exited": sequences.SkipReentry,
			"exited_then_completed": sequences.SkipReentry, "completed_then_exited": sequences.SkipReentry,
		},
		sequences.ReentryAfterCompletion: {
			"none": "", "active": sequences.SkipAlreadyActive, "completed": "", "exited": sequences.SkipReentry,
			"exited_then_completed": "", "completed_then_exited": sequences.SkipReentry,
		},
		sequences.ReentryAlways: {
			"none": "", "active": sequences.SkipAlreadyActive, "completed": "", "exited": "",
			"exited_then_completed": "", "completed_then_exited": "",
		},
	}
	for policy, cases := range want {
		for history, reason := range cases {
			t.Run(policy+"/"+history, func(t *testing.T) {
				hs := newHarness(t)
				seq := hs.sequence(sequences.SequenceInput{Name: "S", Reentry: policy}, 0)
				c := hs.contact("a@example.com")
				hs.insertHistory(seq.ID, c.ID, histories[history]...)

				res, err := hs.svc.EnrollContacts(hs.ctx, seq.ID, sequences.EnrollRequest{ContactIDs: []int64{c.ID}}, false)
				if err != nil {
					t.Fatal(err)
				}
				if reason == "" {
					if res.Enrolled != 1 || len(res.EnrollmentIDs) != 1 || len(res.Skipped) != 0 {
						t.Fatalf("want enrolled, got %+v", res)
					}
					return
				}
				if res.Enrolled != 0 || res.Skipped[reason] != 1 {
					t.Fatalf("want skipped %s, got %+v", reason, res)
				}
				// The core.Enroller form reports a skip as (0, nil).
				if id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{Source: "api"}); id != 0 {
					t.Fatalf("Enroll = %d, want 0 (skipped)", id)
				}
			})
		}
	}
}

// TestSequenceOptOutBlocksReentry: a contact who chose "Only stop these
// emails" (exit reason unsubscribed_sequence) is never enrolled in that
// sequence again, not even with re-entry "always", by tag, API or form;
// other sequences are unaffected.
func TestSequenceOptOutBlocksReentry(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S", Reentry: sequences.ReentryAlways, TriggerTag: strp("joined")}, 0)
	other := hs.sequence(sequences.SequenceInput{Name: "Other", Reentry: sequences.ReentryAlways}, 0)
	c := hs.contact("a@example.com")
	ts := db.FormatTime(testutil.Epoch.Add(-time.Hour))
	testutil.Exec(t, hs.d, `INSERT INTO enrollments (sequence_id, contact_id, status, exit_reason, source, started_at, updated_at)
		VALUES (?, ?, 'exited', ?, 'fixture', ?, ?)`, seq.ID, c.ID, sequences.ExitUnsubscribed, ts, ts)

	res, err := hs.svc.EnrollContacts(hs.ctx, seq.ID, sequences.EnrollRequest{ContactIDs: []int64{c.ID}}, false)
	if err != nil || res.Enrolled != 0 || res.Skipped[sequences.SkipUnsubscribed] != 1 {
		t.Fatalf("API enrol = %+v, %v", res, err)
	}
	if id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{Source: "form:abc"}); id != 0 {
		t.Fatalf("Enroll = %d, want 0 (skipped)", id)
	}
	hs.addTags(c.ID, "api", "joined")
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE sequence_id = ? AND contact_id = ?`, seq.ID, c.ID); n != 1 {
		t.Fatalf("enrollments after the trigger tag = %d, want the old one only", n)
	}
	if id := hs.enroll(other.ID, c.ID, core.EnrollOptions{Source: "api"}); id == 0 {
		t.Fatal("the opt-out blocked another sequence")
	}
}

func TestEnrollSkipsAndErrors(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S", ExitTags: []string{"customer"}}, 60, 120)
	empty := hs.sequence(sequences.SequenceInput{Name: "Empty"})
	active := hs.contact("a@example.com")
	customer := hs.contact("c@example.com", "Customer")
	unsub := hs.contact("u@example.com")
	hs.setStatus(unsub.ID, core.StatusUnsubscribed)

	res, err := hs.svc.EnrollContacts(hs.ctx, seq.ID, sequences.EnrollRequest{
		ContactIDs: []int64{active.ID, customer.ID, unsub.ID, 9999, active.ID},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	wantSkips := map[string]int{sequences.SkipExitTag: 1, sequences.SkipNotActive: 1, sequences.SkipNotFound: 1}
	if res.Enrolled != 1 || fmt.Sprint(res.Skipped) != fmt.Sprint(wantSkips) {
		t.Fatalf("result = %+v", res)
	}

	res, err = hs.svc.EnrollContacts(hs.ctx, empty.ID, sequences.EnrollRequest{ContactIDs: []int64{active.ID}}, false)
	if err != nil || res.Skipped[sequences.SkipNoSteps] != 1 {
		t.Fatalf("empty sequence: %+v, %v", res, err)
	}

	errs := []struct {
		name  string
		seqID int64
		req   sequences.EnrollRequest
		field string
		isErr error
	}{
		{"no contacts", seq.ID, sequences.EnrollRequest{}, "contact_ids", core.ErrValidation},
		{"both", seq.ID, sequences.EnrollRequest{ContactIDs: []int64{1}, Emails: []string{"a@example.com"}}, "contact_ids", core.ErrValidation},
		{"bad email", seq.ID, sequences.EnrollRequest{Emails: []string{"a@example.com", "nope"}}, "emails[1]", core.ErrValidation},
		{"step beyond end", seq.ID, sequences.EnrollRequest{ContactIDs: []int64{active.ID}, AtStep: 3}, "at_step", core.ErrValidation},
		{"unknown sequence", 999, sequences.EnrollRequest{ContactIDs: []int64{active.ID}}, "!", core.ErrNotFound},
	}
	for _, tc := range errs {
		t.Run(tc.name, func(t *testing.T) {
			_, err := hs.svc.EnrollContacts(hs.ctx, tc.seqID, tc.req, false)
			if !isErr(err, tc.isErr) || fieldOf(err) != tc.field {
				t.Fatalf("err = %v (field %s), want %v field %s", err, fieldOf(err), tc.isErr, tc.field)
			}
		})
	}
}

func TestEnrollAtStepWithStartTime(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S"}, 0, 86400, 2*86400)
	c := hs.contact("a@example.com")
	start := testutil.Epoch.Add(-12 * time.Hour)
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{Source: "migration", AtStep: 2, StartAt: &start})
	if id == 0 {
		t.Fatal("not enrolled")
	}
	e := hs.enrollment(id)
	if e.NextStepID.Int64 != seq.Steps[1].ID || e.Source != "migration" {
		t.Fatalf("enrollment = %+v, want step %d", e, seq.Steps[1].ID)
	}
	if got, want := hs.nextRun(id), start.Add(24*time.Hour); !got.Equal(want) {
		t.Fatalf("next_run_at = %s, want %s", got, want)
	}

	// Due 12 h from now: nothing yet, then step 2, then step 3 two days later.
	if res := hs.run(); res.Queued != 0 {
		t.Fatalf("early run queued %+v", res)
	}
	hs.clock.Advance(12 * time.Hour)
	if res := hs.run(); res.Queued != 1 {
		t.Fatalf("run = %+v", res)
	}
	if got := hs.messages(id); len(got) != 1 || got[0] != seq.Steps[1].ID {
		t.Fatalf("messages = %v", got)
	}
	if got, want := hs.nextRun(id), hs.clock.Now().Add(48*time.Hour); !got.Equal(want) {
		t.Fatalf("next_run_at = %s, want %s", got, want)
	}

	// Default: now + first delay.
	c2 := hs.contact("b@example.com")
	id2 := hs.enroll(seq.ID, c2.ID, core.EnrollOptions{})
	if e := hs.enrollment(id2); e.NextStepID.Int64 != seq.Steps[0].ID || e.Source != "api" || !hs.nextRun(id2).Equal(hs.clock.Now()) {
		t.Fatalf("default enrollment = %+v", e)
	}
}

func TestPausedSequenceAcceptsEnrollmentsButWaits(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S", Status: sequences.StatusPaused}, 0)
	c := hs.contact("a@example.com")
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	if id == 0 {
		t.Fatal("paused sequence refused an enrollment")
	}
	if res := hs.run(); res.Queued+res.Completed != 0 {
		t.Fatalf("paused sequence sent: %+v", res)
	}
	if _, err := hs.svc.SetStatus(hs.ctx, seq.ID, sequences.StatusActive, 0); err != nil {
		t.Fatal(err)
	}
	if res := hs.run(); res.Completed != 1 {
		t.Fatalf("resumed run = %+v", res)
	}
	if e := hs.enrollment(id); e.Status != sequences.EnrollmentCompleted {
		t.Fatalf("enrollment = %+v", e)
	}
}

func isErr(err, target error) bool {
	if target == nil {
		return err == nil
	}
	return errors.Is(err, target)
}

// TestEnrollJobResumeIsIdempotent re-runs an interrupted sequences.enroll
// job: contacts the first run already enrolled count as enrolled and are
// not enrolled again, even with reentry always.
func TestEnrollJobResumeIsIdempotent(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S", Reentry: sequences.ReentryAlways}, 0)
	a := hs.contact("a@example.com")
	b := hs.contact("b@example.com")
	submitted := hs.clock.Now()
	// The first run enrolled a, and the enrolment already completed.
	after := db.FormatTime(submitted.Add(time.Second))
	testutil.Exec(t, hs.d, `INSERT INTO enrollments (sequence_id, contact_id, status, source, started_at, updated_at)
		VALUES (?, ?, 'completed', 'api', ?, ?)`, seq.ID, a.ID, after, after)
	params := fmt.Sprintf(`{"sequence_id":%d,"request":{"contact_ids":[%d,%d],"source":"api"},"submitted_at":%q}`,
		seq.ID, a.ID, b.ID, submitted.Format(time.RFC3339Nano))
	var jobID int64
	if err := hs.d.Write.QueryRow(`INSERT INTO jobs (type, status, params, created_at, started_at) VALUES (?, 'running', ?, ?, ?) RETURNING id`,
		sequences.JobEnroll, params, db.FormatTime(submitted), db.FormatTime(submitted)).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	hs.startJobs()
	j := hs.waitJob(jobID)
	var res sequences.EnrollResult
	if err := jsonUnmarshal(j.Result, &res); err != nil {
		t.Fatal(err)
	}
	if j.Status != "succeeded" || res.Enrolled != 2 || len(res.Skipped) != 0 {
		t.Fatalf("job = %+v, result %+v", j, res)
	}
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE contact_id = ?`, a.ID); n != 1 {
		t.Fatalf("a has %d enrollments", n)
	}
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE contact_id = ? AND status = 'active'`, b.ID); n != 1 {
		t.Fatalf("b has %d active enrollments", n)
	}
}
