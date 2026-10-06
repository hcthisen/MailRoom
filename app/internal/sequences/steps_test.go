package sequences_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/sequences"
)

// stepIDs returns the sequence's step IDs in position order and checks
// the positions are 1..n.
func (hs *harness) stepIDs(seqID int64) []int64 {
	hs.t.Helper()
	seq, err := hs.svc.Get(hs.ctx, seqID)
	if err != nil {
		hs.t.Fatal(err)
	}
	var ids []int64
	for i, st := range seq.Steps {
		if st.Position != i+1 {
			hs.t.Fatalf("step %d at position %d, want %d", st.ID, st.Position, i+1)
		}
		ids = append(ids, st.ID)
	}
	return ids
}

func TestDeleteStep(t *testing.T) {
	tests := []struct {
		name       string
		deleteIdx  int // which step to delete (0-based)
		waitingIdx int // the step the enrollment waits for
		onActive   string
		wantErr    error
		wantStatus string
		wantReason string
		wantNext   int // index into the original steps of the new next step, -1 = none
	}{
		{name: "waiting enrollments need a decision", deleteIdx: 1, waitingIdx: 1, wantErr: core.ErrConflict},
		{name: "advance to the following step", deleteIdx: 1, waitingIdx: 1, onActive: "advance", wantStatus: "active", wantNext: 2},
		{name: "advance past the last step completes", deleteIdx: 2, waitingIdx: 2, onActive: "advance", wantStatus: "completed", wantNext: -1},
		{name: "exit", deleteIdx: 1, waitingIdx: 1, onActive: "exit", wantStatus: "exited", wantReason: sequences.ExitStepDeleted, wantNext: -1},
		{name: "no waiting enrollments needs no decision", deleteIdx: 0, waitingIdx: 1, wantStatus: "active", wantNext: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t)
			seq := hs.sequence(sequences.SequenceInput{Name: "S"}, 0, 3600, 7200)
			c := hs.contact("a@example.com")
			id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{AtStep: tc.waitingIdx + 1})
			before := hs.nextRun(id)
			res, err := hs.svc.DeleteStep(hs.ctx, seq.ID, seq.Steps[tc.deleteIdx].ID, tc.onActive, 0)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if got := hs.stepIDs(seq.ID); len(got) != 3 {
					t.Fatalf("steps = %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := slices.Delete(slices.Clone([]int64{seq.Steps[0].ID, seq.Steps[1].ID, seq.Steps[2].ID}), tc.deleteIdx, tc.deleteIdx+1)
			if got := hs.stepIDs(seq.ID); !slices.Equal(got, want) {
				t.Fatalf("steps = %v, want %v", got, want)
			}
			if res.Sequence.Version != seq.Version+1 {
				t.Fatalf("version = %d, want %d", res.Sequence.Version, seq.Version+1)
			}
			e := hs.enrollment(id)
			if e.Status != tc.wantStatus || e.ExitReason != tc.wantReason {
				t.Fatalf("enrollment = %+v", e)
			}
			if tc.wantNext < 0 {
				if e.NextStepID.Valid {
					t.Fatalf("next step = %d, want none", e.NextStepID.Int64)
				}
				return
			}
			if e.NextStepID.Int64 != seq.Steps[tc.wantNext].ID {
				t.Fatalf("next step = %d, want %d", e.NextStepID.Int64, seq.Steps[tc.wantNext].ID)
			}
			if got := hs.nextRun(id); !got.Equal(before) {
				t.Fatalf("next_run_at moved from %s to %s", before, got)
			}
		})
	}
}

func TestDeleteStepErrors(t *testing.T) {
	hs := newHarness(t)
	a := hs.sequence(sequences.SequenceInput{Name: "A"}, 0)
	b := hs.sequence(sequences.SequenceInput{Name: "B"}, 0)
	if _, err := hs.svc.DeleteStep(hs.ctx, a.ID, b.Steps[0].ID, "", 0); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("step of another sequence: %v", err)
	}
	if _, err := hs.svc.DeleteStep(hs.ctx, a.ID, a.Steps[0].ID, "sideways", 0); fieldOf(err) != "query.on_active" {
		t.Fatalf("bad on_active: %v", err)
	}
	if _, err := hs.svc.DeleteStep(hs.ctx, a.ID, a.Steps[0].ID, "", a.Version+5); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale If-Match: %v", err)
	}
}

func TestReorderSteps(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S"}, 0, 0, 0)
	s1, s2, s3 := seq.Steps[0].ID, seq.Steps[1].ID, seq.Steps[2].ID
	c := hs.contact("a@example.com")
	// The enrollment waits for step 2.
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{AtStep: 2})

	got, err := hs.svc.ReorderSteps(hs.ctx, seq.ID, []int64{s3, s1, s2}, "", seq.Version)
	if err != nil {
		t.Fatal(err)
	}
	if ids := hs.stepIDs(seq.ID); !slices.Equal(ids, []int64{s3, s1, s2}) {
		t.Fatalf("order = %v", ids)
	}
	if got.Sequence.Version != seq.Version+1 || got.Completed != 0 {
		t.Fatalf("result = %+v", got)
	}
	// Kept by step ID: s2 is now last, so the enrollment gets s2 and completes.
	hs.run()
	if msgs := hs.messages(id); !slices.Equal(msgs, []int64{s2}) {
		t.Fatalf("messages = %v", msgs)
	}
	if e := hs.enrollment(id); e.Status != sequences.EnrollmentCompleted {
		t.Fatalf("enrollment = %+v", e)
	}

	errs := []struct {
		name  string
		ids   []int64
		field string
	}{
		{"missing a step", []int64{s1, s2}, "step_ids"},
		{"duplicate", []int64{s1, s1, s2}, "step_ids[1]"},
		{"foreign step", []int64{s1, s2, 9999}, "step_ids[2]"},
	}
	for _, tc := range errs {
		if _, err := hs.svc.ReorderSteps(hs.ctx, seq.ID, tc.ids, "", 0); fieldOf(err) != tc.field {
			t.Errorf("%s: err = %v, want field %s", tc.name, err, tc.field)
		}
	}
	if _, err := hs.svc.ReorderSteps(hs.ctx, seq.ID, []int64{s1, s2, s3}, "sideways", 0); fieldOf(err) != "query.on_active" {
		t.Fatalf("bad on_active: %v", err)
	}
	if _, err := hs.svc.ReorderSteps(hs.ctx, seq.ID, []int64{s1, s2, s3}, "", seq.Version); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale If-Match: %v", err)
	}
}

// TestReorderOnActiveEnd checks S-7's "move them to the end": active
// enrollments complete, their queued message stays deliverable, and
// finished enrollments and other sequences are untouched.
func TestReorderOnActiveEnd(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S", Reentry: sequences.ReentryAlways}, 0, 3600, 3600)
	other := hs.sequence(sequences.SequenceInput{Name: "Other"}, 3600)
	s1, s2, s3 := seq.Steps[0].ID, seq.Steps[1].ID, seq.Steps[2].ID
	a, b := hs.contact("a@example.com"), hs.contact("b@example.com")
	active := hs.enroll(seq.ID, a.ID, core.EnrollOptions{})
	hs.run() // s1 queued for a, waiting for s2
	exited := hs.enroll(seq.ID, b.ID, core.EnrollOptions{})
	if _, err := hs.svc.Unenroll(hs.ctx, seq.ID, exited); err != nil {
		t.Fatal(err)
	}
	elsewhere := hs.enroll(other.ID, a.ID, core.EnrollOptions{})

	// An unchanged order changes no enrollment, even with end.
	res, err := hs.svc.ReorderSteps(hs.ctx, seq.ID, []int64{s1, s2, s3}, sequences.OnActiveEnd, 0)
	if err != nil || res.Completed != 0 || hs.enrollment(active).Status != sequences.EnrollmentActive {
		t.Fatalf("unchanged order: %+v, %v, %+v", res, err, hs.enrollment(active))
	}
	res, err = hs.svc.ReorderSteps(hs.ctx, seq.ID, []int64{s3, s2, s1}, sequences.OnActiveEnd, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 1 {
		t.Fatalf("completed = %d, want 1", res.Completed)
	}
	if e := hs.enrollment(active); e.Status != sequences.EnrollmentCompleted || e.NextStepID.Valid || e.NextRunAt.Valid {
		t.Fatalf("active enrollment = %+v", e)
	}
	if e := hs.enrollment(exited); e.Status != sequences.EnrollmentExited || e.ExitReason != sequences.ExitManual {
		t.Fatalf("exited enrollment = %+v", e)
	}
	if e := hs.enrollment(elsewhere); e.Status != sequences.EnrollmentActive {
		t.Fatalf("other sequence's enrollment = %+v", e)
	}
	// Completed (not exited), so the sender still delivers the queued s1.
	if n := hs.count(`SELECT count(*) FROM messages WHERE enrollment_id = ? AND status = 'queued'`, active); n != 1 {
		t.Fatalf("queued messages = %d, want 1", n)
	}
	hs.clock.Advance(3 * time.Hour)
	hs.run()
	if msgs := hs.messages(active); !slices.Equal(msgs, []int64{s1}) {
		t.Fatalf("messages after end = %v", msgs)
	}
}

// TestReorderSkipsReceivedSteps checks that after a reorder an enrollment
// never waits the delay of a step it already received: it moves straight
// on to the next step it hasn't had, with that step's delay.
func TestReorderSkipsReceivedSteps(t *testing.T) {
	const (
		A, B, C = 0, 1, 2 // step indexes
		done    = -1      // the enrollment completes
	)
	tests := []struct {
		name      string
		order     []int
		afterB    int   // the step waited for after B is sent, or done
		wantSteps []int // steps sent overall, in order
	}{
		{name: "received step moved to the end", order: []int{B, C, A}, afterB: C, wantSteps: []int{A, B, C}},
		{name: "received step moved between unsent ones", order: []int{B, A, C}, afterB: C, wantSteps: []int{A, B, C}},
		// Kept by ID, B is at position 2: C (now first) is skipped, A is received.
		{name: "only received steps follow", order: []int{C, B, A}, afterB: done, wantSteps: []int{A, B}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t)
			// A has a long delay: waiting it out for nothing is the bug.
			seq := hs.sequence(sequences.SequenceInput{Name: "S"}, 5*3600, 3600, 3600)
			ids := []int64{seq.Steps[0].ID, seq.Steps[1].ID, seq.Steps[2].ID}
			pick := func(idx []int) []int64 {
				out := make([]int64, len(idx))
				for i, x := range idx {
					out[i] = ids[x]
				}
				return out
			}
			ct := hs.contact("a@example.com")
			start := hs.clock.Now().Add(-5 * time.Hour)
			id := hs.enroll(seq.ID, ct.ID, core.EnrollOptions{StartAt: &start})
			hs.run() // A sent, waiting for B
			if _, err := hs.svc.ReorderSteps(hs.ctx, seq.ID, pick(tc.order), "", 0); err != nil {
				t.Fatal(err)
			}
			hs.clock.Advance(time.Hour)
			hs.run() // B sent (kept by ID)
			e := hs.enrollment(id)
			if tc.afterB == done {
				if e.Status != sequences.EnrollmentCompleted {
					t.Fatalf("after B: enrollment = %+v, want completed", e)
				}
			} else {
				if e.NextStepID.Int64 != ids[tc.afterB] {
					t.Fatalf("after B: next step = %d, want %d", e.NextStepID.Int64, ids[tc.afterB])
				}
				if got, want := hs.nextRun(id), hs.clock.Now().Add(time.Hour); !got.Equal(want) {
					t.Fatalf("after B: next run = %s, want %s (the next unsent step's delay)", got, want)
				}
				hs.clock.Advance(time.Hour)
				hs.run()
				if e := hs.enrollment(id); e.Status != sequences.EnrollmentCompleted {
					t.Fatalf("enrollment = %+v, want completed without an empty wait", e)
				}
			}
			if msgs := hs.messages(id); !slices.Equal(msgs, pick(tc.wantSteps)) {
				t.Fatalf("messages = %v, want %v", msgs, pick(tc.wantSteps))
			}
		})
	}
}

func TestAddStepAtPosition(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S"}, 0, 60)
	emailID := seq.Steps[0].EmailID
	got, st, err := hs.svc.AddStep(hs.ctx, seq.ID, sequences.StepInput{EmailID: emailID, DelaySeconds: 30, Position: 1}, seq.Version)
	if err != nil {
		t.Fatal(err)
	}
	if st.Position != 1 || st.EmailID != emailID || st.Delay != "30s" {
		t.Fatalf("step = %+v", st)
	}
	if ids := hs.stepIDs(seq.ID); !slices.Equal(ids, []int64{st.ID, seq.Steps[0].ID, seq.Steps[1].ID}) || got.Version != seq.Version+1 {
		t.Fatalf("steps = %v, version %d", ids, got.Version)
	}

	errs := []struct {
		name  string
		in    sequences.StepInput
		field string
	}{
		{"neither email", sequences.StepInput{}, "email_id"},
		{"both", sequences.StepInput{EmailID: emailID, Email: &sequences.StepEmail{Subject: "x", Body: "y"}}, "email_id"},
		{"unknown email", sequences.StepInput{EmailID: 999}, "email_id"},
		{"bad position", sequences.StepInput{EmailID: emailID, Position: 9}, "position"},
		{"negative delay", sequences.StepInput{EmailID: emailID, DelaySeconds: -1}, "delay_seconds"},
		{"invalid content", sequences.StepInput{Email: &sequences.StepEmail{Subject: "", Body: "y"}}, "email.subject"},
		{"bad merge field", sequences.StepInput{Email: &sequences.StepEmail{Subject: "Hi {{nope}}", Body: "y"}}, "email.subject"},
	}
	for _, tc := range errs {
		if _, _, err := hs.svc.AddStep(hs.ctx, seq.ID, tc.in, 0); fieldOf(err) != tc.field {
			t.Errorf("%s: err = %v, want field %s", tc.name, err, tc.field)
		}
	}
}

func TestUpdateStep(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S"}, 0, 86400)
	c := hs.contact("a@example.com")
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	hs.run() // step 1 sent; step 2 due in a day
	due := hs.nextRun(id)
	step2 := seq.Steps[1]

	// A shorter delay moves the waiting enrollment by the difference.
	d := int64(3600)
	got, st, err := hs.svc.UpdateStep(hs.ctx, seq.ID, step2.ID, sequences.StepPatch{DelaySeconds: &d}, seq.Version)
	if err != nil {
		t.Fatal(err)
	}
	if st.DelaySeconds != 3600 || got.Version != seq.Version+1 {
		t.Fatalf("step = %+v, version %d", st, got.Version)
	}
	if next := hs.nextRun(id); !next.Equal(due.Add(-23 * time.Hour)) {
		t.Fatalf("next_run_at = %s, want %s", next, due.Add(-23*time.Hour))
	}

	// Content goes through the content service.
	subject, body := "New subject", "New body"
	_, st, err = hs.svc.UpdateStep(hs.ctx, seq.ID, step2.ID, sequences.StepPatch{Subject: &subject, Body: &body}, 0)
	if err != nil {
		t.Fatal(err)
	}
	e, err := hs.content.GetEmail(hs.ctx, nil, step2.EmailID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Subject != subject || e.Subject != subject || e.Body != body || e.Version != 2 {
		t.Fatalf("step %+v, email %+v", st, e)
	}

	// A no-op keeps the version.
	cur, _ := hs.svc.Get(hs.ctx, seq.ID)
	again, _, err := hs.svc.UpdateStep(hs.ctx, seq.ID, step2.ID, sequences.StepPatch{Subject: &subject, DelaySeconds: &d}, cur.Version)
	if err != nil || again.Version != cur.Version {
		t.Fatalf("no-op: version %d -> %d, %v", cur.Version, again.Version, err)
	}

	// Switching the email.
	other := seq.Steps[0].EmailID
	if _, st, err = hs.svc.UpdateStep(hs.ctx, seq.ID, step2.ID, sequences.StepPatch{EmailID: &other}, 0); err != nil || st.EmailID != other {
		t.Fatalf("switch email: %+v, %v", st, err)
	}

	errs := []struct {
		name  string
		p     sequences.StepPatch
		field string
	}{
		{"both email and content", sequences.StepPatch{EmailID: &other, Subject: &subject}, "email_id"},
		{"unknown email", sequences.StepPatch{EmailID: ptr(int64(999))}, "email_id"},
		{"bad delay", sequences.StepPatch{DelaySeconds: ptr(int64(-5))}, "delay_seconds"},
		{"empty subject", sequences.StepPatch{Subject: strp(" ")}, "subject"},
	}
	for _, tc := range errs {
		if _, _, err := hs.svc.UpdateStep(hs.ctx, seq.ID, step2.ID, tc.p, 0); fieldOf(err) != tc.field {
			t.Errorf("%s: err = %v, want field %s", tc.name, err, tc.field)
		}
	}
}

func ptr[T any](v T) *T { return &v }
