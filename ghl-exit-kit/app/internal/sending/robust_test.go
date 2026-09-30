package sending

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// TestManualPauseTakesOverSESPause: a manual pause made while an SES
// pause is in place is not lifted when SES enables sending again.
func TestManualPauseTakesOverSESPause(t *testing.T) {
	e := newEnv(t)
	e.account.set(false, 14)
	if _, err := e.svc.PollAccount(e.ctx); err != nil {
		t.Fatal(err)
	}
	sesPause := e.state()
	if !sesPause.Paused || sesPause.Source != PauseSES {
		t.Fatalf("state = %+v", sesPause)
	}
	e.clock.Advance(time.Minute)

	// Dry run: shows the takeover, stores nothing.
	st, err := e.svc.Pause(e.ctx, "list review", true)
	if err != nil || st.Source != PauseManual || e.state().Source != PauseSES {
		t.Fatalf("dry run = %+v %v, stored %+v", st, err, e.state())
	}
	st, err = e.svc.Pause(e.ctx, "list review", false)
	if err != nil || !st.Paused || st.Source != PauseManual || st.Reason != "list review" || !st.Since.Equal(*sesPause.Since) {
		t.Fatalf("pause = %+v %v", st, err)
	}
	recs, err := audit.New(e.db).List(e.ctx, audit.Filter{Operation: "sending.pause"})
	if err != nil || len(recs) != 1 || !strings.Contains(fmt.Sprint(recs[0].Changes), PauseSES) {
		t.Fatalf("audit = %+v %v", recs, err)
	}

	e.account.set(true, 14)
	if _, err := e.svc.PollAccount(e.ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.state(); !st.Paused || st.Source != PauseManual {
		t.Fatalf("poller lifted the manual pause: %+v", st)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE kind = 'alert' AND json_extract(data, '$.key') = ?`, AlertSESResumed); n != 0 {
		t.Fatalf("resumed alerts = %d", n)
	}
	if _, err := e.svc.Resume(e.ctx, false); err != nil || e.state().Paused {
		t.Fatalf("resume: %v %+v", err, e.state())
	}
}

// blockingProvider blocks every send until release is closed.
type blockingProvider struct {
	core.EmailProvider
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Send(ctx context.Context, m core.OutgoingEmail) (string, error) {
	p.once.Do(func() { close(p.started) })
	<-p.release
	return p.EmailProvider.Send(ctx, m)
}

// TestCompleteBroadcastsWhileBusy: a finished broadcast becomes sent while
// the sender is still busy with another broadcast's backlog.
func TestCompleteBroadcastsWhileBusy(t *testing.T) {
	e := newEnv(t)
	e.svc.workers = 1
	e.svc.completeInterval = 10 * time.Millisecond
	bp := &blockingProvider{EmailProvider: e.provider, started: make(chan struct{}), release: make(chan struct{})}
	e.svc.provider = bp

	finished, _ := e.broadcast("sending")
	busy, bEmail := e.broadcast("sending")
	testutil.Exec(t, e.db, `UPDATE broadcasts SET snapshot_done_at = '2026-01-01T00:00:00Z' WHERE id IN (?, ?)`, finished, busy)
	for i := range 3 {
		e.broadcastMessage(busy, bEmail, testutil.InsertContact(t, e.db, fmt.Sprintf("b%d@example.org", i), core.StatusActive))
	}
	status := func(id int64) string {
		var st string
		if err := e.db.Read.QueryRow(`SELECT status FROM broadcasts WHERE id = ?`, id).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	stop := startWorker(t, e)
	<-bp.started // the dispatcher is now blocked handing out the backlog
	waitFor(t, func() bool { return status(finished) == "sent" })
	if status(busy) != "sending" {
		t.Fatal("busy broadcast completed early")
	}
	close(bp.release)
	waitFor(t, func() bool { return status(busy) == "sent" })
	stop()
}

// failSentWrites makes every write of status 'sent' fail until
// allowSentWrites is called.
func failSentWrites(t *testing.T, e *env) (allow func()) {
	t.Helper()
	testutil.Exec(t, e.db, `CREATE TABLE fail_ctl (on_ INTEGER NOT NULL)`)
	testutil.Exec(t, e.db, `INSERT INTO fail_ctl VALUES (1)`)
	testutil.Exec(t, e.db, `
		CREATE TRIGGER fail_sent BEFORE UPDATE OF status ON messages
		WHEN NEW.status = 'sent' AND (SELECT on_ FROM fail_ctl) = 1
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`)
	return func() { testutil.Exec(t, e.db, `UPDATE fail_ctl SET on_ = 0`) }
}

// hookHandler calls fn for every record, then logs it to the inner handler.
type hookHandler struct {
	slog.Handler
	fn func(slog.Record)
}

func (h hookHandler) Handle(ctx context.Context, r slog.Record) error {
	h.fn(r)
	return h.Handler.Handle(ctx, r)
}

func (h hookHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return hookHandler{h.Handler.WithAttrs(as), h.fn}
}

func (h hookHandler) WithGroup(name string) slog.Handler {
	return hookHandler{h.Handler.WithGroup(name), h.fn}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestOutcomeWriteRetried: a failed write of the outcome after a
// successful send is retried, so the row does not stay in "sending" and
// the SES MessageId is kept.
func TestOutcomeWriteRetried(t *testing.T) {
	e := newEnv(t)
	allow := failSentWrites(t, e)
	var retries int
	e.svc.outcomeBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	e.svc.log = slog.New(hookHandler{slog.NewTextHandler(&syncBuffer{}, nil), func(r slog.Record) {
		if strings.Contains(r.Message, "recording the outcome failed") {
			retries++
			allow() // the database is available again
		}
	}})
	id := e.testMessage("owner@example.org", 0)
	if n := e.sendDue(); n != 1 {
		t.Fatalf("claimed %d", n)
	}
	if r := e.row(id); r.Status != "sent" || r.SESID != "fake-1" || retries != 1 {
		t.Fatalf("row = %+v, retries %d", r, retries)
	}
}

// TestOutcomeWriteGivesUp: when the outcome cannot be written at all, the
// provider MessageId is logged at error level for repair.
func TestOutcomeWriteGivesUp(t *testing.T) {
	e := newEnv(t)
	failSentWrites(t, e)
	e.svc.outcomeBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	var buf syncBuffer
	e.svc.log = slog.New(slog.NewTextHandler(&buf, nil))
	id := e.testMessage("owner@example.org", 0)
	if _, err := e.svc.SendDue(e.ctx); err == nil {
		t.Fatal("want error")
	}
	if r := e.row(id); r.Status != "sending" {
		t.Fatalf("row = %+v", r)
	}
	out := buf.String()
	if strings.Count(out, "recording the outcome failed") != 2 || !strings.Contains(out, "level=ERROR") ||
		!strings.Contains(out, "ses_message_id=fake-1") || !strings.Contains(out, fmt.Sprintf("message_id=%d", id)) {
		t.Fatalf("log = %s", out)
	}
}

// TestTransactionalKindsFirst: opt-in confirmations, tests and alerts are
// sent before an earlier broadcast backlog.
func TestTransactionalKindsFirst(t *testing.T) {
	e := newEnv(t)
	bID, emailID := e.broadcast("sending")
	for i := range 5 {
		e.broadcastMessage(bID, emailID, testutil.InsertContact(t, e.db, fmt.Sprintf("b%d@example.org", i), core.StatusActive))
	}
	seqID := e.sequenceMessage(testutil.InsertContact(t, e.db, "seq@example.org", core.StatusActive), "active")
	e.clock.Advance(time.Minute)
	optin := e.optinMessage(testutil.InsertContact(t, e.db, "optin@example.org", core.StatusPending))
	test := e.testMessage("tester@example.org", 0)
	if err := e.svc.Alert(e.ctx, nil, "backup_failed", "Backup failed", "disk full"); err != nil {
		t.Fatal(err)
	}
	ids, err := e.svc.dueIDs(e.ctx, 100)
	if err != nil || len(ids) != 9 {
		t.Fatalf("due = %v %v", ids, err)
	}
	if ids[0] != optin || ids[1] != test || ids[8] != seqID {
		t.Fatalf("order = %v (optin %d, test %d, sequence %d)", ids, optin, test, seqID)
	}
	e.sendDue()
	var first []string
	for _, m := range e.provider.Sent()[:3] {
		first = append(first, m.To)
	}
	if got := strings.Join(first, ","); got != "optin@example.org,tester@example.org,owner@example.org" {
		t.Fatalf("first sent = %s", got)
	}
}

func TestBatchSize(t *testing.T) {
	tests := []struct {
		rate    float64
		workers int
		want    int
	}{
		{1, 4, 8},      // two per worker
		{14, 4, 28},    // two seconds of sending
		{11.2, 4, 23},  // rounded up
		{1000, 4, 100}, // capped
		{0, 1, 2},
	}
	for _, tt := range tests {
		if got := batchSize(tt.rate, tt.workers); got != tt.want {
			t.Errorf("batchSize(%v, %d) = %d, want %d", tt.rate, tt.workers, got, tt.want)
		}
	}
}
