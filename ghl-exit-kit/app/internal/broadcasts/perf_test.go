package broadcasts_test

import (
	"fmt"
	"testing"
	"time"

	"mailroom/internal/broadcasts"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

// TestSnapshotPerformance snapshots a 50k segment; PRD §12 targets a 50k
// broadcast snapshot in under 10 s.
func TestSnapshotPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: builds a 50k-contact database")
	}
	hs := newHarness(t)
	hs.contacts("c", 50_000, core.StatusActive, "vip")
	seg := hs.segment("VIPs", "vip")
	b := hs.create("Big", fmt.Sprintf(`{"type":"segment","segment_id":%d}`, seg))
	start := time.Now()
	res := hs.sendAndSnapshot(b.ID)
	took := time.Since(start)
	if !res.Done || res.RecipientsTotal != 50_000 {
		t.Fatalf("result = %+v", res)
	}
	t.Logf("50k snapshot took %v", took)
	if took > 10*time.Second {
		t.Fatalf("50k snapshot took %v, want < 10s", took)
	}
}

// TestStatsPerformance reads the stats of a 50k-message broadcast in a
// database with 50k contact status changes. A subquery correlated per
// message over contact_status_changes (which has no message_id index)
// made this quadratic: minutes instead of well under a second.
func TestStatsPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: builds a 50k-message database")
	}
	hs := newHarness(t)
	hs.contacts("c", 50_000, core.StatusActive, "")
	b := hs.create("Big", "")
	hs.setStatus(b.ID, broadcasts.StatusSending, "")
	now := db.FormatTime(hs.clock.Now())
	testutil.Exec(t, hs.d, `INSERT INTO messages (kind, broadcast_id, email_id, contact_id, to_email, status, next_attempt_at, queued_at, sent_at, updated_at)
		SELECT 'broadcast', ?, ?, id, email, 'sent', ?, ?, ?, ? FROM contacts`, b.ID, b.Email.ID, now, now, now, now)
	// Every message has a status change; every 100th is an unsubscribe.
	testutil.Exec(t, hs.d, `INSERT INTO contact_status_changes (contact_id, from_status, to_status, source, message_id, created_at)
		SELECT contact_id, 'active', CASE WHEN id % 100 = 0 THEN 'unsubscribed' ELSE 'bounced' END, 'link', id, ? FROM messages`, now)
	// Every 250th has an unsubscribe event; every 500th has both.
	testutil.Exec(t, hs.d, `INSERT INTO events (message_id, type, subtype, created_at)
		SELECT id, 'unsubscribe', 'one_click', ? FROM messages WHERE id % 250 = 0`, now)
	start := time.Now()
	st, err := hs.svc.Stats(hs.ctx, b.ID)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	// Unsubscribed: 500 status changes plus 200 events, 100 of them on
	// messages that also have a status change.
	if st.Sent != 50_000 || st.Unsubscribed != 600 {
		t.Fatalf("stats = %+v", st)
	}
	t.Logf("50k-message stats took %v", took)
	if took > 3*time.Second {
		t.Fatalf("50k-message stats took %v, want < 3s", took)
	}
}
