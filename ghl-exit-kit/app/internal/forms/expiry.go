package forms

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/server"
)

// expiryBatch bounds the pending opt-ins deleted per transaction, so the
// single write connection is never held for long.
const expiryBatch = 500

// ExpiryResult is what one expiry run deleted.
type ExpiryResult struct {
	PendingOptins int64 `json:"pending_optins"`
	Contacts      int64 `json:"contacts"`
}

// Expire deletes unconfirmed pending opt-ins past their expiry, and the
// contacts a form created for them that are still pending and have no
// other open pending opt-in: sign-ups that were never confirmed (F-6).
// Their confirmation messages keep their counts but lose the address.
// A contact counts as created by a form when its consent log holds only
// the form's optin_requested events. Pending contacts created any other
// way (API, admin, import: they carry that consent event) are never
// deleted, even when they later submitted a form and never confirmed;
// only their expired opt-in goes.
func (s *Service) Expire(ctx context.Context) (ExpiryResult, error) {
	var total ExpiryResult
	for {
		var res ExpiryResult
		err := s.db.WithTx(ctx, func(tx *sql.Tx) (err error) {
			res, err = s.expireBatch(ctx, tx)
			return err
		})
		if err != nil {
			return total, err
		}
		total.PendingOptins += res.PendingOptins
		total.Contacts += res.Contacts
		if res.PendingOptins < expiryBatch {
			return total, nil
		}
	}
}

func (s *Service) expireBatch(ctx context.Context, tx *sql.Tx) (ExpiryResult, error) {
	now := db.FormatTime(s.clock.Now())
	rows, err := tx.QueryContext(ctx, `
		SELECT id, contact_id FROM pending_optins
		WHERE confirmed_at IS NULL AND expires_at <= ?
		ORDER BY id LIMIT ?`, now, expiryBatch)
	if err != nil {
		return ExpiryResult{}, fmt.Errorf("find expired opt-ins: %w", err)
	}
	var ids, contacts []int64
	for rows.Next() {
		var id, contact int64
		if err := rows.Scan(&id, &contact); err != nil {
			rows.Close()
			return ExpiryResult{}, err
		}
		ids, contacts = append(ids, id), append(contacts, contact)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ExpiryResult{}, err
	}
	var res ExpiryResult
	if len(ids) == 0 {
		return res, nil
	}
	idsJSON, err := db.MarshalJSON(ids)
	if err != nil {
		return res, err
	}
	r, err := tx.ExecContext(ctx, `DELETE FROM pending_optins WHERE id IN (SELECT value FROM json_each(?))`, idsJSON)
	if err != nil {
		return res, fmt.Errorf("delete expired opt-ins: %w", err)
	}
	res.PendingOptins, _ = r.RowsAffected()

	contactsJSON, err := db.MarshalJSON(contacts)
	if err != nil {
		return res, err
	}
	// Never-confirmed form sign-ups: still pending, no open opt-in left,
	// and a consent log of nothing but form opt-in requests.
	const stale = `
		SELECT c.id FROM contacts c
		WHERE c.id IN (SELECT value FROM json_each(?1)) AND c.status = 'pending'
		  AND NOT EXISTS (SELECT 1 FROM pending_optins p WHERE p.contact_id = c.id AND p.confirmed_at IS NULL)
		  AND EXISTS (SELECT 1 FROM consent_events e
		              WHERE e.contact_id = c.id AND e.type = 'optin_requested' AND e.source LIKE 'form:%')
		  AND NOT EXISTS (SELECT 1 FROM consent_events e
		                  WHERE e.contact_id = c.id AND (e.type <> 'optin_requested' OR e.source NOT LIKE 'form:%'))`
	// Their messages (confirmation emails) outlive them as counts only: the
	// SES events lose the raw JSON, which names the recipient, and the
	// messages their address. Without the contact row an erase could no
	// longer find them.
	if _, err := tx.ExecContext(ctx, `
		UPDATE events SET detail = '{}' WHERE message_id IN (SELECT id FROM messages WHERE contact_id IN (`+stale+`))`,
		contactsJSON); err != nil {
		return res, fmt.Errorf("anonymise expired sign-ups' events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE messages SET to_email = NULL WHERE contact_id IN (`+stale+`)`, contactsJSON); err != nil {
		return res, fmt.Errorf("anonymise expired sign-ups' messages: %w", err)
	}
	r, err = tx.ExecContext(ctx, `DELETE FROM contacts WHERE id IN (`+stale+`)`, contactsJSON)
	if err != nil {
		return res, fmt.Errorf("delete expired sign-ups: %w", err)
	}
	res.Contacts, _ = r.RowsAffected()
	return res, audit.Write(core.WithActor(ctx, core.Actor{Type: core.ActorSystem}), tx, audit.Entry{
		Operation: "form.optin_expire", TargetType: "pending_optin", Changes: res, At: s.clock.Now(),
	})
}

// ExpiryWorker returns the "optin-expiry" worker: it runs Expire at start
// and then every ExpiryInterval.
func (s *Service) ExpiryWorker() server.Worker {
	return &expiryWorker{s: s, interval: ExpiryInterval}
}

type expiryWorker struct {
	s        *Service
	interval time.Duration
}

// Name implements server.Worker.
func (w *expiryWorker) Name() string { return "optin-expiry" }

// Run implements server.Worker. It returns when ctx is cancelled.
func (w *expiryWorker) Run(ctx context.Context) error {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		res, err := w.s.Expire(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			w.s.log.ErrorContext(ctx, "forms: expire unconfirmed sign-ups", "err", err)
		case res.PendingOptins > 0:
			w.s.log.InfoContext(ctx, "forms: expired unconfirmed sign-ups",
				"pending_optins", res.PendingOptins, "contacts", res.Contacts)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
