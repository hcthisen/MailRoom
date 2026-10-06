package events

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/tokens"
)

// Unsubscribe form values.
const (
	// ScopeAll unsubscribes the contact from everything.
	ScopeAll = "all"
	// ScopeSequence exits only the enrollment the message belongs to.
	ScopeSequence = "sequence"
	// ExitUnsubscribedSequence is enrollments.exit_reason for ScopeSequence.
	ExitUnsubscribedSequence = "unsubscribed_sequence"
)

// maxFormBytes bounds an unsubscribe POST body.
const maxFormBytes = 64 << 10

// publicActor is the actor of changes made through public links.
const publicActor = core.ActorPublic

// unsubTarget is what an unsubscribe token points at.
type unsubTarget struct {
	ContactID int64
	Exists    bool
	Status    core.ContactStatus
	// OptOutRecorded reports that the contact's unsubscribe request is
	// already recorded (contacts.unsubscribe_requested_at).
	OptOutRecorded bool
	// MessageID is the token's message when it exists (it then always
	// belongs to the contact, see loadTarget), else 0.
	MessageID int64
	// EnrollmentID is the enrollment of a sequence message, else 0.
	EnrollmentID int64
	// EnrollmentOpen reports whether that enrollment can still be stopped:
	// it is active, or completed (its last step may still be queued; the
	// scheduler completes an enrollment when it queues the last step).
	EnrollmentOpen bool
}

// loadTarget resolves a verified unsubscribe token payload with q. A
// token acts on its contact only while its message still belongs to that
// contact (U-1): after a GDPR erase the message loses its contact and the
// token is stale (Exists false), even if a new contact got the old id.
// When the message row itself is gone (e.g. its broadcast was deleted) or
// the token names no message, the token's contact is used.
func loadTarget(ctx context.Context, q db.Querier, pl tokens.Payload) (unsubTarget, error) {
	t := unsubTarget{ContactID: pl.ContactID}
	var (
		status string
		optOut sql.NullString
	)
	err := q.QueryRowContext(ctx, `SELECT status, unsubscribe_requested_at FROM contacts WHERE id = ?`, pl.ContactID).
		Scan(&status, &optOut)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return t, nil
	case err != nil:
		return t, fmt.Errorf("events: load contact: %w", err)
	}
	t.Exists, t.Status, t.OptOutRecorded = true, core.ContactStatus(status), optOut.Valid
	if pl.MessageID == 0 {
		return t, nil
	}
	var (
		kind       string
		contactID  sql.NullInt64
		enrollment sql.NullInt64
	)
	err = q.QueryRowContext(ctx, `SELECT kind, contact_id, enrollment_id FROM messages WHERE id = ?`, pl.MessageID).
		Scan(&kind, &contactID, &enrollment)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return t, nil
	case err != nil:
		return t, fmt.Errorf("events: load message: %w", err)
	}
	if !contactID.Valid || contactID.Int64 != pl.ContactID {
		// The message no longer belongs to the token's contact: that
		// contact was erased and its id may have been reused by a newer
		// contact (contacts.id is a plain rowid). The token is stale and
		// must not act on anyone.
		return unsubTarget{ContactID: pl.ContactID}, nil
	}
	t.MessageID = pl.MessageID
	if core.MessageKind(kind) != core.KindSequence || !enrollment.Valid {
		return t, nil
	}
	t.EnrollmentID = enrollment.Int64
	var est string
	err = q.QueryRowContext(ctx, `SELECT status FROM enrollments WHERE id = ? AND contact_id = ?`, t.EnrollmentID, pl.ContactID).Scan(&est)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		t.EnrollmentID = 0
	case err != nil:
		return t, fmt.Errorf("events: load enrollment: %w", err)
	default:
		t.EnrollmentOpen = est == "active" || est == "completed"
	}
	return t, nil
}

// canUnsubscribe reports whether the contact still receives marketing
// email that an unsubscribe would stop.
func (t unsubTarget) canUnsubscribe() bool {
	return t.Exists && (t.Status == core.StatusActive || t.Status == core.StatusPending)
}

// canRecordOptOut reports whether an unsubscribe would record a request
// that isn't recorded yet (contactstore.RecordOptOut): the contact gets no
// marketing email now because it bounced, complained or is suppressed,
// but lifting that later must not resume it.
func (t unsubTarget) canRecordOptOut() bool {
	if !t.Exists || t.OptOutRecorded {
		return false
	}
	switch t.Status {
	case core.StatusBounced, core.StatusComplained, core.StatusSuppressedManual:
		return true
	}
	return false
}

// verifyUnsub checks an unsubscribe token.
func (s *Service) verifyUnsub(token string) (tokens.Payload, bool) {
	pl, err := s.signer.Verify(tokens.PurposeUnsubscribe, token)
	if err != nil || pl.ContactID <= 0 {
		return tokens.Payload{}, false
	}
	return pl, true
}

// handleUnsubscribePage serves GET /u/{token} (U-2): the confirmation page
// with one "Unsubscribe" button and, for a sequence enrollment that is
// active or completed, "Only stop these emails". A bounced, complained or
// suppressed contact whose request isn't recorded yet gets the button
// alone, so the link path reaches the POST that records it (U-3); the GET
// itself never changes anything (link scanners prefetch it). Already
// unsubscribed contacts see the result page; invalid tokens a generic 404
// page.
func (s *Service) handleUnsubscribePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	company := companyName(s.appSettings(ctx), s.cfg.Domain)
	pl, ok := s.verifyUnsub(r.PathValue("token"))
	if !ok {
		s.renderPage(w, http.StatusNotFound, notFoundPage())
		return
	}
	t, err := loadTarget(ctx, s.db.Read, pl)
	if err != nil {
		s.log.Error("events: unsubscribe page", "err", err)
		s.renderPage(w, http.StatusInternalServerError, errorPage(company))
		return
	}
	switch {
	case t.canUnsubscribe():
		s.renderPage(w, http.StatusOK, confirmPage(company, t.EnrollmentOpen))
	case t.canRecordOptOut():
		s.renderPage(w, http.StatusOK, confirmPage(company, false))
	default:
		s.renderPage(w, http.StatusOK, unsubscribedPage(company))
	}
}

// handleUnsubscribe serves POST /u/{token} (U-3): RFC 8058 one-click
// ("List-Unsubscribe=One-Click", no confirmation) and the page's form.
// It always answers 200 for a valid token, also when nothing changes.
func (s *Service) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	ctx := core.WithActor(r.Context(), core.Actor{Type: core.ActorPublic})
	company := companyName(s.appSettings(ctx), s.cfg.Domain)
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxFormBytes))
	pl, ok := s.verifyUnsub(r.PathValue("token"))
	if !ok {
		s.renderPage(w, http.StatusNotFound, notFoundPage())
		return
	}
	form := parseForm(r.Header.Get("Content-Type"), body)
	source, scope := contactstore.SourceLink, form.Get("scope")
	if form.Get("List-Unsubscribe") == "One-Click" {
		source, scope = contactstore.SourceOneClick, ScopeAll
	}
	var result page
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.unsubscribe(ctx, tx, pl, scope, source, company)
		return err
	})
	if err != nil {
		s.log.Error("events: unsubscribe", "err", err)
		s.renderPage(w, http.StatusInternalServerError, errorPage(company))
		return
	}
	s.renderPage(w, http.StatusOK, result)
}

// unsubscribe applies an unsubscribe inside tx and returns the result
// page. ScopeSequence on a sequence message exits only its enrollment
// (active or completed, so a queued last step is skipped too); everything
// else sets the contact unsubscribed, which exits all its sequences
// through the StatusChanged hook. A contact that already receives no
// marketing email because it bounced, complained or is suppressed keeps
// its status, but the request is recorded (contactstore.RecordOptOut), so
// lifting the bounce or the suppression later lands on unsubscribed. An
// events row (type unsubscribe, subtype link|one_click) is written
// whenever something was recorded, and only then, so repeats are harmless
// (U-4).
func (s *Service) unsubscribe(ctx context.Context, tx *sql.Tx, pl tokens.Payload, scope, source, company string) (page, error) {
	t, err := loadTarget(ctx, tx, pl)
	if err != nil {
		return page{}, err
	}
	if !t.Exists {
		return unsubscribedPage(company), nil
	}
	if scope == ScopeSequence && t.EnrollmentID != 0 {
		if !t.canUnsubscribe() {
			return unsubscribedPage(company), nil
		}
		exited, err := s.exitEnrollment(ctx, tx, t)
		if err != nil {
			return page{}, err
		}
		if exited {
			if err := s.recordUnsubscribe(ctx, tx, t.MessageID, source,
				fmt.Sprintf(`{"scope":"sequence","enrollment_id":%d}`, t.EnrollmentID)); err != nil {
				return page{}, err
			}
		}
		return sequenceStoppedPage(company), nil
	}
	changed, err := s.store.SetStatus(ctx, tx, t.ContactID, core.StatusUnsubscribed, contactstore.StatusChange{
		Source: source, Actor: publicActor, MessageID: t.MessageID,
	})
	if errors.Is(err, core.ErrNotFound) {
		return unsubscribedPage(company), nil
	}
	if errors.Is(err, core.ErrForbidden) {
		// bounced, complained or suppressed_manual: the contact gets no
		// marketing email now; remember the request for a reactivation.
		recorded, err := s.store.RecordOptOut(ctx, tx, t.ContactID)
		if err != nil {
			return page{}, err
		}
		if recorded {
			detail, err := json.Marshal(map[string]string{"scope": ScopeAll, "status_kept": string(t.Status)})
			if err != nil {
				return page{}, fmt.Errorf("events: unsubscribe detail: %w", err)
			}
			if err := s.recordUnsubscribe(ctx, tx, t.MessageID, source, string(detail)); err != nil {
				return page{}, err
			}
		}
		return unsubscribedPage(company), nil
	}
	if err != nil {
		return page{}, err
	}
	if changed {
		if err := s.recordUnsubscribe(ctx, tx, t.MessageID, source, `{"scope":"all"}`); err != nil {
			return page{}, err
		}
	}
	return unsubscribedPage(company), nil
}

// exitEnrollment ends one active or completed enrollment with reason
// ExitUnsubscribedSequence. It writes enrollments directly (see the
// package doc). The sender skips queued messages of exited enrollments,
// which covers the last step of a completed one that is still queued, and
// the sequence never enrols the contact again (sequences.enrollBlocker).
func (s *Service) exitEnrollment(ctx context.Context, tx *sql.Tx, t unsubTarget) (bool, error) {
	now := s.now()
	res, err := tx.ExecContext(ctx, `
		UPDATE enrollments SET status = 'exited', exit_reason = ?, next_run_at = NULL, finished_at = ?, updated_at = ?
		WHERE id = ? AND contact_id = ? AND status IN ('active', 'completed')`,
		ExitUnsubscribedSequence, now, now, t.EnrollmentID, t.ContactID)
	if err != nil {
		return false, fmt.Errorf("events: exit enrollment %d: %w", t.EnrollmentID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("events: exit enrollment %d: %w", t.EnrollmentID, err)
	}
	return n > 0, nil
}

// recordUnsubscribe writes the unsubscribe events row.
func (s *Service) recordUnsubscribe(ctx context.Context, tx *sql.Tx, messageID int64, source, detail string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events (message_id, type, subtype, detail, created_at) VALUES (?, ?, ?, ?, ?)`,
		db.NullID(messageID), EventUnsubscribe, source, detail, s.now()); err != nil {
		return fmt.Errorf("events: record unsubscribe: %w", err)
	}
	return nil
}

// parseForm decodes an unsubscribe POST body: multipart/form-data, or
// otherwise URL-encoded pairs whatever the declared type (some one-click
// senders use text/plain or none). Undecodable bodies yield no values.
func parseForm(contentType string, body []byte) url.Values {
	mt, params, _ := mime.ParseMediaType(contentType)
	if mt == "multipart/form-data" {
		f, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(maxFormBytes)
		if err != nil {
			return url.Values{}
		}
		defer func() { _ = f.RemoveAll() }()
		return url.Values(f.Value)
	}
	v, _ := url.ParseQuery(strings.TrimSpace(string(body)))
	return v
}
