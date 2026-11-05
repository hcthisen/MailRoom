package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigration0005KeepsMessages: rebuilding messages for the kind reply
// keeps every row, the events that reference them, and the AUTOINCREMENT
// counter, so an ID that was used and deleted before the migration is not
// handed out again (message IDs are in signed tokens).
func TestMigration0005KeepsMessages(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "v4.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.MigrateFrom(ctx, all[:4]); err != nil {
		t.Fatal(err)
	}
	const now = "2026-01-01T00:00:00.000000Z"
	for _, q := range []string{
		`INSERT INTO messages (id, kind, to_email, status, next_attempt_at, queued_at, updated_at) VALUES (10, 'alert', 'o@example.com', 'sent', ?1, ?1, ?1)`,
		`INSERT INTO messages (id, kind, to_email, status, next_attempt_at, queued_at, updated_at) VALUES (11, 'alert', 'o@example.com', 'sent', ?1, ?1, ?1)`,
		`DELETE FROM messages WHERE id = 11`,
		`INSERT INTO events (message_id, type, provider_event_id, created_at) VALUES (10, 'delivery', 'sns:x', ?1)`,
	} {
		if _, err := d.Write.ExecContext(ctx, q, now); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN events e ON e.message_id = m.id WHERE m.id = 10`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("message 10 with its event = %d, %v", n, err)
	}
	var id int64
	if err := d.Write.QueryRowContext(ctx, `INSERT INTO messages (kind, to_email, system_template, status, next_attempt_at, queued_at, updated_at)
		VALUES ('reply', 'a@example.com', 'reply', 'queued', ?1, ?1, ?1) RETURNING id`, now).Scan(&id); err != nil || id != 12 {
		t.Fatalf("new message id = %d, %v (want 12: 11 was used before)", id, err)
	}
	if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name IN
		('messages_queue', 'messages_broadcast', 'messages_contact', 'messages_step', 'messages_inbound', 'inbound_pending')`).Scan(&n); err != nil || n != 6 {
		t.Fatalf("indexes = %d, %v", n, err)
	}
	// Deleting a message still cascades to its events.
	if _, err := d.Write.ExecContext(ctx, `DELETE FROM messages WHERE id = 10`); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("events after delete = %d, %v", n, err)
	}
}
