package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0003DropsEventHeaders: stored SES events lose mail.headers
// and mail.commonHeaders (they carry the recipient's List-Unsubscribe URL);
// the rest of the event, and events without headers, stay as they were.
func TestMigration0003DropsEventHeaders(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "v2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.MigrateFrom(ctx, all[:2]); err != nil {
		t.Fatal(err)
	}
	const now = "2026-01-01T00:00:00.000000Z"
	withHeaders := `{"eventType":"Delivery","mail":{"messageId":"ses-1","destination":["a@example.com"],` +
		`"headersTruncated":false,"headers":[{"name":"List-Unsubscribe","value":"<https://mail.example.com/u/TOKEN>"}],` +
		`"commonHeaders":{"subject":"Hi","to":["a@example.com"]}},"delivery":{"recipients":["a@example.com"]}}`
	plain := `{"eventType":"Bounce","mail":{"messageId":"ses-2"}}`
	for i, detail := range []string{withHeaders, plain, `{}`} {
		if _, err := d.Write.ExecContext(ctx, `INSERT INTO events (id, type, provider_event_id, detail, created_at) VALUES (?, 'delivery', ?, ?, ?)`,
			i+1, "sns-"+string(rune('a'+i)), detail, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	details := map[int]string{}
	rows, err := d.Read.QueryContext(ctx, `SELECT id, detail FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int
		var s string
		if err := rows.Scan(&id, &s); err != nil {
			t.Fatal(err)
		}
		details[id] = s
	}
	rows.Close()
	if s := details[1]; strings.Contains(s, "/u/") || strings.Contains(s, "headers") || strings.Contains(s, "commonHeaders") ||
		!strings.Contains(s, `"messageId":"ses-1"`) || !strings.Contains(s, `"recipients"`) {
		t.Errorf("event 1 = %s", s)
	}
	if details[2] != plain || details[3] != `{}` {
		t.Errorf("events without headers changed: %q, %q", details[2], details[3])
	}
}
