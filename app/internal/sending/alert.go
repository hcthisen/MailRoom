package sending

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/outbox"
)

// AlertDedupWindow is how long an alert key stays de-duplicated.
const AlertDedupWindow = 6 * time.Hour

var _ core.Alerter = (*Service)(nil)

// Alert queues an alert email (PRD O-5) to settings alert_email, or
// owner_email when that is empty: a message of kind alert with system
// template alert and data {"key", "title", "body"}. An alert with the same
// key queued in the last AlertDedupWindow suppresses this one. Alerts are
// never suppressed at send time.
//
// Pass the caller's transaction as tx to join it (the alert then exists
// only if that transaction commits), or nil to use an own transaction.
// Alert only writes to the database. When no alert address is configured,
// or it is invalid, the alert is logged and dropped rather than failing
// the caller.
func (s *Service) Alert(ctx context.Context, tx *sql.Tx, key, title, body string) error {
	if key == "" {
		return core.Invalid("key", "is required")
	}
	if tx == nil {
		return s.db.WithTx(ctx, func(tx *sql.Tx) error { return s.alert(ctx, tx, key, title, body) })
	}
	return s.alert(ctx, tx, key, title, body)
}

func (s *Service) alert(ctx context.Context, q db.Querier, key, title, body string) error {
	now := s.clock.Now()
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM messages
		WHERE kind = 'alert' AND queued_at >= ? AND json_extract(data, '$.key') = ?
		LIMIT 1`, db.FormatTime(now.Add(-AlertDedupWindow)), key).Scan(&one)
	switch {
	case err == nil:
		s.log.Debug("sending: alert de-duplicated", "key", key)
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("sending: alert dedup: %w", err)
	}

	st, err := s.settings.Get(ctx)
	if err != nil {
		return err
	}
	to := st.AlertAddress()
	if to == "" {
		s.log.Error("sending: alert dropped: no alert_email or owner_email configured", "key", key, "title", title)
		return nil
	}
	_, _, err = outbox.Enqueue(ctx, q, s.clock, outbox.MessageSpec{
		Kind:           core.KindAlert,
		SystemTemplate: core.TemplateAlert,
		ToEmail:        to,
		Data:           map[string]any{"key": key, "title": title, "body": body},
	})
	if errors.Is(err, core.ErrValidation) {
		s.log.Error("sending: alert dropped: invalid alert address", "key", key, "title", title, "err", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("sending: queue alert %q: %w", key, err)
	}
	s.log.Info("sending: alert queued", "key", key, "title", title)
	return nil
}
