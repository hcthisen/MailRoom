package sequences

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// exitWhere ends the active enrollments matching cond (a condition over
// the enrollments table with args) with reason, and returns how many
// ended. It adds no exit tags: use exitEnrollments for condition exits.
func (s *Service) exitWhere(ctx context.Context, tx *sql.Tx, cond string, args []any, reason string) (int, error) {
	now := db.FormatTime(s.now())
	res, err := tx.ExecContext(ctx, `
		UPDATE enrollments SET status = 'exited', exit_reason = ?, next_run_at = NULL, finished_at = ?, updated_at = ?
		WHERE status = 'active' AND `+cond, append([]any{reason, now, now}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("sequences: exit enrollments (%s): %w", reason, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// exiting is an active enrollment about to end.
type exiting struct {
	ID           int64
	SequenceID   int64
	ContactID    int64
	ExitAddTagID int64
}

// exitEnrollments ends the given active enrollments with reason. When the
// reason is an exit condition (exit tag, contact no longer active), each
// sequence's exit_add_tag is then added to the contact, which publishes
// TagAdded in tx (and may start or end other sequences).
//
// Every enrollment of list ends before the first exit tag is added: an
// added tag can end other enrollments through the TagAdded hook, and
// those in list must keep list's reason rather than be overtaken.
func (s *Service) exitEnrollments(ctx context.Context, tx *sql.Tx, list []exiting, reason string) error {
	ended := make([]exiting, 0, len(list))
	for _, e := range list {
		n, err := s.exitWhere(ctx, tx, `id = ?`, []any{e.ID}, reason)
		if err != nil {
			return err
		}
		if n > 0 {
			ended = append(ended, e)
		}
	}
	if reason != ExitTag && reason != ExitNotActive {
		return nil
	}
	for _, e := range ended {
		if e.ExitAddTagID == 0 {
			continue
		}
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM tags WHERE id = ?`, e.ExitAddTagID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			continue // the tag was deleted meanwhile
		}
		if err != nil {
			return fmt.Errorf("sequences: exit tag of sequence %d: %w", e.SequenceID, err)
		}
		if err := s.store.AddTags(ctx, tx, e.ContactID, []string{name}, "sequence:"+strconv.FormatInt(e.SequenceID, 10)); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			return fmt.Errorf("sequences: add exit tag %q to contact %d: %w", name, e.ContactID, err)
		}
	}
	return nil
}

// activeEnrollments returns the contact's active enrollments selected by
// cond (a condition over enrollments e and sequences s) with args.
func activeEnrollments(ctx context.Context, tx *sql.Tx, contactID int64, cond string, args ...any) ([]exiting, error) {
	q := `SELECT e.id, e.sequence_id, e.contact_id, COALESCE(s.exit_add_tag_id, 0)
		FROM enrollments e JOIN sequences s ON s.id = e.sequence_id
		WHERE e.contact_id = ? AND e.status = 'active'`
	if cond != "" {
		q += ` AND ` + cond
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY e.id`, append([]any{contactID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("sequences: active enrollments of contact %d: %w", contactID, err)
	}
	defer rows.Close()
	var out []exiting
	for rows.Next() {
		var e exiting
		if err := rows.Scan(&e.ID, &e.SequenceID, &e.ContactID, &e.ExitAddTagID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Unenroll ends an active enrollment of the sequence manually (reason
// manual). A finished enrollment is core.ErrConflict.
func (s *Service) Unenroll(ctx context.Context, sequenceID, enrollmentID int64) (SequenceEnrollment, error) {
	var out SequenceEnrollment
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := getEnrollment(ctx, tx, sequenceID, enrollmentID)
		if err != nil {
			return err
		}
		if cur.Status != EnrollmentActive {
			return fmt.Errorf("enrollment %d is already %s: %w", enrollmentID, cur.Status, core.ErrConflict)
		}
		if err := s.exitEnrollments(ctx, tx, []exiting{{ID: cur.ID, SequenceID: sequenceID, ContactID: cur.ContactID}}, ExitManual); err != nil {
			return err
		}
		if out, err = getEnrollment(ctx, tx, sequenceID, enrollmentID); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "sequence.unenroll", "enrollment", enrollmentID, map[string]any{
			"sequence_id": sequenceID, "contact_id": cur.ContactID,
		})
	})
	return out, err
}
