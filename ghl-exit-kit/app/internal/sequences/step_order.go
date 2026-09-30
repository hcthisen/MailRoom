package sequences

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// DeleteStepResult reports a step deletion.
type DeleteStepResult struct {
	Sequence  Sequence `json:"sequence"`
	Advanced  int      `json:"advanced" doc:"Active enrollments moved to the following step (on_active=advance); those at the last step completed" example:"3"`
	Completed int      `json:"completed" doc:"Of the advanced enrollments, those that had no following step and completed" example:"0"`
	Exited    int      `json:"exited" doc:"Active enrollments ended with reason step_deleted (on_active=exit)" example:"0"`
}

// DeleteStep deletes a step and closes the gap in the positions (S-7).
// Active enrollments waiting for it need a decision in onActive: advance
// (they get the following step when the deleted one was due, or complete
// when it was the last) or exit (reason step_deleted). Without one,
// deleting such a step is core.ErrConflict. The step's email is kept, and
// messages already queued for it are still sent. Exiting ends those
// enrollments like any other exit: a message of theirs that is still
// queued (for an earlier step, e.g. while sending is paused) is cancelled
// by the sender. Advancing keeps such messages deliverable.
func (s *Service) DeleteStep(ctx context.Context, seqID, stepID int64, onActive string, ifVersion int64) (DeleteStepResult, error) {
	var res DeleteStepResult
	if onActive != "" && onActive != OnActiveAdvance && onActive != OnActiveExit {
		return res, core.Invalid("query.on_active", "must be advance or exit")
	}
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := lockSequence(ctx, tx, seqID, ifVersion); err != nil {
			return err
		}
		cur, err := loadStep(ctx, tx, seqID, stepID)
		if err != nil {
			return err
		}
		var waiting int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM enrollments WHERE next_step_id = ? AND status = 'active'`,
			stepID).Scan(&waiting); err != nil {
			return fmt.Errorf("sequences: waiting enrollments: %w", err)
		}
		if waiting > 0 && onActive == "" {
			return fmt.Errorf("%d active enrollment(s) are waiting for step %d; pass on_active=advance or on_active=exit: %w",
				waiting, cur.Position, core.ErrConflict)
		}
		if waiting > 0 {
			if err := s.releaseWaiting(ctx, tx, cur, onActive, &res); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sequence_steps WHERE id = ?`, stepID); err != nil {
			return fmt.Errorf("sequences: delete step %d: %w", stepID, err)
		}
		if err := s.shiftPositions(ctx, tx, seqID, cur.Position+1, -1); err != nil {
			return err
		}
		if err := s.bumpVersion(ctx, tx, seqID); err != nil {
			return err
		}
		if res.Sequence, err = s.get(ctx, tx, seqID); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "sequence.step.delete", "sequence", seqID, map[string]any{
			"step_id": stepID, "position": cur.Position, "email_id": cur.EmailID, "on_active": onActive,
			"advanced": res.Advanced, "completed": res.Completed, "exited": res.Exited,
		})
	})
	return res, err
}

// releaseWaiting moves or ends the enrollments waiting for step cur.
func (s *Service) releaseWaiting(ctx context.Context, tx *sql.Tx, cur stepRow, onActive string, res *DeleteStepResult) error {
	if onActive == OnActiveExit {
		n, err := s.exitWhere(ctx, tx, `next_step_id = ?`, []any{cur.ID}, ExitStepDeleted)
		res.Exited = n
		return err
	}
	now := db.FormatTime(s.now())
	var nextID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM sequence_steps WHERE sequence_id = ? AND position > ? ORDER BY position LIMIT 1`,
		cur.SequenceID, cur.Position).Scan(&nextID)
	if errors.Is(err, sql.ErrNoRows) {
		r, err := tx.ExecContext(ctx, `UPDATE enrollments SET status = 'completed', next_step_id = NULL, next_run_at = NULL,
			finished_at = ?, updated_at = ? WHERE next_step_id = ? AND status = 'active'`, now, now, cur.ID)
		if err != nil {
			return fmt.Errorf("sequences: complete waiting enrollments: %w", err)
		}
		n, _ := r.RowsAffected()
		res.Advanced, res.Completed = int(n), int(n)
		return nil
	}
	if err != nil {
		return fmt.Errorf("sequences: step after %d: %w", cur.ID, err)
	}
	r, err := tx.ExecContext(ctx, `UPDATE enrollments SET next_step_id = ?, updated_at = ? WHERE next_step_id = ? AND status = 'active'`,
		nextID, now, cur.ID)
	if err != nil {
		return fmt.Errorf("sequences: advance waiting enrollments: %w", err)
	}
	n, _ := r.RowsAffected()
	res.Advanced = int(n)
	return nil
}

// ReorderResult reports a reorder.
type ReorderResult struct {
	Sequence  Sequence `json:"sequence"`
	Completed int      `json:"completed" doc:"Active enrollments moved to the end and completed (on_active=end)" example:"0"`
}

// ReorderSteps puts the steps in the order of stepIDs, which must list
// every step of the sequence exactly once (S-7). onActive says what
// happens to active enrollments: keep ("" or OnActiveKeep) keeps their
// next step by ID, so an enrollment waiting for a step that moved gets it
// next and continues from its new position, skipping steps it already
// received; end (OnActiveEnd) completes them, so they get no further
// steps (their queued messages are still sent). A reorder that changes
// nothing changes no enrollment. ifVersion is the sequence's If-Match.
func (s *Service) ReorderSteps(ctx context.Context, seqID int64, stepIDs []int64, onActive string, ifVersion int64) (ReorderResult, error) {
	var out ReorderResult
	if onActive == "" {
		onActive = OnActiveKeep
	}
	if onActive != OnActiveKeep && onActive != OnActiveEnd {
		return out, core.Invalid("query.on_active", "must be keep or end")
	}
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := lockSequence(ctx, tx, seqID, ifVersion); err != nil {
			return err
		}
		steps, err := loadSteps(ctx, tx, seqID)
		if err != nil {
			return err
		}
		if err := checkPermutation(steps, stepIDs); err != nil {
			return err
		}
		before := make([]int64, len(steps))
		changed := false
		for i, st := range steps {
			before[i] = st.ID
			changed = changed || st.ID != stepIDs[i]
		}
		if !changed {
			out.Sequence, err = s.get(ctx, tx, seqID)
			return err
		}
		now := db.FormatTime(s.now())
		if _, err := tx.ExecContext(ctx, `UPDATE sequence_steps SET position = -position WHERE sequence_id = ?`, seqID); err != nil {
			return fmt.Errorf("sequences: reorder: %w", err)
		}
		for i, id := range stepIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE sequence_steps SET position = ?, updated_at = ? WHERE id = ?`, i+1, now, id); err != nil {
				return fmt.Errorf("sequences: reorder: %w", err)
			}
		}
		if onActive == OnActiveEnd {
			r, err := tx.ExecContext(ctx, `UPDATE enrollments SET status = 'completed', next_step_id = NULL, next_run_at = NULL,
				finished_at = ?, updated_at = ? WHERE sequence_id = ? AND status = 'active'`, now, now, seqID)
			if err != nil {
				return fmt.Errorf("sequences: complete active enrollments: %w", err)
			}
			n, _ := r.RowsAffected()
			out.Completed = int(n)
		}
		if err := s.bumpVersion(ctx, tx, seqID); err != nil {
			return err
		}
		if out.Sequence, err = s.get(ctx, tx, seqID); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "sequence.steps.reorder", "sequence", seqID, map[string]any{
			"from": before, "to": stepIDs, "on_active": onActive, "completed": out.Completed,
		})
	})
	return out, err
}

// checkPermutation checks that ids lists every step exactly once.
func checkPermutation(steps []stepRow, ids []int64) error {
	if len(ids) != len(steps) {
		return core.Invalid("step_ids", "must list all %d step(s) of the sequence exactly once", len(steps))
	}
	have := make(map[int64]bool, len(steps))
	for _, st := range steps {
		have[st.ID] = true
	}
	seen := make(map[int64]bool, len(ids))
	for i, id := range ids {
		if !have[id] {
			return core.Invalid(fmt.Sprintf("step_ids[%d]", i), "step %d is not a step of this sequence", id)
		}
		if seen[id] {
			return core.Invalid(fmt.Sprintf("step_ids[%d]", i), "step %d is listed twice", id)
		}
		seen[id] = true
	}
	return nil
}
