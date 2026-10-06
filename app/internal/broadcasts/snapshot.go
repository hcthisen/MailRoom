package broadcasts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mailroom/internal/core"
	"mailroom/internal/jobs"
	"mailroom/internal/outbox"
)

// SnapshotResult is the result of a broadcast.snapshot job.
type SnapshotResult struct {
	BroadcastID     int64  `json:"broadcast_id"`
	Queued          int64  `json:"queued" doc:"Messages queued by this run"`
	AlreadyQueued   int64  `json:"already_queued" doc:"Recipients found already queued (a resumed or repeated run)"`
	Suppressed      int64  `json:"suppressed" doc:"Audience contacts left out because their address is suppressed"`
	Invalid         int64  `json:"invalid" doc:"Audience contacts left out because their stored address can't be sent to"`
	RecipientsTotal int64  `json:"recipients_total" doc:"Messages of the broadcast once the snapshot is done"`
	Done            bool   `json:"done" doc:"true when every recipient is queued"`
	Stopped         string `json:"stopped,omitempty" doc:"Why the run stopped early, e.g. the broadcast was cancelled"`
}

// candidate is an audience contact of one snapshot batch.
type candidate struct {
	id    int64
	email string
}

// runSnapshot is the broadcast.snapshot job handler (B-3). It is
// idempotent: it resumes after the highest contact already queued, and
// outbox.Enqueue skips contacts that already have a message.
func (s *Service) runSnapshot(ctx context.Context, job jobs.JobContext) (any, error) {
	var p snapshotParams
	if err := job.Params(&p); err != nil {
		return nil, fmt.Errorf("broadcasts: snapshot params: %w", err)
	}
	res := &SnapshotResult{BroadcastID: p.BroadcastID}
	b, err := s.get(ctx, nil, p.BroadcastID)
	if errors.Is(err, core.ErrNotFound) {
		res.Stopped = "broadcast deleted"
		return res, nil
	}
	if err != nil {
		return res, err
	}
	if b.SnapshotDoneAt != nil {
		res.Done, res.RecipientsTotal = true, b.RecipientsTotal
		return res, nil
	}
	where, args, err := s.audienceWhere(ctx, b.Audience)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrValidation) {
			return res, s.failSnapshot(ctx, b, err)
		}
		return res, err
	}
	// Batches commit in contact-ID order, so every contact up to the
	// highest one already queued has been handled.
	var after int64
	if err := s.db.Read.QueryRowContext(ctx,
		`SELECT IFNULL(max(contact_id), 0) FROM messages WHERE broadcast_id = ?`, b.ID).Scan(&after); err != nil {
		return res, fmt.Errorf("broadcasts: snapshot resume point: %w", err)
	}
	query := `SELECT c.id, c.email FROM contacts c WHERE ` + where + ` AND c.id > ? AND c.id <= ? ORDER BY c.id LIMIT ?`
	for !res.Done && res.Stopped == "" {
		err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
			return s.snapshotBatch(ctx, tx, b, query, args, p.MaxContactID, &after, res)
		})
		if err != nil {
			return res, err
		}
		if p.MaxContactID > 0 {
			job.SetProgress(min(float64(after)/float64(p.MaxContactID), 1))
		}
	}
	if res.Done {
		job.SetProgress(1)
		s.log.Info("broadcasts: snapshot done", "broadcast_id", b.ID, "recipients", res.RecipientsTotal)
	}
	return res, nil
}

// snapshotBatch queues the next batch of recipients in tx. The batch that
// finds no more recipients after it also sets recipients_total and
// snapshot_done_at, in the same transaction.
func (s *Service) snapshotBatch(ctx context.Context, tx *sql.Tx, b Broadcast, query string, args []any,
	maxID int64, after *int64, res *SnapshotResult) error {
	var status string
	var done sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, snapshot_done_at FROM broadcasts WHERE id = ?`, b.ID).Scan(&status, &done); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			res.Stopped = "broadcast deleted"
			return nil
		}
		return fmt.Errorf("broadcasts: snapshot status: %w", err)
	}
	switch {
	case done.Valid:
		res.Done = true
		return tx.QueryRowContext(ctx, `SELECT recipients_total FROM broadcasts WHERE id = ?`, b.ID).Scan(&res.RecipientsTotal)
	case status != StatusSending && status != StatusPaused:
		res.Stopped = "broadcast is " + status
		return nil
	}
	cands, err := batchCandidates(ctx, tx, query, append(append([]any{}, args...), *after, maxID, SnapshotBatchSize+1))
	if err != nil {
		return err
	}
	last := len(cands) <= SnapshotBatchSize
	if !last {
		cands = cands[:SnapshotBatchSize]
	}
	emails := make([]string, len(cands))
	for i, c := range cands {
		emails[i] = c.email
	}
	sup, err := suppressedHashes(ctx, tx, emails)
	if err != nil {
		return err
	}
	var queued, existing, suppressed, invalid int64
	for _, c := range cands {
		if sup[c.email] {
			suppressed++
			continue
		}
		_, created, err := outbox.Enqueue(ctx, tx, s.clock, outbox.MessageSpec{
			Kind: core.KindBroadcast, BroadcastID: b.ID, EmailID: b.Email.ID, ContactID: c.id, ToEmail: c.email,
		})
		if errors.Is(err, core.ErrValidation) {
			// One unusable stored address must not stall the whole
			// snapshot: leave that contact out (Enqueue wrote nothing).
			invalid++
			s.log.Warn("broadcasts: snapshot skipped a contact whose address can't be sent to",
				"broadcast_id", b.ID, "contact_id", c.id, "err", err)
			continue
		}
		if err != nil {
			return fmt.Errorf("broadcasts: queue contact %d: %w", c.id, err)
		}
		if created {
			queued++
		} else {
			existing++
		}
	}
	if len(cands) > 0 {
		*after = cands[len(cands)-1].id
	}
	if last {
		now := s.now()
		if err := tx.QueryRowContext(ctx, `
			UPDATE broadcasts SET recipients_total = (SELECT count(*) FROM messages WHERE broadcast_id = ?1),
			       snapshot_done_at = ?2, version = version + 1, updated_at = ?2
			WHERE id = ?1 RETURNING recipients_total`, b.ID, now).Scan(&res.RecipientsTotal); err != nil {
			return fmt.Errorf("broadcasts: finish snapshot: %w", err)
		}
	}
	// The caller commits the batch; a failed commit fails the job.
	res.Queued += queued
	res.AlreadyQueued += existing
	res.Suppressed += suppressed
	res.Invalid += invalid
	res.Done = last
	return nil
}

func batchCandidates(ctx context.Context, tx *sql.Tx, query string, args []any) ([]candidate, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("broadcasts: snapshot audience: %w", err)
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.email); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// failSnapshot pauses a broadcast whose audience can't be resolved (reason
// snapshot_failed) and alerts the owner. It returns cause for the job.
func (s *Service) failSnapshot(ctx context.Context, b Broadcast, cause error) error {
	ctx = systemCtx(ctx)
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := s.pauseSnapshotFailed(ctx, tx, b.ID, b.Name, cause)
		return err
	})
	if err != nil {
		return fmt.Errorf("broadcasts: pause after failed snapshot: %w (snapshot: %v)", err, cause)
	}
	return fmt.Errorf("broadcast %d snapshot: %w", b.ID, cause)
}

// pauseSnapshotFailed pauses a sending broadcast whose recipients can't be
// queued (reason snapshot_failed), audits it and alerts the owner (key
// "broadcast_snapshot_failed:<id>"), all in tx. It reports whether the
// broadcast was still sending.
func (s *Service) pauseSnapshotFailed(ctx context.Context, tx *sql.Tx, id int64, name string, cause error) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE broadcasts SET status = 'paused', paused_reason = ?, version = version + 1, updated_at = ?
		WHERE id = ? AND status = 'sending'`, PauseSnapshotFailed, s.now(), id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := s.logAudit(ctx, tx, "broadcast.snapshot_failed", id, map[string]any{
		"status": map[string]any{"from": StatusSending, "to": StatusPaused}, "error": cause.Error(),
	}); err != nil {
		return false, err
	}
	s.log.Warn("broadcasts: snapshot failed, broadcast paused", "broadcast_id", id, "err", cause)
	return true, s.alert(ctx, tx, fmt.Sprintf("broadcast_snapshot_failed:%d", id),
		fmt.Sprintf("Broadcast %q paused: its recipients could not be selected", name),
		fmt.Sprintf("Broadcast %d (%s) was paused because its recipients could not be selected and queued: %v.\n\n"+
			"Fix the cause (e.g. the audience segment) and resume the broadcast; the recipients queued so far are kept.", id, name, cause))
}

// alert raises an alert in tx, or only logs it when no Alerter is set.
func (s *Service) alert(ctx context.Context, tx *sql.Tx, key, title, body string) error {
	if s.alerter == nil {
		s.log.Warn("broadcasts: alert (no alerter configured)", "key", key, "title", title)
		return nil
	}
	return s.alerter.Alert(ctx, tx, key, title, body)
}
