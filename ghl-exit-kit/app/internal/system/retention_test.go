package system

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

func insertEvent(t *testing.T, f *fixture, at time.Time, detail string) int64 {
	t.Helper()
	var id int64
	err := f.d.Write.QueryRow(`INSERT INTO events (type, detail, created_at) VALUES ('delivery', ?, ?) RETURNING id`,
		detail, db.FormatTime(at)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertJob(t *testing.T, f *fixture, status string, finished *time.Time) int64 {
	t.Helper()
	var id int64
	err := f.d.Write.QueryRow(`INSERT INTO jobs (type, status, created_at, finished_at) VALUES ('backup', ?, ?, ?) RETURNING id`,
		status, db.FormatTime(f.clock.Now().Add(-90*day)), db.NullTime(finished)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertSession(t *testing.T, f *fixture, id string, expires time.Time) {
	t.Helper()
	now := db.FormatTime(f.clock.Now())
	testutil.Exec(t, f.d, `INSERT OR IGNORE INTO users (id, email, password_hash, created_at, updated_at) VALUES (1, 'u@example.com', 'x', ?, ?)`, now, now)
	testutil.Exec(t, f.d, `INSERT INTO sessions (id, user_id, csrf_token, created_at, last_seen_at, expires_at) VALUES (?, 1, 'c', ?, ?, ?)`,
		id, now, now, db.FormatTime(expires))
}

func TestRetention(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	oldEvent := insertEvent(t, f, now.Add(-91*day), `{"bounce":{"x":1}}`)
	edgeEvent := insertEvent(t, f, now.Add(-89*day), `{"keep":true}`)
	newEvent := insertEvent(t, f, now.Add(-time.Hour), `{"keep":true}`)

	oldDone := now.Add(-31 * day)
	recentDone := now.Add(-29 * day)
	jOldOK := insertJob(t, f, "succeeded", &oldDone)
	jOldFailed := insertJob(t, f, "failed", &oldDone)
	jRecent := insertJob(t, f, "succeeded", &recentDone)
	jQueued := insertJob(t, f, "queued", nil)
	jRunning := insertJob(t, f, "running", nil)

	insertSession(t, f, "expired", now.Add(-time.Minute))
	insertSession(t, f, "valid", now.Add(time.Hour))

	for _, at := range []time.Time{now.Add(-32 * day), now.Add(-30 * day)} {
		testutil.Exec(t, f.d, `INSERT INTO confirmation_emails (email_hash, form_id, kind, queued_at) VALUES ('h', 1, 'optin', ?)`, db.FormatTime(at))
	}

	res, err := f.service().Retention(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM confirmation_emails`); n != 1 {
		t.Fatalf("confirmation log rows = %d, want the recent one", n)
	}
	if res != (RetentionResult{EventDetailsCleared: 1, JobsDeleted: 2, SessionsDeleted: 1, ConfirmationsPurged: 1}) {
		t.Fatalf("result = %+v", res)
	}
	detail := func(id int64) string {
		var d string
		if err := f.d.Read.QueryRow(`SELECT detail FROM events WHERE id = ?`, id).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if detail(oldEvent) != "{}" || detail(edgeEvent) != `{"keep":true}` || detail(newEvent) != `{"keep":true}` {
		t.Fatalf("details = %s %s %s", detail(oldEvent), detail(edgeEvent), detail(newEvent))
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM events`); n != 3 {
		t.Fatalf("events = %d: rows must stay, only the payload is emptied", n)
	}
	for id, want := range map[int64]bool{jOldOK: false, jOldFailed: false, jRecent: true, jQueued: true, jRunning: true} {
		if got := testutil.Count(t, f.d.Read, `SELECT count(*) FROM jobs WHERE id = ?`, id) == 1; got != want {
			t.Errorf("job %d exists = %v, want %v", id, got, want)
		}
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM sessions WHERE id = 'valid'`); n != 1 {
		t.Error("a valid session was deleted")
	}

	// A second run changes nothing.
	res, err = f.service().Retention(f.ctx)
	if err != nil || res != (RetentionResult{}) {
		t.Fatalf("second run = %+v, %v", res, err)
	}
}

func TestRetentionBatches(t *testing.T) {
	f := newFixture(t)
	old := db.FormatTime(f.clock.Now().Add(-100 * day))
	n := 2*retentionBatch + 17
	err := f.d.WithTx(f.ctx, func(tx *sql.Tx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(`INSERT INTO events (type, detail, created_at) VALUES ('delivery', ?, ?)`, fmt.Sprintf(`{"i":%d}`, i), old); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.service().Retention(f.ctx)
	if err != nil || res.EventDetailsCleared != int64(n) {
		t.Fatalf("cleared %d, %v; want %d", res.EventDetailsCleared, err, n)
	}
	if left := testutil.Count(t, f.d.Read, `SELECT count(*) FROM events WHERE detail <> '{}'`); left != 0 {
		t.Fatalf("%d details left", left)
	}
}

// TestRetentionEventDetailWrites checks that the write transactions only
// touch bounded primary-key ranges: the table scan that finds old payloads
// runs once on the read pool, never inside a write transaction.
func TestRetentionEventDetailWrites(t *testing.T) {
	f := newFixture(t)
	plan := func(query string, args ...any) string {
		t.Helper()
		rows, err := f.d.Read.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			out = append(out, detail)
		}
		return strings.Join(out, "; ")
	}
	if p := plan(clearEventDetailRange, 0, 1000, "x"); !strings.Contains(p, "INTEGER PRIMARY KEY") || strings.Contains(p, "SCAN") {
		t.Fatalf("write query plan = %q, want a primary-key range search", p)
	}

	// 2017 old payloads in one contiguous id range: 3 write transactions.
	old := db.FormatTime(f.clock.Now().Add(-100 * day))
	n := 2*retentionBatch + 17
	err := f.d.WithTx(f.ctx, func(tx *sql.Tx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(`INSERT INTO events (type, detail, created_at) VALUES ('delivery', '{"x":1}', ?)`, old); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	commits := 0
	ctx := db.WithCommitHook(f.ctx, &db.CommitHook{After: func() { commits++ }})
	cleared, err := f.service().clearEventDetails(ctx, db.FormatTime(f.clock.Now().Add(-EventDetailRetention)))
	if err != nil || cleared != int64(n) || commits != 3 {
		t.Fatalf("cleared %d in %d transactions, %v; want %d in 3", cleared, commits, err, n)
	}
	// Nothing left to clear: no write transaction at all.
	commits = 0
	cleared, err = f.service().clearEventDetails(ctx, db.FormatTime(f.clock.Now().Add(-EventDetailRetention)))
	if err != nil || cleared != 0 || commits != 0 {
		t.Fatalf("second run cleared %d in %d transactions, %v", cleared, commits, err)
	}
}

// TestRetentionIDsNotOrderedByTime: an event inside the old id range whose
// created_at is recent keeps its payload.
func TestRetentionIDsNotOrderedByTime(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	first := insertEvent(t, f, now.Add(-120*day), `{"old":1}`)
	recent := insertEvent(t, f, now.Add(-time.Hour), `{"keep":true}`)
	last := insertEvent(t, f, now.Add(-100*day), `{"old":2}`)
	res, err := f.service().Retention(f.ctx)
	if err != nil || res.EventDetailsCleared != 2 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	for id, want := range map[int64]string{first: "{}", recent: `{"keep":true}`, last: "{}"} {
		var got string
		if err := f.d.Read.QueryRow(`SELECT detail FROM events WHERE id = ?`, id).Scan(&got); err != nil || got != want {
			t.Errorf("event %d detail = %s, %v; want %s", id, got, err, want)
		}
	}
}

func TestRetentionNeedsDB(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	s.db = nil
	if _, err := s.Retention(f.ctx); !errors.Is(err, errNoDB) {
		t.Fatalf("err = %v", err)
	}
}
