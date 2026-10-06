package sequences

import (
	"context"
	"database/sql"
	"fmt"
	"time"
	"unicode/utf8"

	"mailroom/internal/api"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// StepEmail is the content of an email created for a step.
type StepEmail struct {
	Subject   string `json:"subject" maxLength:"300" doc:"Subject line; may use merge fields" example:"Welcome, {{first_name | \"friend\"}}!"`
	Preheader string `json:"preheader,omitempty" maxLength:"300" doc:"Preview text after the subject" example:"Here is what happens next"`
	Format    string `json:"format,omitempty" enum:"markdown,html" doc:"markdown (default) or html" example:"markdown"`
	Body      string `json:"body" doc:"The content, wrapped in the base layout when sent" example:"Hi {{first_name | \"there\"}},\n\nThanks for joining."`
}

// StepInput describes a new step: an existing email (EmailID) or new
// content (Email), a delay and an optional position (0 = at the end).
type StepInput struct {
	EmailID      int64
	Email        *StepEmail
	DelaySeconds int64
	Position     int
}

// StepPatch changes a step: its delay, the email it sends (EmailID) or
// that email's content (the other fields, through the content service).
type StepPatch struct {
	DelaySeconds *int64
	EmailID      *int64
	Subject      *string
	Preheader    *string
	Format       *string
	Body         *string
}

func (p StepPatch) editsContent() bool {
	return p.Subject != nil || p.Preheader != nil || p.Format != nil || p.Body != nil
}

// checkDelay validates a delay in seconds.
func checkDelay(field string, d int64) error {
	if d < 0 || d > MaxDelay {
		return core.Invalid(field, "must be between 0 and %d seconds (365 days)", MaxDelay)
	}
	return nil
}

// lockSequence loads a sequence for a step change and checks If-Match.
func lockSequence(ctx context.Context, tx *sql.Tx, id, ifVersion int64) (seqRow, error) {
	r, err := loadSeq(ctx, tx, id)
	if err != nil {
		return seqRow{}, err
	}
	return r, api.CheckVersion(ifVersion, r.Version)
}

// stepEmailName is the internal name of an email created for a step.
func stepEmailName(seqName string, position int) string {
	name := fmt.Sprintf("%s - step %d", seqName, position)
	for utf8.RuneCountInString(name) > content.MaxNameLength {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// AddStep adds a step to a sequence, at the end or at in.Position
// (later steps move down). A step added at the end also reaches active
// enrollments that haven't finished. ifVersion is the sequence's If-Match.
func (s *Service) AddStep(ctx context.Context, seqID int64, in StepInput, ifVersion int64) (Sequence, SequenceStep, error) {
	var seq Sequence
	var step SequenceStep
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		r, err := lockSequence(ctx, tx, seqID, ifVersion)
		if err != nil {
			return err
		}
		id, err := s.addStep(ctx, tx, r, in)
		if err != nil {
			return err
		}
		if err := s.bumpVersion(ctx, tx, seqID); err != nil {
			return err
		}
		if seq, step, err = s.withStep(ctx, tx, seqID, id); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "sequence.step.create", "sequence", seqID, map[string]any{
			"step_id": id, "position": step.Position, "delay_seconds": step.DelaySeconds, "email_id": step.EmailID,
		})
	})
	return seq, step, err
}

// addStep inserts a step in tx and returns its ID.
func (s *Service) addStep(ctx context.Context, tx *sql.Tx, seq seqRow, in StepInput) (int64, error) {
	if err := checkDelay("delay_seconds", in.DelaySeconds); err != nil {
		return 0, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sequence_steps WHERE sequence_id = ?`, seq.ID).Scan(&count); err != nil {
		return 0, fmt.Errorf("sequences: count steps: %w", err)
	}
	if count >= MaxSteps {
		return 0, core.Invalid("steps", "a sequence can have at most %d steps", MaxSteps)
	}
	pos := in.Position
	switch {
	case pos == 0:
		pos = count + 1
	case pos < 0 || pos > count+1:
		return 0, core.Invalid("position", "must be between 1 and %d", count+1)
	}
	emailID, err := s.stepEmail(ctx, tx, seq, in, pos)
	if err != nil {
		return 0, err
	}
	if pos <= count {
		if err := s.shiftPositions(ctx, tx, seq.ID, pos, 1); err != nil {
			return 0, err
		}
	}
	now := db.FormatTime(s.now())
	var id int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO sequence_steps (sequence_id, position, delay_seconds, email_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, seq.ID, pos, in.DelaySeconds, emailID, now, now).Scan(&id); err != nil {
		return 0, fmt.Errorf("sequences: insert step: %w", err)
	}
	return id, nil
}

// stepEmail returns the email of a new step: in.EmailID after checking it
// exists, or a new email created from in.Email.
func (s *Service) stepEmail(ctx context.Context, tx *sql.Tx, seq seqRow, in StepInput, pos int) (int64, error) {
	if (in.EmailID > 0) == (in.Email != nil) {
		return 0, core.Invalid("email_id", "give either email_id or email")
	}
	if in.EmailID > 0 {
		ok, err := s.content.EmailExists(ctx, tx, in.EmailID)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, core.Invalid("email_id", "email %d does not exist", in.EmailID)
		}
		return in.EmailID, nil
	}
	e, err := s.content.CreateEmail(ctx, tx, content.EmailInput{
		Name: stepEmailName(seq.Name, pos), Subject: in.Email.Subject, Preheader: in.Email.Preheader,
		Format: in.Email.Format, Body: in.Email.Body,
	})
	if err != nil {
		return 0, prefixField(err, "email.")
	}
	return e.ID, nil
}

// prefixField prefixes a validation error's field.
func prefixField(err error, prefix string) error {
	if f, ok := validationField(err); ok {
		return retarget(err, prefix+f)
	}
	return err
}

// shiftPositions moves the steps at position from and later by delta
// (+1 or -1). The UNIQUE (sequence_id, position) constraint is checked
// row by row, so the move goes through negative positions.
func (s *Service) shiftPositions(ctx context.Context, tx *sql.Tx, seqID int64, from, delta int) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sequence_steps SET position = -position WHERE sequence_id = ? AND position >= ?`,
		seqID, from); err != nil {
		return fmt.Errorf("sequences: move steps: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sequence_steps SET position = -position + ?, updated_at = ? WHERE sequence_id = ? AND position < 0`,
		delta, db.FormatTime(s.now()), seqID); err != nil {
		return fmt.Errorf("sequences: move steps: %w", err)
	}
	return nil
}

// withStep returns the sequence and one of its steps.
func (s *Service) withStep(ctx context.Context, q db.Querier, seqID, stepID int64) (Sequence, SequenceStep, error) {
	seq, err := s.get(ctx, q, seqID)
	if err != nil {
		return Sequence{}, SequenceStep{}, err
	}
	for _, st := range seq.Steps {
		if st.ID == stepID {
			return seq, st, nil
		}
	}
	return Sequence{}, SequenceStep{}, fmt.Errorf("step %d: %w", stepID, core.ErrNotFound)
}

// UpdateStep changes a step's delay, email or email content. Content
// edits apply to future sends (messages are rendered when sent) and to
// every other user of that email. A new delay also moves the due time of
// active enrollments waiting for this step by the difference. ifVersion
// is the sequence's If-Match.
func (s *Service) UpdateStep(ctx context.Context, seqID, stepID int64, p StepPatch, ifVersion int64) (Sequence, SequenceStep, error) {
	var seq Sequence
	var step SequenceStep
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := lockSequence(ctx, tx, seqID, ifVersion); err != nil {
			return err
		}
		cur, err := loadStep(ctx, tx, seqID, stepID)
		if err != nil {
			return err
		}
		changes, err := s.updateStep(ctx, tx, cur, p)
		if err != nil {
			return err
		}
		if len(changes) > 0 {
			if err := s.bumpVersion(ctx, tx, seqID); err != nil {
				return err
			}
			changes["step_id"] = stepID
			if err := s.logAudit(ctx, tx, "sequence.step.update", "sequence", seqID, changes); err != nil {
				return err
			}
		}
		seq, step, err = s.withStep(ctx, tx, seqID, stepID)
		return err
	})
	return seq, step, err
}

// updateStep applies p to step cur in tx and returns the changes.
func (s *Service) updateStep(ctx context.Context, tx *sql.Tx, cur stepRow, p StepPatch) (map[string]any, error) {
	changes := map[string]any{}
	if p.EmailID != nil && p.editsContent() {
		return nil, core.Invalid("email_id", "change either email_id or the email content, not both")
	}
	now := s.now()
	if p.EmailID != nil && *p.EmailID != cur.EmailID {
		ok, err := s.content.EmailExists(ctx, tx, *p.EmailID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, core.Invalid("email_id", "email %d does not exist", *p.EmailID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sequence_steps SET email_id = ?, updated_at = ? WHERE id = ?`,
			*p.EmailID, db.FormatTime(now), cur.ID); err != nil {
			return nil, fmt.Errorf("sequences: update step %d: %w", cur.ID, err)
		}
		changes["email_id"] = map[string]any{"from": cur.EmailID, "to": *p.EmailID}
	}
	if p.editsContent() {
		before, err := s.content.GetEmail(ctx, tx, cur.EmailID)
		if err != nil {
			return nil, err
		}
		after, err := s.content.UpdateEmail(ctx, tx, cur.EmailID, content.EmailPatch{
			Subject: p.Subject, Preheader: p.Preheader, Format: p.Format, Body: p.Body,
		}, 0)
		if err != nil {
			return nil, err
		}
		if after.Version != before.Version {
			changes["email_version"] = map[string]any{"from": before.Version, "to": after.Version}
		}
	}
	if p.DelaySeconds != nil && *p.DelaySeconds != cur.Delay {
		if err := checkDelay("delay_seconds", *p.DelaySeconds); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sequence_steps SET delay_seconds = ?, updated_at = ? WHERE id = ?`,
			*p.DelaySeconds, db.FormatTime(now), cur.ID); err != nil {
			return nil, fmt.Errorf("sequences: update step %d: %w", cur.ID, err)
		}
		moved, err := s.shiftWaiting(ctx, tx, cur.ID, time.Duration(*p.DelaySeconds-cur.Delay)*time.Second)
		if err != nil {
			return nil, err
		}
		changes["delay_seconds"] = map[string]any{"from": cur.Delay, "to": *p.DelaySeconds}
		changes["rescheduled_enrollments"] = moved
	}
	return changes, nil
}

// shiftWaiting moves the due time of active enrollments waiting for step
// stepID by d and returns how many moved.
func (s *Service) shiftWaiting(ctx context.Context, tx *sql.Tx, stepID int64, d time.Duration) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, next_run_at FROM enrollments
		WHERE next_step_id = ? AND status = 'active' AND next_run_at IS NOT NULL`, stepID)
	if err != nil {
		return 0, fmt.Errorf("sequences: waiting enrollments of step %d: %w", stepID, err)
	}
	type waiting struct {
		id    int64
		runAt time.Time
	}
	var list []waiting
	for rows.Next() {
		var w waiting
		var at string
		if err := rows.Scan(&w.id, &at); err != nil {
			rows.Close()
			return 0, err
		}
		if w.runAt, err = db.ParseTime(at); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, w)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := db.FormatTime(s.now())
	for _, w := range list {
		if _, err := tx.ExecContext(ctx, `UPDATE enrollments SET next_run_at = ?, updated_at = ? WHERE id = ?`,
			db.FormatTime(w.runAt.Add(d)), now, w.id); err != nil {
			return 0, fmt.Errorf("sequences: reschedule enrollment %d: %w", w.id, err)
		}
	}
	return len(list), nil
}
