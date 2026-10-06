package sending

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mailroom/internal/core"
)

// Skip reasons stored in messages.error when a message is skipped.
const (
	SkipContactErased     = "contact erased"
	SkipContactDeleted    = "contact deleted"
	SkipSuppressed        = "address suppressed"
	SkipNoAddress         = "recipient address erased"
	SkipEnrollmentExited  = "enrollment exited"
	SkipEnrollmentRemoved = "enrollment removed"
	SkipUnknownKind       = "unknown message kind"
)

// eligibility is the final send-time check (D-6, ARCHITECTURE.md §6):
//
//   - broadcast, sequence, optin: the contact must still exist, its status
//     must allow the kind (core.CanReceive) and its address must not be in
//     the suppressions table. Sequence messages are also skipped when their
//     enrollment has exited (unsubscribed, exit tag, manual unenrol) or is
//     gone. A completed enrollment is fine: the scheduler completes it in
//     the same transaction that queues the last step;
//   - test: the address must not be in the suppressions table, and the
//     contact with that address, if there is one, must be active or
//     pending (core.CanReceive): a test never reaches an address that
//     unsubscribed, bounced, complained or was suppressed (PRD goal 3);
//   - alert: never suppressed.
//
// It returns the contact for contact messages (and for tests tied to a
// contact, for merge fields) and a skip reason, "" when the message may
// be sent. Reads use the read pool, so they see only committed changes.
func (s *Service) eligibility(ctx context.Context, m message) (*core.Contact, string, error) {
	q := s.db.Read
	switch m.Kind {
	case core.KindAlert:
		return nil, "", nil
	case core.KindTest:
		if m.ToEmail == "" {
			return nil, SkipNoAddress, nil
		}
		sup, err := s.contacts.IsSuppressed(ctx, q, m.ToEmail)
		if err != nil || sup {
			return nil, reasonIf(sup, SkipSuppressed), err
		}
		recipient, err := s.contacts.GetByEmail(ctx, q, m.ToEmail)
		switch {
		case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrValidation):
			// No contact has the address.
		case err != nil:
			return nil, "", err
		case !core.CanReceive(recipient.Status, m.Kind):
			return nil, "contact status is " + string(recipient.Status), nil
		}
		if m.ContactID == 0 {
			return nil, "", nil
		}
		c, err := s.contacts.Get(ctx, q, m.ContactID)
		if errors.Is(err, core.ErrNotFound) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		return &c, "", nil
	case core.KindBroadcast, core.KindSequence, core.KindOptin:
	default:
		return nil, SkipUnknownKind, nil
	}

	if m.ContactID == 0 {
		return nil, SkipContactErased, nil
	}
	c, err := s.contacts.Get(ctx, q, m.ContactID)
	if errors.Is(err, core.ErrNotFound) {
		return nil, SkipContactDeleted, nil
	}
	if err != nil {
		return nil, "", err
	}
	if !core.CanReceive(c.Status, m.Kind) {
		return nil, "contact status is " + string(c.Status), nil
	}
	sup, err := s.contacts.IsSuppressed(ctx, q, c.Email)
	if err != nil || sup {
		return nil, reasonIf(sup, SkipSuppressed), err
	}
	if m.Kind == core.KindSequence {
		if m.EnrollmentID == 0 {
			return nil, SkipEnrollmentRemoved, nil
		}
		var status string
		err := q.QueryRowContext(ctx, `SELECT status FROM enrollments WHERE id = ?`, m.EnrollmentID).Scan(&status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, SkipEnrollmentRemoved, nil
		case err != nil:
			return nil, "", fmt.Errorf("sending: enrollment %d: %w", m.EnrollmentID, err)
		case status == "exited":
			return nil, SkipEnrollmentExited, nil
		}
	}
	return &c, "", nil
}

func reasonIf(cond bool, reason string) string {
	if cond {
		return reason
	}
	return ""
}
