package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Contact rule thresholds (X-4): a contact becomes bounced after
// TransientBounceLimit transient (or undetermined) bounces within
// TransientBounceWindow.
const (
	TransientBounceLimit  = 3
	TransientBounceWindow = 30 * 24 * time.Hour
)

// events.type values written for SES events.
const (
	EventDelivery    = "delivery"
	EventBounce      = "bounce"
	EventComplaint   = "complaint"
	EventReject      = "reject"
	EventUnsubscribe = "unsubscribe"
)

// statusActor is the contact_status_changes.actor of SES-driven changes.
const statusActor = "system"

// eventTypes maps SES event kinds to events.type.
var eventTypes = map[string]string{
	SESBounce:    EventBounce,
	SESComplaint: EventComplaint,
	SESDelivery:  EventDelivery,
	SESReject:    EventReject,
}

// processNotification applies the SES event carried by a verified SNS
// Notification in one transaction. Ignored event types return nil.
func (s *Service) processNotification(ctx context.Context, env Envelope) error {
	ev, ok, err := parseSESEvent(env.Message)
	if err != nil {
		return err
	}
	if !ok {
		s.log.Debug("events: ignored SNS notification", "message_id", env.MessageID)
		return nil
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		return s.applyEvent(ctx, tx, env.MessageID, env.Message, ev)
	})
}

// messageRow is the part of a messages row event processing needs.
type messageRow struct {
	ID        int64
	ContactID sql.NullInt64
	ToEmail   sql.NullString
}

// erased reports whether the message's contact was GDPR-erased: erase
// NULLs both contact_id and to_email. (Test and alert messages have no
// contact but keep their address.)
func (m messageRow) erased() bool { return !m.ContactID.Valid && !m.ToEmail.Valid }

// findMessage returns the message with the given SES MessageId.
func findMessage(ctx context.Context, tx *sql.Tx, sesID string) (messageRow, bool, error) {
	var m messageRow
	err := tx.QueryRowContext(ctx, `SELECT id, contact_id, to_email FROM messages WHERE ses_message_id = ?`, sesID).
		Scan(&m.ID, &m.ContactID, &m.ToEmail)
	if errors.Is(err, sql.ErrNoRows) {
		return messageRow{}, false, nil
	}
	if err != nil {
		return messageRow{}, false, fmt.Errorf("events: find message: %w", err)
	}
	return m, true, nil
}

// applyEvent records one event row per recipient and applies the message
// and contact rules (X-3..X-6). Recipients whose row already exists (a
// repeated SNS delivery) are skipped entirely. A fresh event for a message
// not recorded yet is deferred once (errRetryLater, nothing written).
func (s *Service) applyEvent(ctx context.Context, tx *sql.Tx, snsID, raw string, ev parsedEvent) error {
	msg, known, err := findMessage(ctx, tx, ev.MessageID)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	if !known && s.shouldDefer(snsID, ev, now) {
		return errRetryLater
	}
	recorded := false
	for _, rcpt := range ev.Recipients {
		norm := normalizeRecipient(rcpt)
		contactID, erased, err := s.resolveContact(ctx, tx, msg, known, norm, len(ev.Recipients))
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO events (message_id, type, subtype, provider_event_id, detail, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (provider_event_id) DO NOTHING`,
			db.NullID(msg.ID), eventTypes[ev.Kind], ev.SubType, providerEventID(snsID, norm),
			storedDetail(raw, ev, msg, known, contactID, erased), db.FormatTime(now))
		if err != nil {
			return fmt.Errorf("events: insert %s event: %w", eventTypes[ev.Kind], err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("events: insert event: %w", err)
		} else if n == 0 {
			continue // duplicate delivery (X-6)
		}
		recorded = true
		if contactID != 0 && !ev.notSpam() {
			if err := s.applyContactRule(ctx, tx, ev, contactID, msg.ID, norm, now); err != nil {
				return err
			}
		}
	}
	if recorded && known && !ev.notSpam() {
		return s.updateMessage(ctx, tx, msg.ID, ev, now)
	}
	return nil
}

// storedDetail returns events.detail for one recipient row. The raw SES
// JSON contains recipient addresses, so it is kept only where a GDPR erase
// will find it again: on a message of a contact, for that contact's own
// address (or an address that is no contact). Rows of erased contacts get
// '{}'. Every other row (unknown message, a test or alert message without
// a contact, another contact's address) gets an address-free summary.
// The raw JSON is stored without the message's headers (withoutHeaders).
func storedDetail(raw string, ev parsedEvent, msg messageRow, known bool, contactID int64, erased bool) string {
	switch {
	case erased:
		return "{}"
	case known && msg.ContactID.Valid && (contactID == 0 || contactID == msg.ContactID.Int64):
		return withoutHeaders(raw, ev)
	default:
		return ev.summary()
	}
}

// withoutHeaders returns the SES event JSON without mail.headers and
// mail.commonHeaders. SES copies the original message headers into every
// event, and they include List-Unsubscribe with the recipient's working
// one-click unsubscribe URL: stored as is, anyone who can read events (a
// read-scope token) could unsubscribe every mailed contact. If the JSON
// can't be rewritten, the address-free summary is stored instead.
func withoutHeaders(raw string, ev parsedEvent) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return ev.summary()
	}
	m, ok := top["mail"]
	if !ok {
		return raw
	}
	var mailObj map[string]json.RawMessage
	if err := json.Unmarshal(m, &mailObj); err != nil {
		return ev.summary()
	}
	_, h := mailObj["headers"]
	_, ch := mailObj["commonHeaders"]
	if !h && !ch {
		return raw
	}
	delete(mailObj, "headers")
	delete(mailObj, "commonHeaders")
	delete(mailObj, "headersTruncated")
	b, err := json.Marshal(mailObj)
	if err != nil {
		return ev.summary()
	}
	top["mail"] = b
	out, err := json.Marshal(top)
	if err != nil {
		return ev.summary()
	}
	return string(out)
}

// resolveContact returns the contact a recipient's rules apply to (0 for
// none) and whether the recipient belongs to an erased contact. The
// message's own contact is used when the recipient is its address (or the
// only one reported); otherwise the contact with that address is looked up,
// which also covers events for unknown messages.
func (s *Service) resolveContact(ctx context.Context, tx *sql.Tx, msg messageRow, known bool, norm string, recipients int) (int64, bool, error) {
	if known && msg.erased() {
		return 0, true, nil
	}
	if known && msg.ContactID.Valid && (recipients == 1 || normalizeRecipient(msg.ToEmail.String) == norm) {
		return msg.ContactID.Int64, false, nil
	}
	if norm == "" {
		return 0, false, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM contacts WHERE email = ?`, norm).Scan(&id)
	switch {
	case err == nil:
		return id, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, fmt.Errorf("events: find contact: %w", err)
	}
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM suppressions WHERE email_hash = ? AND reason = 'erased'`, core.EmailHash(norm)).Scan(&one)
	switch {
	case err == nil:
		return 0, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("events: check erased address: %w", err)
	}
}

// applyContactRule applies X-4 to a contact: a permanent bounce, or the
// third transient bounce within 30 days, makes it bounced; a complaint
// makes it complained (callers skip "not-spam" feedback).
func (s *Service) applyContactRule(ctx context.Context, tx *sql.Tx, ev parsedEvent, contactID, messageID int64, norm string, now time.Time) error {
	switch ev.Kind {
	case SESBounce:
		if ev.BounceType == BouncePermanent {
			return s.setStatus(ctx, tx, contactID, core.StatusBounced, contactstore.StatusChange{
				Source: contactstore.SourceBounce, Actor: statusActor, Note: "permanent bounce", MessageID: messageID,
			})
		}
		n, err := transientBounces(ctx, tx, norm, now)
		if err != nil {
			return err
		}
		if n < TransientBounceLimit {
			return nil
		}
		return s.setStatus(ctx, tx, contactID, core.StatusBounced, contactstore.StatusChange{
			Source: contactstore.SourceBounce, Actor: statusActor, MessageID: messageID,
			Note: fmt.Sprintf("%d transient bounces within 30 days", n),
		})
	case SESComplaint:
		return s.setStatus(ctx, tx, contactID, core.StatusComplained, contactstore.StatusChange{
			Source: contactstore.SourceComplaint, Actor: statusActor, MessageID: messageID,
		})
	}
	return nil
}

// transientBounces counts the transient and undetermined bounce events of
// an address within TransientBounceWindow, by the recipient hash in
// provider_event_id (so events of unknown messages count too).
func transientBounces(ctx context.Context, tx *sql.Tx, norm string, now time.Time) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM events
		WHERE type = 'bounce' AND created_at >= ? AND subtype IN ('transient', 'undetermined')
		  AND provider_event_id LIKE '%:' || ?`,
		db.FormatTime(now.Add(-TransientBounceWindow)), core.EmailHash(norm)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("events: count transient bounces: %w", err)
	}
	return n, nil
}

// setStatus changes a contact's status. Transitions the status rules
// refuse (e.g. a bounce over a complaint) and vanished contacts are not
// errors: the contact already can't receive marketing email.
func (s *Service) setStatus(ctx context.Context, tx *sql.Tx, contactID int64, to core.ContactStatus, ch contactstore.StatusChange) error {
	_, err := s.store.SetStatus(ctx, tx, contactID, to, ch)
	if errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrNotFound) {
		s.log.Debug("events: status change not applied", "contact_id", contactID, "to", to, "err", err)
		return nil
	}
	return err
}

// updateMessage moves the message's status forward, never back:
// complained > bounced > delivered > sent. Only a permanent bounce makes a
// message bounced ("hard bounce" in the broadcast and sequence stats); a
// transient or undetermined bounce is kept as its events row only. A
// reject fails a sent message.
func (s *Service) updateMessage(ctx context.Context, tx *sql.Tx, id int64, ev parsedEvent, now time.Time) error {
	ts := db.FormatTime(now)
	var (
		query string
		args  []any
	)
	switch ev.Kind {
	case SESDelivery:
		at := ev.DeliveredAt
		if at.IsZero() {
			at = now
		}
		query = `UPDATE messages SET delivered_at = COALESCE(delivered_at, ?),
			status = CASE WHEN status IN ('sending', 'sent') THEN 'delivered' ELSE status END,
			updated_at = ? WHERE id = ?`
		args = []any{db.FormatTime(at), ts, id}
	case SESBounce:
		if !ev.hardBounce() {
			return nil
		}
		query = `UPDATE messages SET status = 'bounced', updated_at = ?
			WHERE id = ? AND status IN ('sending', 'sent', 'delivered')`
		args = []any{ts, id}
	case SESComplaint:
		query = `UPDATE messages SET status = 'complained', updated_at = ?
			WHERE id = ? AND status IN ('sending', 'sent', 'delivered', 'bounced')`
		args = []any{ts, id}
	case SESReject:
		query = `UPDATE messages SET status = 'failed', error = 'rejected by SES', updated_at = ?
			WHERE id = ? AND status IN ('sending', 'sent')`
		args = []any{ts, id}
	default:
		return nil
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("events: update message %d: %w", id, err)
	}
	return nil
}

// normalizeRecipient returns the normalised form of an address SES
// reports. Addresses NormalizeEmail rejects (e.g. with a display name) are
// parsed with net/mail first, then lowercased as a last resort, so the
// hash is always defined.
func normalizeRecipient(r string) string {
	r = strings.TrimSpace(r)
	if r == "" {
		return ""
	}
	if n, err := core.NormalizeEmail(r); err == nil {
		return n
	}
	if a, err := mail.ParseAddress(r); err == nil {
		if n, err := core.NormalizeEmail(a.Address); err == nil {
			return n
		}
		return strings.ToLower(strings.TrimSpace(a.Address))
	}
	return strings.ToLower(r)
}

// providerEventID is the idempotency key of one recipient of an SES event:
// "<SNS MessageId>:<sha256 hex of the normalised recipient>".
func providerEventID(snsID, normalized string) string {
	return snsID + ":" + core.EmailHash(normalized)
}
