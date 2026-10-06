package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestIDsNeverReused: contacts and messages IDs are carried in signed
// public tokens, and segment, email, broadcast and step IDs in stored
// references, so a deleted row's ID must never be handed out again
// (AUTOINCREMENT).
func TestIDsNeverReused(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	const now = "2026-01-01T00:00:00.000000Z"
	if _, err := d.Write.ExecContext(ctx, `INSERT INTO sequences (name, created_at, updated_at) VALUES ('seq', ?1, ?1)`, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ table, insert string }{
		{"contacts", `INSERT INTO contacts (email, status, status_changed_at, created_at, updated_at)
			VALUES ('a' || abs(random()) || '@example.com', 'active', ?1, ?1, ?1) RETURNING id`},
		{"messages", `INSERT INTO messages (kind, to_email, status, next_attempt_at, queued_at, updated_at)
			VALUES ('alert', 'owner@example.com', 'queued', ?1, ?1, ?1) RETURNING id`},
		// Referenced by ID from stored JSON and rules (audience.segment_id,
		// clicked.broadcast_id, message rows): migration 0002.
		{"segments", `INSERT INTO segments (name, filter, created_at, updated_at)
			VALUES ('s' || abs(random()), '{}', ?1, ?1) RETURNING id`},
		{"emails", `INSERT INTO emails (subject, format, body, created_at, updated_at)
			VALUES ('Hi', 'markdown', 'Hi', ?1, ?1) RETURNING id`},
		{"broadcasts", `INSERT INTO broadcasts (name, email_id, status, created_at, updated_at)
			VALUES ('b', (SELECT max(id) FROM emails), 'draft', ?1, ?1) RETURNING id`},
		{"sequence_steps", `INSERT INTO sequence_steps (sequence_id, position, delay_seconds, email_id, created_at, updated_at)
			VALUES ((SELECT max(id) FROM sequences), 1, 0, (SELECT max(id) FROM emails), ?1, ?1) RETURNING id`},
	} {
		var first, second int64
		if err := d.Write.QueryRowContext(ctx, tc.insert, now).Scan(&first); err != nil {
			t.Fatalf("%s: %v", tc.table, err)
		}
		if _, err := d.Write.ExecContext(ctx, `DELETE FROM `+tc.table+` WHERE id = ?`, first); err != nil {
			t.Fatal(err)
		}
		if err := d.Write.QueryRowContext(ctx, tc.insert, now).Scan(&second); err != nil {
			t.Fatal(err)
		}
		if second <= first {
			t.Errorf("%s: id %d reused after delete (got %d)", tc.table, first, second)
		}
	}
}

// TestMigration0002KeepsData: rebuilding segments, emails, sequence steps
// and broadcasts with AUTOINCREMENT keeps every row, its ID and the rows
// that reference it, and new IDs start after the highest existing one.
func TestMigration0002KeepsData(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "v1.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.MigrateFrom(ctx, all[:1]); err != nil {
		t.Fatal(err)
	}
	const now = "2026-01-01T00:00:00.000000Z"
	for _, q := range []string{
		`INSERT INTO segments (id, name, filter, created_at, updated_at) VALUES (7, 'seg', '{}', ?1, ?1)`,
		`INSERT INTO emails (id, subject, format, body, created_at, updated_at) VALUES (5, 'Hi', 'markdown', 'Hi', ?1, ?1)`,
		`INSERT INTO sequences (id, name, created_at, updated_at) VALUES (1, 'seq', ?1, ?1)`,
		`INSERT INTO sequence_steps (id, sequence_id, position, delay_seconds, email_id, created_at, updated_at) VALUES (3, 1, 1, 0, 5, ?1, ?1)`,
		`INSERT INTO broadcasts (id, name, email_id, audience, status, created_at, updated_at)
			VALUES (4, 'b', 5, '{"type":"segment","segment_id":7}', 'sent', ?1, ?1)`,
		`INSERT INTO contacts (id, email, status, status_changed_at, created_at, updated_at) VALUES (1, 'a@example.com', 'active', ?1, ?1, ?1)`,
		`INSERT INTO enrollments (id, sequence_id, contact_id, next_step_id, status, started_at, updated_at) VALUES (1, 1, 1, 3, 'active', ?1, ?1)`,
		`INSERT INTO messages (id, kind, broadcast_id, contact_id, email_id, status, next_attempt_at, queued_at, updated_at)
			VALUES (1, 'broadcast', 4, 1, 5, 'sent', ?1, ?1, ?1)`,
		`INSERT INTO messages (id, kind, enrollment_id, step_id, contact_id, email_id, status, next_attempt_at, queued_at, updated_at)
			VALUES (2, 'sequence', 1, 3, 1, 5, 'sent', ?1, ?1, ?1)`,
	} {
		if _, err := d.Write.ExecContext(ctx, q, now); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM segments WHERE id = 7`,
		`SELECT count(*) FROM emails WHERE id = 5`,
		`SELECT count(*) FROM sequence_steps WHERE id = 3 AND email_id = 5`,
		`SELECT count(*) FROM broadcasts WHERE id = 4 AND email_id = 5 AND json_extract(audience, '$.segment_id') = 7`,
		`SELECT count(*) FROM enrollments WHERE next_step_id = 3`,
		`SELECT count(*) FROM messages WHERE id = 1 AND broadcast_id = 4 AND email_id = 5`,
		`SELECT count(*) FROM messages WHERE id = 2 AND step_id = 3`,
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'broadcasts_status'`,
	} {
		var n int
		if err := d.Read.QueryRowContext(ctx, q).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s = %d, %v", q, n, err)
		}
	}
	var id int64
	if err := d.Write.QueryRowContext(ctx, `INSERT INTO segments (name, filter, created_at, updated_at) VALUES ('new', '{}', ?1, ?1) RETURNING id`, now).Scan(&id); err != nil || id != 8 {
		t.Fatalf("new segment id = %d, %v", id, err)
	}
	// Deleting a referenced email is still refused (the foreign keys work).
	if _, err := d.Write.ExecContext(ctx, `DELETE FROM emails WHERE id = 5`); err == nil {
		t.Fatal("deleting an email that steps and broadcasts use must fail")
	}
}
