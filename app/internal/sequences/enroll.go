package sequences

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Enroll enrols a contact in a sequence (core.Enroller) inside tx. It
// returns (0, nil) when the enrolment is skipped: the contact is not
// active, already has an active enrolment, the re-entry policy forbids
// another one, the contact has one of the sequence's exit tags, or the
// sequence has no steps yet. The enrolment starts at opts.AtStep (0 or 1:
// the first step) and its first step is due at opts.StartAt (nil: now)
// plus that step's delay. Paused sequences accept enrolments; the
// scheduler waits until they are resumed.
//
// An unknown sequence wraps core.ErrNotFound and an AtStep beyond the last
// step is a validation error for field at_step. Enroll only writes to tx
// and publishes no hooks, so it may be called from hook handlers.
func (s *Service) Enroll(ctx context.Context, tx *sql.Tx, sequenceID, contactID int64, opts core.EnrollOptions) (int64, error) {
	id, _, err := s.enroll(ctx, tx, sequenceID, contactID, opts)
	return id, err
}

// enroll implements Enroll and also returns the skip reason ("" when
// enrolled).
func (s *Service) enroll(ctx context.Context, tx *sql.Tx, sequenceID, contactID int64, opts core.EnrollOptions) (int64, string, error) {
	seq, err := loadSeq(ctx, tx, sequenceID)
	if err != nil {
		return 0, "", err
	}
	step, ok, err := startStep(ctx, tx, sequenceID, opts.AtStep)
	if err != nil || !ok {
		return 0, SkipNoSteps, err
	}
	reason, err := enrollBlocker(ctx, tx, seq, contactID)
	if err != nil || reason != "" {
		return 0, reason, err
	}
	now := s.now()
	start := now
	if opts.StartAt != nil {
		start = opts.StartAt.UTC()
	}
	source := strings.TrimSpace(opts.Source)
	if source == "" {
		source = "api"
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO enrollments (sequence_id, contact_id, next_step_id, next_run_at, status, source, started_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?) RETURNING id`,
		sequenceID, contactID, step.ID, db.FormatTime(start.Add(time.Duration(step.Delay)*time.Second)),
		source, db.FormatTime(now), db.FormatTime(now)).Scan(&id)
	if db.IsUniqueViolation(err) {
		return 0, SkipAlreadyActive, nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("sequences: enrol contact %d in %d: %w", contactID, sequenceID, err)
	}
	return id, "", nil
}

// startStep returns the step an enrolment at atStep starts with. ok is
// false when the sequence has no steps; an atStep beyond the last step is
// a validation error.
func startStep(ctx context.Context, tx db.Querier, sequenceID int64, atStep int) (stepRow, bool, error) {
	if atStep < 0 {
		return stepRow{}, false, core.Invalid("at_step", "must be 0 or a step position (1 = first step)")
	}
	if atStep == 0 {
		atStep = 1
	}
	var st stepRow
	err := tx.QueryRowContext(ctx, `SELECT id, position, delay_seconds, email_id FROM sequence_steps
		WHERE sequence_id = ? AND position = ?`, sequenceID, atStep).Scan(&st.ID, &st.Position, &st.Delay, &st.EmailID)
	if err == nil {
		return st, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return stepRow{}, false, fmt.Errorf("sequences: step %d of %d: %w", atStep, sequenceID, err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sequence_steps WHERE sequence_id = ?`, sequenceID).Scan(&n); err != nil {
		return stepRow{}, false, fmt.Errorf("sequences: count steps of %d: %w", sequenceID, err)
	}
	if n == 0 {
		return stepRow{}, false, nil
	}
	return stepRow{}, false, core.Invalid("at_step", "the sequence has %d step(s)", n)
}

// enrollBlocker returns why the contact can't be enrolled in seq now, or
// "" when it can: it is missing or not active, already enrolled, stopped
// this sequence from its unsubscribe page before (whatever the re-entry
// policy), is refused by the re-entry policy, or has an exit tag.
func enrollBlocker(ctx context.Context, tx db.Querier, seq seqRow, contactID int64) (string, error) {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM contacts WHERE id = ?`, contactID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return SkipNotFound, nil
	}
	if err != nil {
		return "", fmt.Errorf("sequences: contact %d: %w", contactID, err)
	}
	if core.ContactStatus(status) != core.StatusActive {
		return SkipNotActive, nil
	}
	var active, total, optedOut int
	var last sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM enrollments WHERE sequence_id = ?1 AND contact_id = ?2 AND status = 'active'),
			(SELECT count(*) FROM enrollments WHERE sequence_id = ?1 AND contact_id = ?2),
			(SELECT status FROM enrollments WHERE sequence_id = ?1 AND contact_id = ?2 ORDER BY started_at DESC, id DESC LIMIT 1),
			(SELECT count(*) FROM enrollments WHERE sequence_id = ?1 AND contact_id = ?2 AND exit_reason = ?3)`,
		seq.ID, contactID, ExitUnsubscribed).Scan(&active, &total, &last, &optedOut); err != nil {
		return "", fmt.Errorf("sequences: enrollments of contact %d: %w", contactID, err)
	}
	switch {
	case active > 0:
		return SkipAlreadyActive, nil
	case optedOut > 0:
		// "Only stop these emails" is a lasting opt-out of this sequence.
		return SkipUnsubscribed, nil
	case seq.Reentry == ReentryNever && total > 0:
		return SkipReentry, nil
	case seq.Reentry == ReentryAfterCompletion && last.Valid && last.String != EnrollmentCompleted:
		return SkipReentry, nil
	}
	has, err := hasExitTag(ctx, tx, seq.ID, contactID)
	if err != nil {
		return "", err
	}
	if has {
		return SkipExitTag, nil
	}
	return "", nil
}

// hasExitTag reports whether the contact has any of the sequence's exit
// tags (read from the sequence row now, so tag merges are respected).
func hasExitTag(ctx context.Context, q db.Querier, sequenceID, contactID int64) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM sequences s JOIN json_each(s.exit_tag_ids) je
		JOIN contact_tags ct ON ct.tag_id = je.value AND ct.contact_id = ?
		WHERE s.id = ? LIMIT 1`, contactID, sequenceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sequences: exit tags of contact %d: %w", contactID, err)
	}
	return true, nil
}
