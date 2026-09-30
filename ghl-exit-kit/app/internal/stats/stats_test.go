package stats_test

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/stats"
	"mailroom/internal/testutil"
)

func TestDashboard(t *testing.T) {
	d := testutil.NewDB(t)
	// Epoch is Monday 2026-01-05 09:00 UTC; "now" is 2026-02-10 12:00.
	now := time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	s := stats.New(stats.Deps{DB: d, Clock: clock})
	ctx := context.Background()

	at := func(ago time.Duration) string { return db.FormatTime(now.Add(-ago)) }
	day := 24 * time.Hour

	contact := func(email string, status core.ContactStatus, created time.Duration) int64 {
		c := testutil.InsertContact(t, d, email, status)
		testutil.Exec(t, d, `UPDATE contacts SET created_at = ? WHERE id = ?`, at(created), c.ID)
		return c.ID
	}
	a := contact("a@example.org", core.StatusActive, 2*day)
	b := contact("b@example.org", core.StatusActive, 10*day)
	c := contact("c@example.org", core.StatusUnsubscribed, 40*day)
	testutil.Exec(t, d, `INSERT INTO contact_status_changes (contact_id, from_status, to_status, source, created_at)
		VALUES (?, 'active', 'unsubscribed', 'link', ?), (?, 'active', 'unsubscribed', 'link', ?)`, c, at(3*day), a, at(45*day))

	msg := func(kind string, contactID int64, sent time.Duration) int64 {
		var id int64
		err := d.Write.QueryRowContext(ctx, `INSERT INTO messages (kind, contact_id, to_email, status, next_attempt_at, queued_at, sent_at, updated_at)
			VALUES (?, NULLIF(?, 0), 'x@example.org', 'sent', ?, ?, ?, ?) RETURNING id`, kind, contactID, at(sent), at(sent), at(sent), at(sent)).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	m1 := msg("broadcast", a, 1*day)  // this month, 30 d
	m2 := msg("sequence", b, 5*day)   // this month, 30 d
	_ = msg("optin", c, 20*day)       // January: 30 d only
	m4 := msg("broadcast", c, 50*day) // outside both
	_ = msg("test", 0, 1*day)         // not counted
	_ = msg("alert", 0, 1*day)        // not counted
	event := func(m int64, typ, sub string, ago time.Duration) {
		testutil.Exec(t, d, `INSERT INTO events (message_id, type, subtype, created_at) VALUES (?, ?, ?, ?)`, m, typ, sub, at(ago))
	}
	event(m1, "bounce", "permanent", 1*day)
	event(m1, "bounce", "permanent", 1*day) // same message twice
	event(m2, "bounce", "transient", 4*day) // soft: not counted
	event(m2, "complaint", "abuse", 4*day)
	event(m4, "complaint", "abuse", 49*day) // outside the window

	got, err := s.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := stats.Dashboard{
		GeneratedAt: now, Timezone: "UTC",
		ContactsTotal: 3, ContactsActive: 2, NewContacts7d: 1, NewContacts30d: 2, Unsubscribes30d: 1,
		MonthStart:    time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		SentThisMonth: 2, Sent30d: 3, HardBounces30d: 1, Complaints30d: 1,
		BounceRate30d: 1.0 / 3, ComplaintRate30d: 1.0 / 3,
	}
	if got != want {
		t.Fatalf("dashboard =\n %+v\nwant\n %+v", got, want)
	}

	// The message numbers are cached for a minute...
	msg("broadcast", a, 0)
	clock.Advance(30 * time.Second)
	if again, _ := s.Dashboard(ctx); again.SentThisMonth != 2 || !again.GeneratedAt.Equal(now) {
		t.Fatalf("cached sent = %d (generated %s)", again.SentThisMonth, again.GeneratedAt)
	}
	clock.Advance(stats.CacheTTL)
	if again, _ := s.Dashboard(ctx); again.SentThisMonth != 3 {
		t.Fatalf("refreshed sent = %d", again.SentThisMonth)
	}
	// ...but a new, erased or changed contact shows at once (an import
	// followed by a check of contacts_total, AGENTS.md phase 7).
	contact("d@example.org", core.StatusActive, 0)
	if again, _ := s.Dashboard(ctx); again.ContactsTotal != 4 || again.ContactsActive != 3 {
		t.Fatalf("after a new contact: total %d, active %d", again.ContactsTotal, again.ContactsActive)
	}
	testutil.Exec(t, d, `DELETE FROM contacts WHERE id = ?`, b)
	if again, _ := s.Dashboard(ctx); again.ContactsTotal != 3 {
		t.Fatalf("after an erase: total %d", again.ContactsTotal)
	}
	testutil.Exec(t, d, `UPDATE contacts SET status = 'unsubscribed' WHERE id = ?`, a)
	testutil.Exec(t, d, `INSERT INTO contact_status_changes (contact_id, from_status, to_status, source, created_at)
		VALUES (?, 'active', 'unsubscribed', 'link', ?)`, a, at(0))
	if again, _ := s.Dashboard(ctx); again.ContactsActive != 1 || again.Unsubscribes30d != 2 {
		t.Fatalf("after an unsubscribe: active %d, unsubscribes %d", again.ContactsActive, again.Unsubscribes30d)
	}
}

func TestDashboardEmpty(t *testing.T) {
	d := testutil.NewDB(t)
	s := stats.New(stats.Deps{DB: d, Clock: testutil.NewClock(testutil.Epoch)})
	got, err := s.Dashboard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.ContactsTotal != 0 || got.BounceRate30d != 0 || got.SentThisMonth != 0 {
		t.Fatalf("empty dashboard = %+v", got)
	}
}
