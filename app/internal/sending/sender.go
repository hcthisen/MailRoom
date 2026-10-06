package sending

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"mailroom/internal/db"
)

// InterruptedError is the error recorded on messages found in "sending" at
// startup.
const InterruptedError = "interrupted during send; delivery unknown"

// dueWhere selects queued messages that are due, excluding broadcast
// messages whose broadcast is not sending (paused, cancelled, ...).
const dueWhere = `m.status = 'queued' AND m.next_attempt_at <= ?
	AND (m.broadcast_id IS NULL OR EXISTS (
		SELECT 1 FROM broadcasts b WHERE b.id = m.broadcast_id AND b.status = 'sending'))`

// RecoverInterrupted fails every message left in "sending" by a crash or
// a hard stop, with error InterruptedError and a send_error event. It
// returns how many there were. The sender calls it at start, before it
// claims anything, so every such row belongs to a previous process.
//
// This makes delivery at most once: such a message may or may not have
// reached SES (the process can die after SES accepted it but before the
// MessageId was stored). Sending it again could email the contact twice,
// which for marketing mail is worse than one lost message, and the lost
// message is visible as failed with this error.
func (s *Service) RecoverInterrupted(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		now := db.FormatTime(s.clock.Now())
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO events (message_id, type, subtype, detail, created_at)
			SELECT id, 'send_error', 'interrupted', json_object('message', ?), ? FROM messages WHERE status = 'sending'`,
			InterruptedError, now); err != nil {
			return fmt.Errorf("sending: recover: events: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE messages SET status = 'failed', error = ?, updated_at = ? WHERE status = 'sending'`,
			InterruptedError, now)
		if err != nil {
			return fmt.Errorf("sending: recover: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Warn("sending: messages interrupted during send marked failed", "count", n)
	}
	return n, nil
}

// dueIDs returns up to limit due message IDs in send order, or none while
// sending is globally paused. Transactional kinds (opt-in confirmations,
// alerts, tests) go first, so they never wait behind a large broadcast or
// sequence backlog; within each group the order is next_attempt_at, id.
func (s *Service) dueIDs(ctx context.Context, limit int) ([]int64, error) {
	st, err := s.GetState(ctx, nil)
	if err != nil || st.Paused {
		return nil, err
	}
	rows, err := s.db.Read.QueryContext(ctx, `
		SELECT m.id FROM messages m WHERE `+dueWhere+`
		ORDER BY CASE WHEN m.kind IN ('broadcast', 'sequence') THEN 1 ELSE 0 END, m.next_attempt_at, m.id
		LIMIT ?`, db.FormatTime(s.clock.Now()), limit)
	if err != nil {
		return nil, fmt.Errorf("sending: select due: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// claim switches a message from queued to sending in its own short
// transaction, re-checking every dispatch condition (due, broadcast
// sending, not globally paused) atomically. Only one caller can win.
func (s *Service) claim(ctx context.Context, id int64) (bool, error) {
	now := db.FormatTime(s.clock.Now())
	res, err := s.db.Write.ExecContext(ctx, `
		UPDATE messages SET status = 'sending', updated_at = ?
		WHERE id = ? AND id IN (SELECT m.id FROM messages m WHERE m.id = ? AND `+dueWhere+`)
		  AND NOT EXISTS (SELECT 1 FROM settings WHERE key = '`+StateKey+`' AND json_extract(value, '$.paused') = 1)`,
		now, id, id, now)
	if err != nil {
		return false, fmt.Errorf("sending: claim message %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SendDue sends every message that is due now, one at a time and without
// the token bucket, and returns how many it claimed. It stops when a round
// claims nothing (messages retried later are not due again). It exists for
// tests and tools that drive the clock themselves; the sender worker is
// the production path.
func (s *Service) SendDue(ctx context.Context) (int, error) {
	total := 0
	for {
		ids, err := s.dueIDs(ctx, dispatchBatch)
		if err != nil {
			return total, err
		}
		claimed := 0
		for _, id := range ids {
			ok, err := s.claim(ctx, id)
			if err != nil {
				return total, err
			}
			if !ok {
				continue
			}
			claimed++
			if err := s.deliver(context.WithoutCancel(ctx), id); err != nil {
				return total + claimed, err
			}
		}
		total += claimed
		if claimed == 0 {
			_, err := s.CompleteBroadcasts(ctx)
			return total, err
		}
	}
}

// process sends one dispatched message: it takes a token from the global
// bucket, claims the message and delivers it. Once claimed, the message is
// finished even if ctx is cancelled (shutdown), so it is never left in
// "sending" by a clean stop.
func (s *Service) process(ctx context.Context, id int64) error {
	if err := s.bucket.Wait(ctx); err != nil {
		return nil // shutting down
	}
	ok, err := s.claim(ctx, id)
	if err != nil || !ok {
		s.bucket.Refund()
		return err
	}
	return s.deliver(context.WithoutCancel(ctx), id)
}

// CompleteBroadcasts sets broadcasts in "sending" to "sent" once their
// snapshot is done (snapshot_done_at set by the broadcasts package when
// every recipient row has been queued) and none of their messages is
// queued or sending (B-2: only the sender sets sent). A broadcast whose
// snapshot queued no messages at all completes too. It returns the IDs it
// completed.
func (s *Service) CompleteBroadcasts(ctx context.Context) ([]int64, error) {
	now := db.FormatTime(s.clock.Now())
	rows, err := s.db.Write.QueryContext(ctx, `
		UPDATE broadcasts SET status = 'sent', finished_at = ?, updated_at = ?, version = version + 1
		WHERE status = 'sending' AND snapshot_done_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.broadcast_id = broadcasts.id AND m.status IN ('queued', 'sending'))
		RETURNING id`, now, now)
	if err != nil {
		return nil, fmt.Errorf("sending: complete broadcasts: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		s.log.Info("sending: broadcast sent", "broadcast_id", id)
	}
	return ids, nil
}

// completeLoop completes finished broadcasts every s.completeInterval
// until ctx is done, so a broadcast becomes "sent" soon after its last
// message even while the sender is busy with a backlog of other messages.
func (s *Service) completeLoop(ctx context.Context) {
	t := time.NewTicker(s.completeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.CompleteBroadcasts(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("sending: complete broadcasts", "err", err)
			}
		}
	}
}

// refreshRate applies the current B-4 rate to the token bucket.
func (s *Service) refreshRate(ctx context.Context) {
	st, err := s.settings.Get(ctx)
	if err != nil {
		s.log.Warn("sending: read settings for rate", "err", err)
		return
	}
	r := effectiveRate(st.Sending, s.currentAccount())
	if r != s.bucket.Rate() {
		s.bucket.SetRate(r)
		s.log.Info("sending: send rate set", "per_sec", r)
	}
}

// senderWorker is the "sender" worker: one dispatcher and N workers.
type senderWorker struct{ s *Service }

func (w *senderWorker) Name() string { return "sender" }

// Run recovers interrupted messages, then dispatches due messages to the
// workers until ctx is cancelled. It returns only after every worker has
// finished its current message.
func (w *senderWorker) Run(ctx context.Context) error {
	s := w.s
	if !s.running.TryLock() {
		return errors.New("sending: sender is already running")
	}
	defer s.running.Unlock()
	if _, err := s.RecoverInterrupted(ctx); err != nil {
		return err
	}
	s.loadAccount(ctx)

	d := &dispatcher{s: s, ids: make(chan int64), wake: make(chan struct{}, 1), inflight: map[int64]struct{}{}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.completeLoop(ctx)
	}()
	for range s.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.work(ctx)
		}()
	}
	defer func() {
		close(d.ids)
		wg.Wait()
	}()

	for {
		n, err := d.dispatch(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			s.log.Error("sending: dispatch", "err", err)
		}
		if n > 0 && err == nil {
			continue
		}
		if _, err := s.CompleteBroadcasts(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("sending: complete broadcasts", "err", err)
		}
		if !wait(ctx, s.pollInterval, d.wake) {
			return nil
		}
	}
}

// dispatcher hands due message IDs to the workers, never the same ID to
// two workers at once (the claim would stop a double send anyway).
type dispatcher struct {
	s    *Service
	ids  chan int64
	wake chan struct{}

	mu       sync.Mutex
	inflight map[int64]struct{}
}

// dispatch selects one batch of due messages and hands the ones not yet in
// flight to the workers. It returns how many it handed out.
func (d *dispatcher) dispatch(ctx context.Context) (int, error) {
	d.s.refreshRate(ctx)
	d.mu.Lock()
	busy := len(d.inflight)
	d.mu.Unlock()
	ids, err := d.s.dueIDs(ctx, batchSize(d.s.bucket.Rate(), d.s.workers)+busy)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		d.mu.Lock()
		_, dup := d.inflight[id]
		if !dup {
			d.inflight[id] = struct{}{}
		}
		d.mu.Unlock()
		if dup {
			continue
		}
		select {
		case d.ids <- id:
			n++
		case <-ctx.Done():
			d.done(id)
			return n, nil
		}
	}
	return n, nil
}

// batchSize is how many messages one dispatcher round hands out: about
// two seconds of sending at rate, at least two per worker and at most
// dispatchBatch. Small rounds keep a transactional message that arrives
// during a backlog (see dueIDs) from waiting behind a long batch.
func batchSize(rate float64, workers int) int {
	n := int(math.Ceil(2 * rate))
	n = max(n, 2*workers)
	return min(n, dispatchBatch)
}

func (d *dispatcher) done(id int64) {
	d.mu.Lock()
	delete(d.inflight, id)
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *dispatcher) work(ctx context.Context) {
	for id := range d.ids {
		if err := d.s.process(ctx, id); err != nil {
			d.s.log.Error("sending: message", "message_id", id, "err", err)
		}
		d.done(id)
	}
}
