package system

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"mailroom/internal/db"
	"mailroom/internal/jobs"
)

// Retention periods.
const (
	// EventDetailRetention: raw provider event payloads (events.detail) are
	// emptied after 90 days (X-5). The event rows stay, so stats and the
	// contact timeline keep working.
	EventDetailRetention = 90 * 24 * time.Hour
	// JobRetention: finished jobs are deleted 30 days after they finished.
	JobRetention = 30 * 24 * time.Hour
	// ConfirmationLogRetention: the log of confirmation emails the forms
	// sent (confirmation_emails: address hash, form, time), which only
	// their 30-day caps read, is deleted after 31 days.
	ConfirmationLogRetention = 31 * 24 * time.Hour
	// InboundRetention: received email (the Inbox) and its attachments are
	// deleted a year after it arrived; messages that could not be fetched
	// after 30 days (N-6).
	InboundRetention       = 365 * 24 * time.Hour
	InboundFailedRetention = 30 * 24 * time.Hour
)

// retentionBatch bounds the rows changed per transaction, so the single
// write connection is never held for long.
const retentionBatch = 1000

// RetentionResult reports what a retention run changed.
type RetentionResult struct {
	EventDetailsCleared int64 `json:"event_details_cleared"`
	JobsDeleted         int64 `json:"jobs_deleted"`
	SessionsDeleted     int64 `json:"sessions_deleted"`
	ConfirmationsPurged int64 `json:"confirmations_purged"`
	InboundDeleted      int64 `json:"inbound_deleted"`
}

// Retention applies the retention rules once:
//   - events.detail older than 90 days becomes '{}' (X-5);
//   - jobs that finished more than 30 days ago are deleted;
//   - expired UI sessions are deleted (auth also prunes them at each login;
//     this covers installs where nobody logs in);
//   - the forms' confirmation email log older than 31 days is deleted;
//   - received email older than a year is deleted, and email that could
//     not be fetched after 30 days.
//
// Consent logs, status changes, suppressions and messages are never
// deleted automatically.
func (s *Service) Retention(ctx context.Context) (RetentionResult, error) {
	var res RetentionResult
	if s.db == nil {
		return res, errNoDB
	}
	now := s.clock.Now()
	var err error
	res.EventDetailsCleared, err = s.clearEventDetails(ctx, db.FormatTime(now.Add(-EventDetailRetention)))
	if err != nil {
		return res, fmt.Errorf("system: retention: clear event details: %w", err)
	}
	res.JobsDeleted, err = s.batched(ctx,
		`DELETE FROM jobs WHERE id IN (
		   SELECT id FROM jobs WHERE status IN ('`+jobs.StatusSucceeded+`', '`+jobs.StatusFailed+`')
		   AND finished_at IS NOT NULL AND finished_at < ? LIMIT ?)`,
		db.FormatTime(now.Add(-JobRetention)))
	if err != nil {
		return res, fmt.Errorf("system: retention: delete old jobs: %w", err)
	}
	res.SessionsDeleted, err = s.batched(ctx,
		`DELETE FROM sessions WHERE id IN (SELECT id FROM sessions WHERE expires_at < ? LIMIT ?)`,
		db.FormatTime(now))
	if err != nil {
		return res, fmt.Errorf("system: retention: delete expired sessions: %w", err)
	}
	res.ConfirmationsPurged, err = s.batched(ctx,
		`DELETE FROM confirmation_emails WHERE id IN (SELECT id FROM confirmation_emails WHERE queued_at < ? LIMIT ?)`,
		db.FormatTime(now.Add(-ConfirmationLogRetention)))
	if err != nil {
		return res, fmt.Errorf("system: retention: delete old confirmation log: %w", err)
	}
	res.InboundDeleted, err = s.batched(ctx,
		`DELETE FROM inbound_messages WHERE id IN (SELECT id FROM inbound_messages WHERE received_at < ? LIMIT ?)`,
		db.FormatTime(now.Add(-InboundRetention)))
	if err != nil {
		return res, fmt.Errorf("system: retention: delete old received email: %w", err)
	}
	n, err := s.batched(ctx,
		`DELETE FROM inbound_messages WHERE id IN (SELECT id FROM inbound_messages WHERE status = 'failed' AND created_at < ? LIMIT ?)`,
		db.FormatTime(now.Add(-InboundFailedRetention)))
	if err != nil {
		return res, fmt.Errorf("system: retention: delete failed received email: %w", err)
	}
	res.InboundDeleted += n
	return res, nil
}

// Event-detail retention queries. The only index on created_at is
// events(type, created_at), so finding the old payloads is a table scan:
// eventDetailRange runs it once on the read pool, where it blocks no
// writer, and clearEventDetailRange then updates one bounded primary-key
// range per write transaction. created_at is re-checked because ids and
// created_at need not grow together.
const (
	eventDetailRange      = `SELECT min(id), max(id) FROM events WHERE created_at < ? AND detail <> '{}'`
	clearEventDetailRange = `UPDATE events SET detail = '{}' WHERE id > ? AND id <= ? AND created_at < ? AND detail <> '{}'`
)

// clearEventDetails empties events.detail of the events created before
// cutoff (X-5), walking their id range in steps of retentionBatch ids, each
// in its own short write transaction.
func (s *Service) clearEventDetails(ctx context.Context, cutoff string) (int64, error) {
	var lo, hi sql.NullInt64
	if err := s.db.Read.QueryRowContext(ctx, eventDetailRange, cutoff).Scan(&lo, &hi); err != nil {
		return 0, err
	}
	if !lo.Valid || !hi.Valid {
		return 0, nil
	}
	var total int64
	for from := lo.Int64 - 1; from < hi.Int64; from += retentionBatch {
		to := min(from+retentionBatch, hi.Int64)
		var n int64
		err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, clearEventDetailRange, from, to, cutoff)
			if err != nil {
				return err
			}
			n, err = r.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
	return total, nil
}

// batched runs query (whose last two parameters are the cutoff and the
// batch size) in short transactions until it affects fewer rows than a
// batch, and returns the total rows affected.
func (s *Service) batched(ctx context.Context, query string, cutoff string) (int64, error) {
	var total int64
	for {
		var n int64
		err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, query, cutoff, retentionBatch)
			if err != nil {
				return err
			}
			n, err = r.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
