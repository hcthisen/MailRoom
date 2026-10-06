package sequences

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/outbox"
	"mailroom/internal/server"
	"mailroom/internal/settings"
)

// SchedulerResult summarises one scheduler run.
type SchedulerResult struct {
	Queued    int // steps queued as messages
	Completed int // enrollments that received their last step
	Exited    int // enrollments that met an exit condition
	Deferred  int // steps moved to the next opening of the send window
	Failed    int // enrollments whose processing failed (retried next run)
}

// outcome is what processing one enrollment did.
type outcome int

const (
	outcomeNone outcome = iota // not due any more (changed meanwhile)
	outcomeQueued
	outcomeCompleted // queued its last step, or had no step left
	outcomeExited
	outcomeDeferred
	outcomeAdvanced // moved past a step it had received before; nothing queued
)

// schedule is the send-window context of one pass.
type schedule struct {
	defaultWindow *settings.SendWindow
	loc           *time.Location
}

// RunDue processes every due enrollment (S-6): status active, next_run_at
// at or before now, sequence active. Each enrollment is handled in its own
// transaction: the exit conditions are checked again (contact not active,
// an exit tag), a step due outside the send window moves to the window's
// next opening, and otherwise the step is queued as a message and the
// enrollment advances to the next step it hasn't received yet (due now +
// that step's delay) or completes, all in that one transaction. The UNIQUE (enrollment_id,
// step_id) constraint on messages makes every step go out at most once,
// also when runs overlap or repeat after a crash.
//
// Enrollments are read in batches of SchedulerBatch. Steps with a zero
// delay become due at once and are processed by a further pass of the
// same run. Failures are logged, counted and retried by the next run.
func (s *Service) RunDue(ctx context.Context) (SchedulerResult, error) {
	var res SchedulerResult
	failed := map[int64]bool{}
	for pass := 0; pass <= MaxSteps; pass++ {
		sch, err := s.schedule(ctx)
		if err != nil {
			return res, err
		}
		now := s.now()
		progressed := false
		var after int64
		for {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			ids, err := s.dueIDs(ctx, now, after)
			if err != nil {
				return res, err
			}
			for _, id := range ids {
				after = id
				if failed[id] {
					continue
				}
				out, err := s.process(ctx, id, sch)
				if err != nil {
					if ctx.Err() != nil {
						return res, ctx.Err()
					}
					s.log.ErrorContext(ctx, "sequences: scheduler: enrollment failed", "enrollment_id", id, "err", err)
					failed[id] = true
					res.Failed++
					continue
				}
				switch out {
				case outcomeQueued:
					res.Queued++
				case outcomeCompleted:
					res.Completed++
				case outcomeExited:
					res.Exited++
				case outcomeDeferred:
					res.Deferred++
				}
				progressed = progressed || out == outcomeQueued || out == outcomeCompleted || out == outcomeAdvanced
			}
			if len(ids) < SchedulerBatch {
				break
			}
		}
		if !progressed {
			break
		}
	}
	return res, nil
}

// schedule reads the default send window and time zone once per pass.
func (s *Service) schedule(ctx context.Context) (schedule, error) {
	st, err := s.appSettings(ctx)
	if err != nil {
		return schedule{}, err
	}
	return schedule{defaultWindow: st.DefaultSendWindow, loc: st.Location()}, nil
}

// dueIDs returns up to SchedulerBatch due enrollment IDs greater than after.
func (s *Service) dueIDs(ctx context.Context, now time.Time, after int64) ([]int64, error) {
	rows, err := s.db.Read.QueryContext(ctx, `
		SELECT e.id FROM enrollments e JOIN sequences s ON s.id = e.sequence_id
		WHERE e.status = 'active' AND e.next_run_at <= ? AND s.status = 'active' AND e.id > ?
		ORDER BY e.id LIMIT ?`, db.FormatTime(now), after, SchedulerBatch)
	if err != nil {
		return nil, fmt.Errorf("sequences: due enrollments: %w", err)
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

// dueRow is an enrollment as the scheduler sees it.
type dueRow struct {
	SequenceID    int64
	ContactID     int64
	NextStepID    sql.NullInt64
	NextRunAt     sql.NullString
	Status        string
	SeqStatus     string
	Window        sql.NullString
	ExitAddTagID  int64
	ContactStatus string
	Email         string
}

// process handles one enrollment in its own transaction.
func (s *Service) process(ctx context.Context, id int64, sch schedule) (outcome, error) {
	out := outcomeNone
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		var r dueRow
		err := tx.QueryRowContext(ctx, `
			SELECT e.sequence_id, e.contact_id, e.next_step_id, e.next_run_at, e.status, s.status, s.send_window,
			       COALESCE(s.exit_add_tag_id, 0), c.status, c.email
			FROM enrollments e JOIN sequences s ON s.id = e.sequence_id JOIN contacts c ON c.id = e.contact_id
			WHERE e.id = ?`, id).Scan(&r.SequenceID, &r.ContactID, &r.NextStepID, &r.NextRunAt, &r.Status, &r.SeqStatus,
			&r.Window, &r.ExitAddTagID, &r.ContactStatus, &r.Email)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load enrollment: %w", err)
		}
		if r.Status != EnrollmentActive || r.SeqStatus != StatusActive || !r.NextRunAt.Valid || r.NextRunAt.String > db.FormatTime(now) {
			return nil
		}
		ex := []exiting{{ID: id, SequenceID: r.SequenceID, ContactID: r.ContactID, ExitAddTagID: r.ExitAddTagID}}
		if core.ContactStatus(r.ContactStatus) != core.StatusActive {
			out = outcomeExited
			return s.exitEnrollments(ctx, tx, ex, ExitNotActive)
		}
		has, err := hasExitTag(ctx, tx, r.SequenceID, r.ContactID)
		if err != nil {
			return err
		}
		if has {
			out = outcomeExited
			return s.exitEnrollments(ctx, tx, ex, ExitTag)
		}
		if !r.NextStepID.Valid {
			out = outcomeCompleted
			return s.complete(ctx, tx, id, now)
		}
		if next, wait, err := sch.nextOpening(r.Window, now); err != nil {
			return err
		} else if wait {
			out = outcomeDeferred
			return s.setNextRun(ctx, tx, id, r.NextStepID.Int64, next, now)
		}
		var step stepRow
		if err := tx.QueryRowContext(ctx, `SELECT id, position, email_id FROM sequence_steps WHERE id = ?`, r.NextStepID.Int64).
			Scan(&step.ID, &step.Position, &step.EmailID); err != nil {
			return fmt.Errorf("load step %d: %w", r.NextStepID.Int64, err)
		}
		_, created, err := outbox.Enqueue(ctx, tx, s.clock, outbox.MessageSpec{
			Kind: core.KindSequence, EnrollmentID: id, StepID: step.ID, EmailID: step.EmailID,
			ContactID: r.ContactID, ToEmail: r.Email,
		})
		if err != nil {
			return err
		}
		nextID, delay, ok, err := nextUnsentStep(ctx, tx, id, r.SequenceID, step.Position)
		if err != nil {
			return fmt.Errorf("next step after %d: %w", step.ID, err)
		}
		if !ok {
			out = outcomeCompleted
			return s.complete(ctx, tx, id, now)
		}
		out = outcomeQueued
		if !created {
			out = outcomeAdvanced // the step had been sent before (steps reordered)
		}
		return s.setNextRun(ctx, tx, id, nextID, now.Add(time.Duration(delay)*time.Second), now)
	})
	if err != nil {
		return outcomeNone, fmt.Errorf("sequences: enrollment %d: %w", id, err)
	}
	return out, nil
}

// nextUnsentStep returns the first step after position that the
// enrollment has no message for yet, with its delay. Steps it already
// received (possible after a reorder) are skipped, so no delay is spent
// waiting for a step that would send nothing. ok is false when none is
// left.
func nextUnsentStep(ctx context.Context, tx *sql.Tx, enrollmentID, seqID int64, position int) (id, delay int64, ok bool, err error) {
	err = tx.QueryRowContext(ctx, `
		SELECT st.id, st.delay_seconds FROM sequence_steps st
		WHERE st.sequence_id = ? AND st.position > ?
		  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.enrollment_id = ? AND m.step_id = st.id)
		ORDER BY st.position LIMIT 1`, seqID, position, enrollmentID).Scan(&id, &delay)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return id, delay, true, nil
}

// nextOpening reports whether now is outside the effective send window
// (the sequence's own, else the default) and, if so, when it next opens.
func (sch schedule) nextOpening(raw sql.NullString, now time.Time) (time.Time, bool, error) {
	w := sch.defaultWindow
	if raw.Valid && raw.String != "null" {
		var own settings.SendWindow
		if err := db.UnmarshalJSON(raw.String, &own); err != nil {
			return time.Time{}, false, fmt.Errorf("send window: %w", err)
		}
		w = &own
	}
	if w == nil || w.Contains(now, sch.loc) {
		return time.Time{}, false, nil
	}
	next := w.NextOpen(now, sch.loc)
	if !next.After(now) {
		return time.Time{}, false, nil // an unusable window never blocks sending
	}
	return next, true, nil
}

// setNextRun points an enrollment at a step due at runAt.
func (s *Service) setNextRun(ctx context.Context, tx *sql.Tx, id, stepID int64, runAt, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE enrollments SET next_step_id = ?, next_run_at = ?, updated_at = ? WHERE id = ?`,
		stepID, db.FormatTime(runAt), db.FormatTime(now), id)
	if err != nil {
		return fmt.Errorf("advance enrollment: %w", err)
	}
	return nil
}

// complete marks an enrollment completed.
func (s *Service) complete(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	ts := db.FormatTime(now)
	_, err := tx.ExecContext(ctx, `UPDATE enrollments SET status = 'completed', next_step_id = NULL, next_run_at = NULL,
		finished_at = ?, updated_at = ? WHERE id = ? AND status = 'active'`, ts, ts, id)
	if err != nil {
		return fmt.Errorf("complete enrollment: %w", err)
	}
	return nil
}

// SchedulerWorker returns the "scheduler" worker: it runs RunDue at start
// and then every scheduler interval (60 s) until its context is cancelled.
func (s *Service) SchedulerWorker() server.Worker { return schedulerWorker{s: s} }

type schedulerWorker struct{ s *Service }

// Name implements server.Worker.
func (schedulerWorker) Name() string { return "scheduler" }

// Run implements server.Worker.
func (w schedulerWorker) Run(ctx context.Context) error {
	t := time.NewTicker(w.s.interval)
	defer t.Stop()
	for {
		res, err := w.s.RunDue(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.s.log.ErrorContext(ctx, "sequences: scheduler run failed", "err", err)
		} else if res != (SchedulerResult{}) {
			w.s.log.InfoContext(ctx, "sequences: scheduler run", "queued", res.Queued, "completed", res.Completed,
				"exited", res.Exited, "deferred", res.Deferred, "failed", res.Failed)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
