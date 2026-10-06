package contactstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/hooks"
)

// Status change sources (contact_status_changes.source).
const (
	SourceLink      = "link"      // unsubscribe page button
	SourceOneClick  = "one_click" // RFC 8058 POST
	SourceAdmin     = "admin"     // admin UI
	SourceAPI       = "api"       // REST API
	SourceImport    = "import"    // CSV import
	SourceBounce    = "bounce"    // SES bounce event
	SourceComplaint = "complaint" // SES complaint event
	SourceForm      = "form"      // form submission
	SourceOptin     = "optin"     // confirmed (re)subscribe
	SourceSystem    = "system"    // internal rules
)

func validSource(s string) bool {
	switch s {
	case SourceLink, SourceOneClick, SourceAdmin, SourceAPI, SourceImport,
		SourceBounce, SourceComplaint, SourceForm, SourceOptin, SourceSystem:
		return true
	}
	return false
}

// StatusChange describes why a status changes.
type StatusChange struct {
	Source    string // one of the Source* constants
	Actor     string // e.g. "user:1", "token:3", "public"
	Note      string // required for some admin transitions
	MessageID int64  // the message that caused it (unsubscribe, bounce); 0 = none
}

// CanTransition reports whether a contact may move from one status to
// another, given the change's source and note (ARCHITECTURE.md §6). It
// returns nil when allowed and an error wrapping core.ErrForbidden when not.
// from == to is always allowed (a no-op).
//
// Moves to active:
//
//   - pending → active needs a confirmed opt-in (source optin), or an admin
//     or API caller with a non-empty note (a consent confirmed elsewhere);
//     a double opt-in can't be skipped by accident (F-6).
//   - unsubscribed → active needs source optin (a confirmed resubscribe),
//     or admin/api with a note.
//   - bounced → active needs admin/api with a note.
//   - suppressed_manual → active needs admin or api.
//   - complained → active is never allowed.
//
// pending may move to any non-active status, and active to any status but
// pending. Moves between the non-active statuses follow one principle:
// they may only make the contact harder to reactivate, never easier. So
// bounced and complained (facts reported by SES) can be recorded over
// unsubscribed and suppressed_manual, complained can be recorded over
// bounced, and complained is final. Everything else is refused, e.g.
// bounced → unsubscribed (it would allow a self-service resubscribe) and
// unsubscribed/bounced → suppressed_manual (it would make reactivation
// need no note). To block such an address manually, add it to the
// suppressions table instead; its status already stops all sending.
func CanTransition(from, to core.ContactStatus, source, note string) error {
	if from == to {
		return nil
	}
	deny := func(why string) error {
		return fmt.Errorf("status change %s → %s via %s: %s: %w", from, to, source, why, core.ErrForbidden)
	}
	hasNote := strings.TrimSpace(note) != ""
	switch {
	case to == core.StatusPending:
		return deny("a contact can never return to pending")
	case to == core.StatusActive:
		manual := (source == SourceAdmin || source == SourceAPI) && hasNote
		switch from {
		case core.StatusPending:
			if source == SourceOptin || manual {
				return nil
			}
			return deny("activating a pending contact needs a confirmed opt-in, or an admin or API caller with a note")
		case core.StatusUnsubscribed:
			if source == SourceOptin || manual {
				return nil
			}
			return deny("resubscribing needs a confirmed opt-in, or an admin or API caller with a note")
		case core.StatusBounced:
			if manual {
				return nil
			}
			return deny("reactivating a bounced contact needs an admin or API caller with a note")
		case core.StatusComplained:
			return deny("a contact who complained can never be reactivated")
		case core.StatusSuppressedManual:
			if source == SourceAdmin || source == SourceAPI {
				return nil
			}
			return deny("only an admin or the API can lift a manual suppression")
		}
	case from == core.StatusPending || from == core.StatusActive:
		return nil // to a non-active status
	case from == core.StatusComplained:
		return deny("complained is final")
	case to == core.StatusComplained:
		return nil // from unsubscribed, bounced or suppressed_manual
	case to == core.StatusBounced && (from == core.StatusUnsubscribed || from == core.StatusSuppressedManual):
		return nil
	}
	return deny("this would make the contact easier to reactivate")
}

// setStatus implements SetStatus.
func (s *Store) setStatus(ctx context.Context, tx *sql.Tx, contactID int64, to core.ContactStatus, ch StatusChange) (changed bool, err error) {
	if !to.Valid() {
		return false, core.Invalid("status", "unknown status %q", to)
	}
	if !validSource(ch.Source) {
		return false, core.Invalid("source", "unknown status change source %q", ch.Source)
	}
	_, from, err := currentState(ctx, tx, contactID)
	if err != nil {
		return false, err
	}
	if from == to {
		return false, nil
	}
	if err := CanTransition(from, to, ch.Source, ch.Note); err != nil {
		return false, err
	}
	note := strings.TrimSpace(ch.Note)
	if to == core.StatusActive && (from == core.StatusBounced || from == core.StatusSuppressedManual) {
		// The contact asked to unsubscribe while bounced or suppressed
		// (RecordOptOut): lifting the bounce or the suppression must not
		// resume marketing email to someone who opted out (U-3).
		var requested sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT unsubscribe_requested_at FROM contacts WHERE id = ?`, contactID).
			Scan(&requested); err != nil {
			return false, fmt.Errorf("read contact %d: %w", contactID, err)
		}
		if requested.Valid {
			to = core.StatusUnsubscribed
			note = strings.TrimSpace(note + " " + fmt.Sprintf(OptOutNote, requested.String))
		}
	}

	now := db.FormatTime(s.Clock.Now())
	if _, err := tx.ExecContext(ctx, `
		UPDATE contacts SET status = ?1, status_changed_at = ?2, updated_at = ?2, version = version + 1,
		       unsubscribe_requested_at = CASE
		           WHEN ?1 IN ('active', 'unsubscribed') THEN NULL
		           -- unsubscribed → bounced/complained: keep the opt-out, so a
		           -- later reactivation lands on unsubscribed (U-3).
		           WHEN status = 'unsubscribed' THEN COALESCE(unsubscribe_requested_at, status_changed_at)
		           ELSE unsubscribe_requested_at END
		WHERE id = ?3`, string(to), now, contactID); err != nil {
		return false, fmt.Errorf("set contact %d status: %w", contactID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contact_status_changes (contact_id, from_status, to_status, source, message_id, actor, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		contactID, string(from), string(to), ch.Source, db.NullID(ch.MessageID), ch.Actor, note, now); err != nil {
		return false, fmt.Errorf("record status change for contact %d: %w", contactID, err)
	}
	if err := s.syncManualSuppression(ctx, tx, contactID, from, to, ch.Note, now); err != nil {
		return false, err
	}
	if err := s.Bus.PublishStatusChanged(ctx, tx, hooks.StatusChanged{
		ContactID: contactID, From: from, To: to, Source: ch.Source, MessageID: ch.MessageID,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// syncManualSuppression keeps the suppressions table in step with the
// suppressed_manual status for every caller: moving a contact to
// suppressed_manual adds its address's hash with reason manual (unless the
// address is already suppressed), and lifting it (suppressed_manual →
// active, or → unsubscribed after an opt-out, see setStatus) removes that
// manual row. Erased rows are never touched, and a
// manual row stays when SES facts (bounced, complained) are recorded over
// suppressed_manual.
func (s *Store) syncManualSuppression(ctx context.Context, tx *sql.Tx, contactID int64, from, to core.ContactStatus, note, now string) error {
	var add bool
	switch {
	case to == core.StatusSuppressedManual:
		add = true
	case from == core.StatusSuppressedManual && (to == core.StatusActive || to == core.StatusUnsubscribed):
		// Lifting the suppression; unsubscribed when the contact had asked to
		// unsubscribe meanwhile (see setStatus).
	default:
		return nil
	}
	var email string
	if err := tx.QueryRowContext(ctx, `SELECT email FROM contacts WHERE id = ?`, contactID).Scan(&email); err != nil {
		return fmt.Errorf("read contact %d: %w", contactID, err)
	}
	return syncSuppressionRow(ctx, tx, core.EmailHash(email), add, note, now)
}

// syncSuppressionRow adds (add) or removes the manual suppression row of
// the address with this hash.
func syncSuppressionRow(ctx context.Context, tx *sql.Tx, hash string, add bool, note, now string) error {
	if add {
		if _, err := tx.ExecContext(ctx, `INSERT INTO suppressions (email_hash, reason, note, created_at)
			VALUES (?, 'manual', ?, ?) ON CONFLICT (email_hash) DO NOTHING`, hash, strings.TrimSpace(note), now); err != nil {
			return fmt.Errorf("add manual suppression: %w", err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM suppressions WHERE email_hash = ? AND reason = 'manual'`, hash); err != nil {
		return fmt.Errorf("remove manual suppression: %w", err)
	}
	return nil
}

// OptOutNote is appended (with the time of the request) to the note of a
// reactivation that lands on unsubscribed because of RecordOptOut.
const OptOutNote = "(kept unsubscribed: the contact asked to unsubscribe on %s)"

// RecordOptOut records an unsubscribe request (link, one-click, or an
// admin or API caller passing on a request received elsewhere) from a
// contact whose status can't become unsubscribed because it already
// receives no marketing email: bounced, complained or suppressed_manual
// (CanTransition refuses those moves, since they would make reactivation
// easier). The status stays, but a later move to active, i.e. an admin or
// API caller lifting the bounce or the manual suppression, lands on
// unsubscribed instead, so the opt-out is never lost (U-3). setStatus
// records the same marker when an unsubscribed contact moves to bounced or
// complained, with the time it unsubscribed. recorded is
// false when the contact is missing, has another status, or already has a
// request recorded (the first one is kept). It bumps the contact's version.
func (s *Store) RecordOptOut(ctx context.Context, tx *sql.Tx, contactID int64) (recorded bool, err error) {
	now := db.FormatTime(s.Clock.Now())
	res, err := tx.ExecContext(ctx, `
		UPDATE contacts SET unsubscribe_requested_at = ?1, updated_at = ?1, version = version + 1
		WHERE id = ?2 AND unsubscribe_requested_at IS NULL AND status IN ('bounced', 'complained', 'suppressed_manual')`,
		now, contactID)
	if err != nil {
		return false, fmt.Errorf("record opt-out of contact %d: %w", contactID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record opt-out of contact %d: %w", contactID, err)
	}
	return n > 0, nil
}
