package sequences_test

import (
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/sequences"
	"mailroom/internal/testutil"
)

func TestStats(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "S", ExitTags: []string{"customer"}}, 0, 86400)
	s1, s2 := seq.Steps[0].ID, seq.Steps[1].ID
	var contacts []core.Contact
	for _, e := range []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"} {
		c := hs.contact(e)
		contacts = append(contacts, c)
		hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	}
	hs.run() // step 1 queued for all four
	hs.addTags(contacts[3].ID, "api", "customer")

	msg := func(contactID int64) int64 {
		var id int64
		if err := hs.d.Read.QueryRow(`SELECT id FROM messages WHERE step_id = ? AND contact_id = ?`, s1, contactID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ts := "2026-01-05T09:30:00.000000Z"
	ma, mb, mc := msg(contacts[0].ID), msg(contacts[1].ID), msg(contacts[2].ID)
	testutil.Exec(t, hs.d, `UPDATE messages SET status = 'delivered', sent_at = ?, delivered_at = ? WHERE id IN (?, ?)`, ts, ts, ma, mb)
	testutil.Exec(t, hs.d, `UPDATE messages SET status = 'bounced', sent_at = ? WHERE id = ?`, ts, mc)
	testutil.Exec(t, hs.d, `INSERT INTO links (url, created_at) VALUES ('https://example.com/x', ?)`, ts)
	for _, m := range []struct{ msg, contact int64 }{{ma, contacts[0].ID}, {ma, contacts[0].ID}, {mb, contacts[1].ID}} {
		testutil.Exec(t, hs.d, `INSERT INTO clicks (link_id, message_id, contact_id, created_at) VALUES (1, ?, ?, ?)`, m.msg, m.contact, ts)
	}
	testutil.Exec(t, hs.d, `INSERT INTO events (message_id, type, subtype, created_at) VALUES (?, 'unsubscribe', 'one_click', ?)`, mb, ts)
	// Test sends of the step don't count.
	if _, err := hs.svc.TestStep(hs.ctx, seq.ID, s1, nil, 0); err != nil {
		t.Fatal(err)
	}

	st, err := hs.svc.Stats(hs.ctx, seq.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != 3 || st.Exited != 1 || st.Completed != 0 || len(st.Steps) != 2 {
		t.Fatalf("stats = %+v", st)
	}
	want1 := sequences.SequenceStepStats{StepID: s1, Position: 1, Queued: 1, Sent: 3, Delivered: 2, Bounced: 1, Clicked: 2, Unsubscribed: 1}
	want2 := sequences.SequenceStepStats{StepID: s2, Position: 2, Waiting: 3}
	if st.Steps[0] != want1 || st.Steps[1] != want2 {
		t.Fatalf("steps = %+v\nwant   %+v %+v", st.Steps, want1, want2)
	}
}
