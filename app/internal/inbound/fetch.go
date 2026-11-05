package inbound

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/server"
)

// Fetcher cadence and retries.
const (
	// DefaultPollInterval is how often the fetcher looks for pending
	// messages when idle.
	DefaultPollInterval = 5 * time.Second
	// fetchBatch is how many pending messages one FetchDue handles.
	fetchBatch = 20
	// fetchTimeout bounds one S3 download.
	fetchTimeout = 2 * time.Minute
)

// fetchBackoff is the delay before each retry; after the last one the
// message is failed.
var fetchBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

// FetchResult counts what one FetchDue did.
type FetchResult struct {
	Received, Retried, Failed int
}

// FetchDue fetches, parses and stores the pending messages that are due,
// then deletes their S3 copies. Each message is handled on its own; a
// failure is retried with backoff and fails the message after the last
// retry. Without an S3 client it does nothing.
func (s *Service) FetchDue(ctx context.Context) (FetchResult, error) {
	var res FetchResult
	if s.s3 == nil {
		return res, nil
	}
	rows, err := s.db.Read.QueryContext(ctx, `
		SELECT id, s3_key, attempts, virus_verdict FROM inbound_messages
		WHERE status = 'pending' AND next_attempt_at <= ? ORDER BY next_attempt_at, id LIMIT ?`,
		db.FormatTime(s.clock.Now()), fetchBatch)
	if err != nil {
		return res, fmt.Errorf("inbound: pending messages: %w", err)
	}
	type pending struct {
		id       int64
		key      string
		attempts int
		virus    string
	}
	var due []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.key, &p.attempts, &p.virus); err != nil {
			rows.Close()
			return res, err
		}
		due = append(due, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, p := range due {
		if ctx.Err() != nil {
			return res, nil
		}
		err := s.fetchOne(ctx, p.id, p.key, p.virus)
		if err == nil {
			res.Received++
			continue
		}
		failed, rerr := s.retry(ctx, p.id, p.attempts, err)
		if rerr != nil {
			return res, rerr
		}
		if failed {
			res.Failed++
		} else {
			res.Retried++
		}
	}
	return res, nil
}

// fetchOne downloads, parses and stores one message.
func (s *Service) fetchOne(ctx context.Context, id int64, key, virus string) error {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	out, err := s.s3.GetObject(fctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.InboundBucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("fetch s3://%s/%s: %w", s.cfg.InboundBucket, key, err)
	}
	raw, err := io.ReadAll(io.LimitReader(out.Body, MaxRawBytes+1))
	out.Body.Close()
	if err != nil {
		return fmt.Errorf("read s3://%s/%s: %w", s.cfg.InboundBucket, key, err)
	}
	if len(raw) > MaxRawBytes {
		return permanent(fmt.Errorf("the message is larger than %d MB", MaxRawBytes>>20))
	}
	p, err := parseMessage(raw)
	if err != nil {
		return permanent(err)
	}
	if strings.EqualFold(virus, "FAIL") && len(p.Attachments) > 0 {
		p.Attachments = nil
		p.Notes = append(p.Notes, "Amazon SES found a virus in this message: its attachments were not kept.")
	}
	if err := s.store(ctx, id, len(raw), p); err != nil {
		return err
	}
	// The message now lives in the database; the S3 copy would otherwise
	// stay until the bucket's lifecycle rule removes it.
	if _, err := s.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.cfg.InboundBucket), Key: aws.String(key)}); err != nil {
		s.log.Warn("inbound: delete stored S3 copy (the bucket's lifecycle rule removes it later)", "key", key, "err", err)
	}
	return nil
}

// store writes a parsed message and its attachments, and links it to the
// contact with the sender's address.
func (s *Service) store(ctx context.Context, id int64, size int, p parsed) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM inbound_messages WHERE id = ?`, id).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // deleted meanwhile
			}
			return err
		}
		if status != "pending" {
			return nil
		}
		var contactID int64
		if p.FromEmail != "" {
			c, err := s.contacts.GetByEmail(ctx, tx, p.FromEmail)
			switch {
			case err == nil:
				contactID = c.ID
			case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrValidation):
			default:
				return err
			}
		}
		to, _ := db.MarshalJSON(nonNil(p.To))
		cc, _ := db.MarshalJSON(nonNil(p.Cc))
		var date any
		if p.Date != nil {
			date = db.FormatTime(*p.Date)
		}
		text := p.Text
		if len(p.Notes) > 0 {
			text = strings.TrimRight(text, "\n")
			if text != "" {
				text += "\n\n"
			}
			text += "[Mailroom: " + strings.Join(p.Notes, " ") + "]"
		}
		now := db.FormatTime(s.clock.Now())
		_, err := tx.ExecContext(ctx, `
			UPDATE inbound_messages SET
				from_email = CASE WHEN ? <> '' THEN ? ELSE from_email END,
				from_name = CASE WHEN ? <> '' THEN ? ELSE from_name END,
				to_addrs = ?, cc_addrs = ?, reply_to = ?,
				subject = ?, message_id_header = CASE WHEN ? <> '' THEN ? ELSE message_id_header END,
				in_reply_to = ?, references_header = ?, sent_date = ?,
				text_body = ?, html_body = ?, snippet = ?, size_bytes = ?, contact_id = ?,
				status = 'received', fetch_error = '', version = version + 1, updated_at = ?
			WHERE id = ?`,
			p.FromEmail, p.FromEmail, p.FromName, p.FromName, to, cc, p.ReplyTo,
			p.Subject, p.MessageID, p.MessageID, p.InReplyTo, p.References, date,
			text, p.HTML, snippet(text), size, db.NullID(contactID), now, id)
		if err != nil {
			return fmt.Errorf("inbound: store message %d: %w", id, err)
		}
		for i, a := range p.Attachments {
			inline := 0
			if a.Inline {
				inline = 1
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO inbound_attachments (inbound_id, position, filename, content_type, content_id, inline, size_bytes, data)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (inbound_id, position) DO NOTHING`,
				id, i+1, a.Filename, a.ContentType, a.ContentID, inline, len(a.Data), a.Data); err != nil {
				return fmt.Errorf("inbound: store attachment: %w", err)
			}
		}
		return nil
	})
}

// permanentError marks a failure a retry can't fix.
type permanentError struct{ error }

func permanent(err error) error { return permanentError{err} }

func (e permanentError) Unwrap() error { return e.error }

// retry records a failed attempt: the message is tried again after the
// next backoff delay, or failed after the last one or on a permanent
// error. It reports whether the message failed.
func (s *Service) retry(ctx context.Context, id int64, attempts int, cause error) (bool, error) {
	attempts++
	var pe permanentError
	var ae smithy.APIError
	failed := errors.As(cause, &pe) || attempts > len(fetchBackoff) ||
		errors.As(cause, &ae) && (ae.ErrorCode() == "NoSuchKey" || ae.ErrorCode() == "NotFound")
	next := s.clock.Now()
	if !failed {
		next = next.Add(fetchBackoff[attempts-1])
	}
	status := "pending"
	if failed {
		status = "failed"
	}
	msg := limit(cause.Error(), 500)
	if failed {
		s.log.Error("inbound: giving up on a received message", "id", id, "attempts", attempts, "err", msg)
	} else {
		s.log.Warn("inbound: fetch failed; will retry", "id", id, "attempts", attempts, "err", msg)
	}
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE inbound_messages SET status = ?, attempts = ?, next_attempt_at = ?, fetch_error = ?, updated_at = ?
			WHERE id = ? AND status = 'pending'`,
			status, attempts, db.FormatTime(next), msg, db.FormatTime(s.clock.Now()), id)
		return err
	})
	return failed, err
}

// FetcherWorker returns the "inbound-fetcher" worker: it runs FetchDue
// every poll interval (at once again after a full batch).
func (s *Service) FetcherWorker() server.Worker { return &fetcher{s: s} }

type fetcher struct{ s *Service }

// Name implements server.Worker.
func (w *fetcher) Name() string { return "inbound-fetcher" }

// Run implements server.Worker. It returns when ctx is cancelled.
func (w *fetcher) Run(ctx context.Context) error {
	for {
		res, err := w.s.FetchDue(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.s.log.ErrorContext(ctx, "inbound: fetch received mail", "err", err)
		}
		if res.Received > 0 {
			w.s.log.InfoContext(ctx, "inbound: received mail stored", "count", res.Received)
		}
		wait := w.s.poll
		if res.Received+res.Retried+res.Failed >= fetchBatch {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}
