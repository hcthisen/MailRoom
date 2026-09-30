package imports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// statusNote is the note of status changes made by an import.
const statusNote = "CSV import"

// unspecifiedSource is the consent source of rows without consent_source.
const unspecifiedSource = "unspecified"

// consentSource returns the consent log source of r (I-2).
func consentSource(r *record) string {
	src := r.consentSource
	if src == "" {
		src = unspecifiedSource
	}
	return "import:" + src
}

// applyBatch writes one batch of records in one transaction and, in the
// same transaction, stores the job's checkpoint (offset = records done
// after this batch, with the updated stats). st is updated only when the
// transaction commits, so a failed batch leaves both the database and the
// counts as they were.
func (s *Service) applyBatch(ctx context.Context, jobID int64, recs []*record, onExisting string, st *applyStats, offset int) error {
	actor := core.ActorFromContext(ctx).String()
	var next applyStats
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		next = st.clone()
		state, err := loadState(ctx, tx, recs)
		if err != nil {
			return err
		}
		for _, r := range recs {
			ex := state.existing[r.email]
			p := decide(r, ex, state.suppressed[r.email], onExisting)
			if p.outcome == outRejected || (p.outcome == outStatusOnly && !p.appliesStatus()) {
				next.count(r, p)
				continue
			}
			err := s.applyRecord(ctx, tx, r, ex, p, actor)
			var partial *statusOnlyError
			switch {
			case err == nil:
				next.count(r, p)
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.As(err, &partial):
				next.count(r, p.withoutDetails(s.applyReason(ctx, r, partial.err)))
			default:
				next.reject(r.line, s.applyReason(ctx, r, err))
			}
		}
		return saveCheckpoint(ctx, tx, jobID, checkpoint{Offset: offset, Stats: next})
	})
	if err != nil {
		return fmt.Errorf("imports: write batch: %w", err)
	}
	*st = next
	return nil
}

// applyReason is the rejection reason for a record whose write failed.
// Unexpected errors are logged (without the address) and reported
// generically, since their text could contain data from the row.
func (s *Service) applyReason(ctx context.Context, r *record, err error) string {
	switch {
	case errors.Is(err, core.ErrValidation):
		return reasonOf(err)
	case errors.Is(err, core.ErrSuppressed):
		return ReasonSuppressed
	case errors.Is(err, core.ErrForbidden):
		return "the status change was refused"
	}
	s.log.WarnContext(ctx, "imports: row not imported", "line", r.line, "err", err)
	return "the contact could not be written; see the server log"
}

// statusOnlyError reports a record whose blocking status was written
// while the rest of it (names, attributes, tags, consent) failed and was
// rolled back.
type statusOnlyError struct{ err error }

func (e *statusOnlyError) Error() string { return "only the status was imported: " + e.err.Error() }
func (e *statusOnlyError) Unwrap() error { return e.err }

// recordSavepoint makes each step of a record's writes all-or-nothing.
const recordSavepoint = "imports_record"

// inSavepoint runs fn inside a savepoint of tx and rolls back to it when
// fn fails, so the batch can go on without fn's writes.
func inSavepoint(ctx context.Context, tx *sql.Tx, fn func() error) error {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+recordSavepoint); err != nil {
		return fmt.Errorf("imports: savepoint: %w", err)
	}
	err := fn()
	bg := context.WithoutCancel(ctx)
	if err != nil {
		if _, rbErr := tx.ExecContext(bg, "ROLLBACK TO "+recordSavepoint); rbErr != nil {
			return errors.Join(err, fmt.Errorf("imports: roll back record: %w", rbErr))
		}
	}
	if _, relErr := tx.ExecContext(bg, "RELEASE "+recordSavepoint); relErr != nil {
		return errors.Join(err, fmt.Errorf("imports: release savepoint: %w", relErr))
	}
	return err
}

// applyRecord carries out p for r. A failed record leaves nothing behind,
// except that a status which blocks sending is kept when only the rest of
// the record fails: the status is written in a step of its own first (for
// a new contact: the contact is created again with only its address and
// status), and the error is then a *statusOnlyError. An import must never
// leave an address sendable that the file says has opted out (G-6).
func (s *Service) applyRecord(ctx context.Context, tx *sql.Tx, r *record, ex *existingContact, p plan, actor string) error {
	if p.create {
		err := inSavepoint(ctx, tx, func() error { return s.createContact(ctx, tx, r, p.finalStatus, false) })
		if err == nil || ctx.Err() != nil || r.statusOnly || !blocksSending(p.finalStatus) {
			return err
		}
		if err2 := inSavepoint(ctx, tx, func() error { return s.createContact(ctx, tx, r, p.finalStatus, true) }); err2 != nil {
			return err
		}
		return &statusOnlyError{err: err}
	}
	if p.setStatus != "" {
		if err := inSavepoint(ctx, tx, func() error {
			_, err := s.store.SetStatus(ctx, tx, ex.id, p.setStatus, contactstore.StatusChange{
				Source: contactstore.SourceImport, Actor: actor, Note: statusNote,
			})
			return err
		}); err != nil {
			return err
		}
	}
	err := inSavepoint(ctx, tx, func() error { return s.writeDetails(ctx, tx, r, ex, p) })
	if err != nil && p.setStatus != "" && ctx.Err() == nil {
		return &statusOnlyError{err: err}
	}
	return err
}

// createContact creates r's contact with status st and its import consent
// event, through contactstore so hooks fire with source "import".
// statusOnly leaves out names, attributes and tags.
func (s *Service) createContact(ctx context.Context, tx *sql.Tx, r *record, st core.ContactStatus, statusOnly bool) error {
	in := contactstore.CreateInput{Email: r.email, Status: st, Source: contactstore.SourceImport}
	if !statusOnly {
		in.FirstName, in.LastName, in.Attributes, in.Tags = r.firstName, r.lastName, anyMap(r.attrs), r.tags
	}
	c, err := s.store.Create(ctx, tx, in)
	if err != nil {
		return err
	}
	return s.store.RecordConsent(ctx, tx, contactstore.ConsentEvent{
		ContactID: c.ID, Type: contactstore.ConsentImport, Source: consentSource(r), OccurredAt: r.consentDate,
	})
}

// writeDetails writes an existing contact's names, attributes, tags and
// consent event (sequences ignore import-sourced tag triggers).
func (s *Service) writeDetails(ctx context.Context, tx *sql.Tx, r *record, ex *existingContact, p plan) error {
	if p.update != nil {
		if _, err := s.store.Update(ctx, tx, ex.id, *p.update, 0); err != nil {
			return err
		}
	}
	if len(p.addTags) > 0 {
		if err := s.store.AddTags(ctx, tx, ex.id, p.addTags, contactstore.SourceImport); err != nil {
			return err
		}
	}
	if p.consent {
		return s.recordConsentOnce(ctx, tx, ex.id, r)
	}
	return nil
}

// recordConsentOnce logs r's consent for an existing contact unless the
// same import consent is already in the log, so importing a file again
// doesn't repeat entries. Without a consent_date any import event with
// the same source counts as the same consent.
func (s *Service) recordConsentOnce(ctx context.Context, tx *sql.Tx, contactID int64, r *record) error {
	src := consentSource(r)
	q := `SELECT count(*) FROM consent_events WHERE contact_id = ? AND type = ? AND source = ?`
	args := []any{contactID, contactstore.ConsentImport, src}
	if !r.consentDate.IsZero() {
		q += ` AND occurred_at = ?`
		args = append(args, db.FormatTime(r.consentDate))
	}
	var n int
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return fmt.Errorf("imports: read consent log: %w", err)
	}
	if n > 0 {
		return nil
	}
	return s.store.RecordConsent(ctx, tx, contactstore.ConsentEvent{
		ContactID: contactID, Type: contactstore.ConsentImport, Source: src, OccurredAt: r.consentDate,
	})
}
