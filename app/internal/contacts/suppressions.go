package contacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Suppression reasons (suppressions.reason).
const (
	ReasonErased = "erased"
	ReasonManual = "manual"
)

// Suppression is an address that must never be (re-)added or emailed. Only
// the SHA-256 of the normalised address is stored.
type Suppression struct {
	ID        int64     `json:"id" doc:"Row ID, used for pagination" example:"5"`
	EmailHash string    `json:"email_hash" doc:"SHA-256 hex of the normalised address" example:"ff8d9819fc0e12bf0d24892e45987e249a28dce836a85cad60e28eaaa8c6d976"`
	Reason    string    `json:"reason" enum:"erased,manual" doc:"erased: written by GDPR erase (permanent); manual: added through the API or UI" example:"manual"`
	Note      string    `json:"note" example:"asked by phone not to be emailed"`
	CreatedAt time.Time `json:"created_at"`
}

const suppressionQuery = `SELECT rowid, email_hash, reason, note, created_at FROM suppressions`

func scanSuppression(r rowScanner) (Suppression, error) {
	var s Suppression
	var created string
	if err := r.Scan(&s.ID, &s.EmailHash, &s.Reason, &s.Note, &created); err != nil {
		return Suppression{}, err
	}
	var err error
	s.CreatedAt, err = db.ParseTime(created)
	return s, err
}

// SuppressionFilter narrows ListSuppressions.
type SuppressionFilter struct {
	Reason   string // "" = any
	Hash     string // exact email hash; "" = any
	BeforeID int64  // newest first
	Limit    int    // default 50
}

// ListSuppressions returns suppressions, newest first.
func (s *Service) ListSuppressions(ctx context.Context, f SuppressionFilter) ([]Suppression, error) {
	q := suppressionQuery + ` WHERE 1=1`
	var args []any
	if f.Reason != "" {
		if f.Reason != ReasonErased && f.Reason != ReasonManual {
			return nil, core.Invalid("query.reason", "must be erased or manual")
		}
		q += ` AND reason = ?`
		args = append(args, f.Reason)
	}
	if f.Hash != "" {
		q += ` AND email_hash = ?`
		args = append(args, strings.ToLower(f.Hash))
	}
	if f.BeforeID > 0 {
		q += ` AND rowid < ?`
		args = append(args, f.BeforeID)
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q += ` ORDER BY rowid DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list suppressions: %w", err)
	}
	defer rows.Close()
	out := []Suppression{}
	for rows.Next() {
		sp, err := scanSuppression(rows)
		if err != nil {
			return nil, fmt.Errorf("scan suppression: %w", err)
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// SuppressResult is the outcome of AddSuppression.
type SuppressResult struct {
	Suppression   Suppression `json:"suppression"`
	Created       bool        `json:"created" doc:"false when the address was already suppressed"`
	ContactID     int64       `json:"contact_id,omitempty" doc:"The matching contact, if any" example:"42"`
	StatusChanged bool        `json:"status_changed" doc:"true when an active contact was set to suppressed_manual"`
}

// AddSuppression adds an address to the suppressions table (reason
// manual). An active contact with that address is set to
// suppressed_manual; contacts with other statuses keep their status (they
// already receive no marketing email) and the hash row blocks every other
// path. Suppressing an address that already is suppressed is not an error.
func (s *Service) AddSuppression(ctx context.Context, email, note string) (SuppressResult, error) {
	norm, note, err := suppressionArgs(email, note)
	if err != nil {
		return SuppressResult{}, err
	}
	hash := core.EmailHash(norm)
	var res SuppressResult
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		if res.Created, err = s.insertManualSuppression(ctx, tx, hash, note); err != nil {
			return err
		}
		c, err := s.store.GetByEmail(ctx, tx, norm)
		switch {
		case errors.Is(err, core.ErrNotFound):
		case err != nil:
			return err
		default:
			res.ContactID = c.ID
			if c.Status == core.StatusActive {
				res.StatusChanged, err = s.store.SetStatus(ctx, tx, c.ID, core.StatusSuppressedManual, contactstore.StatusChange{
					Source: changeSource(ctx), Actor: core.ActorFromContext(ctx).String(), Note: note,
				})
				if err != nil {
					return err
				}
			}
		}
		if res.Suppression, err = scanSuppression(tx.QueryRowContext(ctx, suppressionQuery+` WHERE email_hash = ?`, hash)); err != nil {
			return fmt.Errorf("read suppression: %w", err)
		}
		if !res.Created && !res.StatusChanged {
			return nil
		}
		return s.logAudit(ctx, tx, "suppression.add", "suppression", hash, map[string]any{
			"email_hash": hash, "created": res.Created, "contact_id": res.ContactID, "status_changed": res.StatusChanged,
		})
	})
	if err != nil {
		return SuppressResult{}, err
	}
	return res, nil
}

// UnsuppressResult is the outcome of RemoveSuppression.
type UnsuppressResult struct {
	EmailHash     string             `json:"email_hash"`
	ContactID     int64              `json:"contact_id,omitempty" doc:"The matching contact, if any" example:"42"`
	Status        core.ContactStatus `json:"status,omitempty" doc:"The matching contact's status afterwards, when the call changed it" example:"active"`
	StatusChanged bool               `json:"status_changed" doc:"true when a suppressed_manual contact was set back to active (or to unsubscribed, when it had asked to unsubscribe while suppressed)"`
}

// RemoveSuppression removes a manual suppression. Erased addresses can't
// be removed (403); bounces and complaints are statuses, not suppressions,
// and stay. A contact with status suppressed_manual is set back to active
// with the note, also when its hash row is missing (e.g. a contact
// imported as suppressed_manual), and the consent log records it; a
// contact that asked to unsubscribe while suppressed becomes unsubscribed
// instead (contactstore.RecordOptOut). Anything else is ErrNotFound.
func (s *Service) RemoveSuppression(ctx context.Context, email, note string) (UnsuppressResult, error) {
	norm, note, err := suppressionArgs(email, note)
	if err != nil {
		return UnsuppressResult{}, err
	}
	hash := core.EmailHash(norm)
	res := UnsuppressResult{EmailHash: hash}
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		reason, err := suppressionReason(ctx, tx, hash)
		if err != nil {
			return err
		}
		if reason != "" && reason != ReasonManual {
			return fmt.Errorf("the address was erased (GDPR) and stays suppressed: %w", core.ErrForbidden)
		}
		c, err := s.store.GetByEmail(ctx, tx, norm)
		switch {
		case errors.Is(err, core.ErrNotFound):
			if reason == "" {
				return fmt.Errorf("that address is not suppressed: %w", core.ErrNotFound)
			}
		case err != nil:
			return err
		case reason == "" && c.Status != core.StatusSuppressedManual:
			return fmt.Errorf("that address is not suppressed: %w", core.ErrNotFound)
		default:
			res.ContactID = c.ID
			if c.Status == core.StatusSuppressedManual {
				res.StatusChanged, err = s.store.SetStatus(ctx, tx, c.ID, core.StatusActive, contactstore.StatusChange{
					Source: changeSource(ctx), Actor: core.ActorFromContext(ctx).String(), Note: note,
				})
				if err != nil {
					return err
				}
				after, err := s.store.Get(ctx, tx, c.ID)
				if err != nil {
					return err
				}
				res.Status = after.Status
				if err := s.recordReactivation(ctx, tx, c.ID, c.Status, after.Status, note); err != nil {
					return err
				}
			}
		}
		if reason != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM suppressions WHERE email_hash = ?`, hash); err != nil {
				return fmt.Errorf("remove suppression: %w", err)
			}
		}
		return s.logAudit(ctx, tx, "suppression.remove", "suppression", hash, map[string]any{
			"email_hash": hash, "contact_id": res.ContactID, "status_changed": res.StatusChanged,
		})
	})
	if err != nil {
		return UnsuppressResult{}, err
	}
	return res, nil
}

// suppressionArgs normalises the address and checks the note.
func suppressionArgs(email, note string) (string, string, error) {
	norm, err := core.NormalizeEmail(email)
	if err != nil {
		return "", "", err
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxNoteLength {
		return "", "", core.Invalid("note", "is longer than %d characters", maxNoteLength)
	}
	return norm, note, nil
}

// suppressionReason returns the reason of the suppressions row of the
// address with this hash, or "" when there is none.
func suppressionReason(ctx context.Context, q db.Querier, hash string) (string, error) {
	var reason string
	err := q.QueryRowContext(ctx, `SELECT reason FROM suppressions WHERE email_hash = ?`, hash).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read suppression: %w", err)
	}
	return reason, nil
}

// insertManualSuppression adds the hash with reason manual unless the
// address is already suppressed. created reports whether a row was added.
func (s *Service) insertManualSuppression(ctx context.Context, tx *sql.Tx, hash, note string) (created bool, err error) {
	r, err := tx.ExecContext(ctx, `INSERT INTO suppressions (email_hash, reason, note, created_at)
		VALUES (?, 'manual', ?, ?) ON CONFLICT (email_hash) DO NOTHING`, hash, note, s.now())
	if err != nil {
		return false, fmt.Errorf("add suppression: %w", err)
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
