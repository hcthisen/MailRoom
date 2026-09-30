package testutil

import (
	"context"
	"testing"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// fixtureTime is the timestamp written by fixtures.
func fixtureTime() string { return db.FormatTime(Epoch) }

// Exec runs a statement on the write pool and fails the test on error.
func Exec(t testing.TB, d *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.Write.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("testutil.Exec %q: %v", query, err)
	}
}

// Count returns the single integer produced by query (e.g. "SELECT count(*) ...").
func Count(t testing.TB, q db.Querier, query string, args ...any) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("testutil.Count %q: %v", query, err)
	}
	return n
}

func insertID(t testing.TB, d *db.DB, query string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := d.Write.QueryRowContext(context.Background(), query+" RETURNING id", args...).Scan(&id); err != nil {
		t.Fatalf("testutil insert %q: %v", query, err)
	}
	return id
}

// InsertContact inserts a contact directly (no hooks, no suppression check)
// with the given status and tags, and returns it as stored.
func InsertContact(t testing.TB, d *db.DB, email string, status core.ContactStatus, tags ...string) core.Contact {
	t.Helper()
	norm, err := core.NormalizeEmail(email)
	if err != nil {
		t.Fatalf("testutil.InsertContact: %v", err)
	}
	now := fixtureTime()
	id := insertID(t, d, `INSERT INTO contacts (email, status, status_changed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		norm, string(status), now, now, now)
	for _, tag := range tags {
		tagID := InsertTag(t, d, tag)
		Exec(t, d, `INSERT INTO contact_tags (contact_id, tag_id, source, added_at) VALUES (?, ?, 'fixture', ?) ON CONFLICT DO NOTHING`, id, tagID, now)
	}
	store := &contactstore.Store{DB: d, Clock: NewClock(Epoch)}
	c, err := store.Get(context.Background(), d.Write, id)
	if err != nil {
		t.Fatalf("testutil.InsertContact: %v", err)
	}
	return c
}

// InsertTag returns the id of the tag named name, creating it if needed.
func InsertTag(t testing.TB, d *db.DB, name string) int64 {
	t.Helper()
	norm, err := contactstore.NormalizeTag(name)
	if err != nil {
		t.Fatalf("testutil.InsertTag: %v", err)
	}
	id, _, err := contactstore.EnsureTag(context.Background(), d.Write, norm, fixtureTime())
	if err != nil {
		t.Fatalf("testutil.InsertTag: %v", err)
	}
	return id
}

// InsertSuppression adds an address to the suppressions table.
// reason is "erased" or "manual".
func InsertSuppression(t testing.TB, d *db.DB, email, reason string) {
	t.Helper()
	norm, err := core.NormalizeEmail(email)
	if err != nil {
		t.Fatalf("testutil.InsertSuppression: %v", err)
	}
	Exec(t, d, `INSERT INTO suppressions (email_hash, reason, created_at) VALUES (?, ?, ?)`,
		core.EmailHash(norm), reason, fixtureTime())
}

// InsertEmail inserts a markdown email and returns its id.
func InsertEmail(t testing.TB, d *db.DB, subject, body string) int64 {
	t.Helper()
	now := fixtureTime()
	return insertID(t, d, `INSERT INTO emails (name, subject, format, body, created_at, updated_at) VALUES (?, ?, 'markdown', ?, ?, ?)`,
		subject, subject, body, now, now)
}

// InsertBroadcast inserts a draft broadcast for emailID (audience: all).
func InsertBroadcast(t testing.TB, d *db.DB, name string, emailID int64) int64 {
	t.Helper()
	now := fixtureTime()
	return insertID(t, d, `INSERT INTO broadcasts (name, email_id, status, created_at, updated_at) VALUES (?, ?, 'draft', ?, ?)`,
		name, emailID, now, now)
}

// InsertSequence inserts an active sequence with reentry "never".
func InsertSequence(t testing.TB, d *db.DB, name string) int64 {
	t.Helper()
	now := fixtureTime()
	return insertID(t, d, `INSERT INTO sequences (name, created_at, updated_at) VALUES (?, ?, ?)`, name, now, now)
}

// InsertStep inserts a sequence step at position with a delay in seconds.
func InsertStep(t testing.TB, d *db.DB, sequenceID int64, position int, delaySeconds int64, emailID int64) int64 {
	t.Helper()
	now := fixtureTime()
	return insertID(t, d, `INSERT INTO sequence_steps (sequence_id, position, delay_seconds, email_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sequenceID, position, delaySeconds, emailID, now, now)
}

// InsertEnrollment inserts an active enrollment due at Epoch whose next
// step is nextStepID.
func InsertEnrollment(t testing.TB, d *db.DB, sequenceID, contactID, nextStepID int64) int64 {
	t.Helper()
	now := fixtureTime()
	return insertID(t, d, `INSERT INTO enrollments (sequence_id, contact_id, next_step_id, next_run_at, status, source, started_at, updated_at) VALUES (?, ?, ?, ?, 'active', 'fixture', ?, ?)`,
		sequenceID, contactID, db.NullID(nextStepID), now, now, now)
}
