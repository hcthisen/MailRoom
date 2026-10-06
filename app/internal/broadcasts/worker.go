package broadcasts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"mailroom/internal/db"
	"mailroom/internal/server"
)

// StarterWorker returns the "broadcast-starter" worker: every
// StarterInterval (and once at start) it runs Tick.
func (s *Service) StarterWorker() server.Worker {
	return &starter{s: s, interval: StarterInterval}
}

type starter struct {
	s        *Service
	interval time.Duration
}

func (w *starter) Name() string { return "broadcast-starter" }

func (w *starter) Run(ctx context.Context) error {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		if err := w.s.Tick(ctx); err != nil && ctx.Err() == nil {
			w.s.log.Error("broadcasts: starter", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick runs one round of the broadcast-starter: it starts scheduled
// broadcasts that are due, re-submits snapshots whose job failed or is
// gone, and runs the circuit breaker. It never marks broadcasts sent (the
// sending package does). Errors of one broadcast don't stop the others;
// they are joined into the result.
func (s *Service) Tick(ctx context.Context) error {
	ctx = systemCtx(ctx)
	_, errStart := s.StartDue(ctx)
	_, errSnap := s.RecoverSnapshots(ctx)
	_, errBreaker := s.CheckCircuitBreakers(ctx)
	return errors.Join(errStart, errSnap, errBreaker)
}

// StartDue starts every scheduled broadcast whose scheduled_at has come,
// each in its own transaction, and returns the IDs it started.
func (s *Service) StartDue(ctx context.Context) ([]int64, error) {
	ids, err := s.idsWhere(ctx, `status = 'scheduled' AND scheduled_at <= ?`, s.now())
	if err != nil {
		return nil, err
	}
	var started []int64
	var errs []error
	for _, id := range ids {
		ok, err := s.startScheduled(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			started = append(started, id)
			s.log.Info("broadcasts: scheduled broadcast started", "broadcast_id", id)
		}
	}
	return started, errors.Join(errs...)
}

func (s *Service) startScheduled(ctx context.Context, id int64) (bool, error) {
	var started bool
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var status, at string
		err := tx.QueryRowContext(ctx, `SELECT status, IFNULL(scheduled_at, '') FROM broadcasts WHERE id = ?`, id).Scan(&status, &at)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if status != StatusScheduled || at == "" || at > s.now() {
			return nil // unscheduled, sent now or rescheduled meanwhile
		}
		job, err := s.start(ctx, tx, id)
		if err != nil {
			return err
		}
		started = true
		return s.logAudit(ctx, tx, "broadcast.start", id, map[string]any{
			"status": map[string]any{"from": StatusScheduled, "to": StatusSending}, "scheduled_at": at, "snapshot_job_id": job.ID,
		})
	})
	if err != nil {
		return false, fmt.Errorf("broadcasts: start scheduled %d: %w", id, err)
	}
	return started, nil
}

// RecoverSnapshots re-submits the snapshot of sending broadcasts whose
// snapshot isn't done and whose job failed, finished early or is missing
// (e.g. a transient database error), and returns their IDs. The new job
// keeps the original contact bound. When MaxSnapshotAttempts jobs in a row
// have failed, the broadcast is paused (reason snapshot_failed) with an
// alert instead; its ID is not returned.
func (s *Service) RecoverSnapshots(ctx context.Context) ([]int64, error) {
	ids, err := s.idsWhere(ctx, `status = 'sending' AND snapshot_done_at IS NULL`)
	if err != nil {
		return nil, err
	}
	var recovered []int64
	var errs []error
	for _, id := range ids {
		ok, err := s.recoverSnapshot(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("broadcasts: recover snapshot %d: %w", id, err))
			continue
		}
		if ok {
			recovered = append(recovered, id)
		}
	}
	if len(recovered) > 0 {
		s.log.Warn("broadcasts: snapshots re-submitted", "broadcast_ids", recovered)
	}
	return recovered, errors.Join(errs...)
}

// recoverSnapshot re-submits one broadcast's stalled snapshot, or gives up
// after MaxSnapshotAttempts. It reports whether a job was submitted.
func (s *Service) recoverSnapshot(ctx context.Context, id int64) (bool, error) {
	var jobID sql.NullInt64
	if err := s.db.Read.QueryRowContext(ctx, `SELECT snapshot_job_id FROM broadcasts WHERE id = ?`, id).Scan(&jobID); err != nil {
		return false, err
	}
	var jp *int64
	if jobID.Valid {
		jp = &jobID.Int64
	}
	prev, err := s.snapshotJob(ctx, jp)
	if err != nil || !snapshotStalled(prev) {
		return false, err
	}
	var resubmitted bool
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		// Re-check under the write lock: the snapshot may have finished.
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM broadcasts WHERE id = ? AND status = 'sending' AND snapshot_done_at IS NULL
			AND snapshot_job_id IS ?`, id, db.NullID(jobID.Int64)).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := s.nextSnapshotParams(ctx, tx, id, prev)
		if err != nil {
			return err
		}
		if p.Attempt > MaxSnapshotAttempts {
			last := prev.Error
			if last == "" {
				last = "it stopped before every recipient was queued"
			}
			cause := fmt.Errorf("%d snapshot jobs in a row did not finish; the last: %s", MaxSnapshotAttempts, last)
			_, err := s.pauseSnapshotFailed(ctx, tx, id, name, cause)
			return err
		}
		job, err := s.submitSnapshot(ctx, tx, p)
		if err != nil {
			return err
		}
		resubmitted = true
		return s.logAudit(ctx, tx, "broadcast.snapshot_retry", id, map[string]any{
			"previous_job_id": jp, "snapshot_job_id": job.ID, "attempt": p.Attempt,
		})
	})
	return resubmitted, err
}
