package sequences_test

import (
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/sequences"
)

// enrollmentsOf returns the IDs of the contact's enrollments in seqID.
func (hs *harness) enrollmentsOf(seqID, contactID int64) []int64 {
	hs.t.Helper()
	rows, err := hs.d.Read.Query(`SELECT id FROM enrollments WHERE sequence_id = ? AND contact_id = ? ORDER BY id`, seqID, contactID)
	if err != nil {
		hs.t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			hs.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestTagTrigger(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		paused   bool
		status   core.ContactStatus
		enrolled bool
	}{
		{name: "api tag enrols", source: "api", status: core.StatusActive, enrolled: true},
		{name: "form tag enrols", source: "form:ab12", status: core.StatusActive, enrolled: true},
		{name: "import tag is ignored", source: "import", status: core.StatusActive},
		{name: "import with source is ignored", source: "import:ghl", status: core.StatusActive},
		{name: "paused sequence is not triggered", source: "api", paused: true, status: core.StatusActive},
		{name: "unsubscribed contact is not enrolled", source: "api", status: core.StatusUnsubscribed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t)
			in := sequences.SequenceInput{Name: "Welcome", TriggerTag: strp("joined")}
			if tc.paused {
				in.Status = sequences.StatusPaused
			}
			seq := hs.sequence(in, 0)
			c := hs.contact("a@example.com")
			if tc.status != core.StatusActive {
				hs.setStatus(c.ID, tc.status)
			}
			hs.addTags(c.ID, tc.source, "Joined")
			ids := hs.enrollmentsOf(seq.ID, c.ID)
			if tc.enrolled != (len(ids) == 1) {
				t.Fatalf("enrollments = %v, want enrolled=%v", ids, tc.enrolled)
			}
			if tc.enrolled {
				if e := hs.enrollment(ids[0]); e.Source != "tag:joined" || e.Status != sequences.EnrollmentActive {
					t.Fatalf("enrollment = %+v", e)
				}
			}
		})
	}
}

func TestTagTriggerOnCreate(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "Welcome", TriggerTag: strp("joined")}, 3600)
	c := hs.contact("a@example.com", "joined")
	if ids := hs.enrollmentsOf(seq.ID, c.ID); len(ids) != 1 {
		t.Fatalf("enrollments = %v", ids)
	}
}

func TestExitTagAndExitAddTag(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{
		Name: "Welcome", TriggerTag: strp("joined"), ExitTags: []string{"customer", "refunded"}, ExitAddTag: strp("welcome-exited"),
	}, 0, 86400)
	// Adding welcome-exited starts a follow-up sequence.
	follow := hs.sequence(sequences.SequenceInput{Name: "Follow-up", TriggerTag: strp("welcome-exited")}, 0)
	other := hs.sequence(sequences.SequenceInput{Name: "Other"}, 0)

	c := hs.contact("a@example.com", "joined")
	otherID := hs.enroll(other.ID, c.ID, core.EnrollOptions{})
	ids := hs.enrollmentsOf(seq.ID, c.ID)
	if len(ids) != 1 {
		t.Fatalf("enrollments = %v", ids)
	}
	hs.run() // step 1 goes out

	hs.addTags(c.ID, "api", "customer")
	if e := hs.enrollment(ids[0]); e.Status != sequences.EnrollmentExited || e.ExitReason != sequences.ExitTag || e.NextRunAt.Valid {
		t.Fatalf("enrollment = %+v", e)
	}
	if !hs.hasTag(c.ID, "welcome-exited") {
		t.Fatal("exit_add_tag was not added")
	}
	if got := hs.enrollmentsOf(follow.ID, c.ID); len(got) != 1 {
		t.Fatalf("follow-up enrollments = %v", got)
	}
	if e := hs.enrollment(otherID); e.Status != sequences.EnrollmentCompleted {
		t.Fatalf("an unrelated sequence was touched: %+v", e)
	}
	// An exit tag also blocks a new enrolment, and the second exit tag is a no-op.
	hs.addTags(c.ID, "api", "refunded")
	c2 := hs.contact("b@example.com", "customer")
	hs.addTags(c2.ID, "api", "joined")
	if got := hs.enrollmentsOf(seq.ID, c2.ID); len(got) != 0 {
		t.Fatalf("contact with an exit tag was enrolled: %v", got)
	}
}

func TestExitTagFromImportStillExits(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "Welcome", ExitTags: []string{"customer"}}, 3600)
	c := hs.contact("a@example.com")
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	hs.addTags(c.ID, "import", "customer")
	if e := hs.enrollment(id); e.Status != sequences.EnrollmentExited || e.ExitReason != sequences.ExitTag {
		t.Fatalf("enrollment = %+v", e)
	}
}

func TestStatusChangeExits(t *testing.T) {
	for _, to := range []core.ContactStatus{core.StatusUnsubscribed, core.StatusBounced, core.StatusComplained, core.StatusSuppressedManual} {
		t.Run(string(to), func(t *testing.T) {
			hs := newHarness(t)
			a := hs.sequence(sequences.SequenceInput{Name: "A", ExitAddTag: strp("left-a")}, 3600)
			b := hs.sequence(sequences.SequenceInput{Name: "B"}, 3600)
			c := hs.contact("a@example.com")
			ida := hs.enroll(a.ID, c.ID, core.EnrollOptions{})
			idb := hs.enroll(b.ID, c.ID, core.EnrollOptions{})
			hs.setStatus(c.ID, to)
			for _, id := range []int64{ida, idb} {
				if e := hs.enrollment(id); e.Status != sequences.EnrollmentExited || e.ExitReason != sequences.ExitNotActive {
					t.Fatalf("enrollment %d = %+v", id, e)
				}
			}
			if !hs.hasTag(c.ID, "left-a") {
				t.Fatal("exit_add_tag was not added")
			}
		})
	}
}

// TestStatusChangeExitReasonNotOvertaken checks that an exit_add_tag added
// while leaving one sequence can't relabel the contact's other
// enrollments: they all end with the status change's reason first.
func TestStatusChangeExitReasonNotOvertaken(t *testing.T) {
	hs := newHarness(t)
	a := hs.sequence(sequences.SequenceInput{Name: "A", ExitAddTag: strp("x")}, 3600)
	b := hs.sequence(sequences.SequenceInput{Name: "B", ExitTags: []string{"x"}, ExitAddTag: strp("y")}, 3600)
	c := hs.contact("a@example.com")
	ida := hs.enroll(a.ID, c.ID, core.EnrollOptions{})
	idb := hs.enroll(b.ID, c.ID, core.EnrollOptions{})
	hs.setStatus(c.ID, core.StatusUnsubscribed)
	for _, id := range []int64{ida, idb} {
		if e := hs.enrollment(id); e.Status != sequences.EnrollmentExited || e.ExitReason != sequences.ExitNotActive {
			t.Fatalf("enrollment %d = %+v, want exited with reason not_active", id, e)
		}
	}
	// Both sequences left on a condition exit, so both exit_add_tags are added.
	if !hs.hasTag(c.ID, "x") || !hs.hasTag(c.ID, "y") {
		t.Fatalf("exit_add_tags: x=%v y=%v", hs.hasTag(c.ID, "x"), hs.hasTag(c.ID, "y"))
	}
}

func TestReactivationDoesNotExit(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "A"}, 3600)
	c := hs.contact("a@example.com")
	hs.setStatus(c.ID, core.StatusUnsubscribed)
	hs.setStatus(c.ID, core.StatusActive)
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	if id == 0 {
		t.Fatal("reactivated contact was not enrolled")
	}
}
