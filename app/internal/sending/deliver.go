package sending

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
)

// Retry policy (D-4): a retryable failure is retried after these delays;
// the MaxAttempts-th failure fails the message.
var retryBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// MaxAttempts is the number of provider calls after which a message that
// keeps failing with retryable errors is failed.
const MaxAttempts = 5

// dbRetryDelay is when a message is tried again after a local error
// (database, settings) that is not the message's fault.
const dbRetryDelay = 30 * time.Second

// FooterWait is how long a marketing message waits (not counted as an
// attempt) while the company footer is empty, before it is tried again.
const FooterWait = time.Hour

// AlertNoFooter is the alert key raised while marketing messages wait for
// a company footer.
const AlertNoFooter = "company_footer_missing"

// message is the part of a messages row the sender needs.
type message struct {
	ID             int64
	Kind           core.MessageKind
	BroadcastID    int64
	EnrollmentID   int64
	StepID         int64
	EmailID        int64
	ContactID      int64
	SystemTemplate string
	ToEmail        string // "" when erased
	Data           map[string]any
	Attempts       int
}

func (s *Service) loadMessage(ctx context.Context, id int64) (message, error) {
	var (
		m                         message
		kind, data                string
		bID, eID, stID, emID, cID sql.NullInt64
		to                        sql.NullString
	)
	err := s.db.Read.QueryRowContext(ctx, `
		SELECT id, kind, broadcast_id, enrollment_id, step_id, email_id, contact_id, system_template, to_email, data, attempts
		FROM messages WHERE id = ?`, id).Scan(&m.ID, &kind, &bID, &eID, &stID, &emID, &cID, &m.SystemTemplate, &to, &data, &m.Attempts)
	if err != nil {
		return message{}, fmt.Errorf("sending: load message %d: %w", id, err)
	}
	m.Kind = core.MessageKind(kind)
	m.BroadcastID, m.EnrollmentID, m.StepID, m.EmailID, m.ContactID = bID.Int64, eID.Int64, stID.Int64, emID.Int64, cID.Int64
	m.ToEmail = to.String
	m.Data = map[string]any{}
	if err := db.UnmarshalJSON(data, &m.Data); err != nil {
		return message{}, fmt.Errorf("sending: message %d data: %w", id, err)
	}
	return m, nil
}

// deliver sends a claimed message (status sending) and records the
// outcome. Pass a context that is not cancelled by shutdown: the row must
// always leave "sending".
func (s *Service) deliver(ctx context.Context, id int64) error {
	m, err := s.loadMessage(ctx, id)
	if err != nil {
		return s.retryLater(ctx, id, err)
	}
	st, err := s.settings.Get(ctx)
	if err != nil {
		return s.retryLater(ctx, id, err)
	}

	// Cheap check first, so ineligible messages are never rendered.
	contact, reason, err := s.eligibility(ctx, m)
	if err != nil {
		return s.retryLater(ctx, id, err)
	}
	if reason != "" {
		return s.markSkipped(ctx, id, reason)
	}
	to := m.ToEmail
	if contact != nil && m.Kind != core.KindTest && m.Kind != core.KindReply {
		to = contact.Email // the contact's current address
	}

	snd, err := s.identity(ctx, st, m)
	if err != nil {
		if errors.Is(err, core.ErrValidation) {
			return s.markFailed(ctx, m, "", "config", err.Error())
		}
		return s.retryLater(ctx, id, err)
	}

	marketing := core.IsMarketingKind(m.Kind)
	if marketing && strings.TrimSpace(st.CompanyFooter) == "" {
		return s.holdForFooter(ctx, m)
	}
	rendered, err := s.renderer.Render(ctx, core.RenderInput{
		MessageID:      m.ID,
		EmailID:        m.EmailID,
		SystemTemplate: m.SystemTemplate,
		Contact:        contact,
		ToEmail:        to,
		Data:           m.Data,
		// Only broadcast and sequence messages are rendered as marketing
		// (working unsubscribe link, click and open tracking). A test send
		// shows a real contact's data but must never unsubscribe that
		// contact or record clicks against them; the renderer still wraps
		// its content in the layout, with a "#preview" unsubscribe link.
		Marketing: marketing,
	})
	if err != nil {
		return s.markFailed(ctx, m, "", "render", "render: "+err.Error())
	}
	if marketing && rendered.UnsubscribeURL == "" {
		return s.markFailed(ctx, m, rendered.Subject, "render", "render: no unsubscribe URL for a marketing message")
	}

	msgID, err := newMessageID(m.ID, s.domain)
	if err != nil {
		return s.retryLater(ctx, id, err)
	}
	mm := mimeMessage{
		FromName:   snd.fromName,
		From:       snd.from,
		To:         to,
		ReplyTo:    snd.replyTo,
		Subject:    rendered.Subject,
		Date:       s.clock.Now(),
		MessageID:  msgID,
		Text:       rendered.Text,
		HTML:       rendered.HTML,
		FeedbackID: feedbackID(m, s.domain),
	}
	if marketing {
		mm.ListUnsubscribe = rendered.UnsubscribeURL
	}
	switch m.Kind {
	case core.KindAlert:
		mm.AutoSubmitted = true
	case core.KindReply:
		mm.FeedbackID = ""
		mm.InReplyTo, _ = m.Data[core.DataReplyInReplyTo].(string)
		if refs, ok := m.Data[core.DataReplyReferences].([]any); ok {
			for _, r := range refs {
				if s, ok := r.(string); ok {
					mm.References = append(mm.References, s)
				}
			}
		}
	}
	raw, err := buildMIME(mm)
	if err != nil {
		return s.markFailed(ctx, m, rendered.Subject, "mime", err.Error())
	}

	// Final check immediately before the provider call (D-6): the contact
	// may have unsubscribed, bounced or been erased while we rendered.
	latest, reason, err := s.eligibility(ctx, m)
	if err != nil {
		return s.retryLater(ctx, id, err)
	}
	if reason != "" {
		return s.markSkipped(ctx, id, reason)
	}
	if latest != nil && m.Kind != core.KindTest && m.Kind != core.KindReply && latest.Email != to {
		// The address changed while we rendered: render again for the new one.
		return s.retryLater(ctx, id, errors.New("contact address changed during send"))
	}

	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	providerID, err := s.provider.Send(sctx, core.OutgoingEmail{
		From:    snd.from,
		To:      to,
		ReplyTo: snd.replyTo,
		Raw:     raw,
		Tags:    map[string]string{"kind": string(m.Kind), "message_id": strconv.FormatInt(m.ID, 10)},
	})
	cancel()
	if err == nil {
		return s.markSent(ctx, m, providerID, rendered.Subject, to)
	}
	return s.handleSendError(ctx, m, rendered.Subject, err)
}

// senderIdentity is the From/Reply-To of one message.
type senderIdentity struct{ fromName, from, replyTo string }

// identity returns the settings sender, with a broadcast's from_name and
// reply_to overrides (NULL = settings default, "" reply_to = none), or for
// a test message the overrides in its data (core.DataFromName,
// core.DataReplyTo). The
// addresses must be on the sending domain (D-7); a violation is a
// validation error that fails the message.
func (s *Service) identity(ctx context.Context, st settings.Settings, m message) (senderIdentity, error) {
	id := senderIdentity{fromName: st.Sender.FromName, from: st.Sender.FromEmail, replyTo: st.Sender.ReplyTo}
	if m.BroadcastID != 0 {
		var name, reply sql.NullString
		err := s.db.Read.QueryRowContext(ctx, `SELECT from_name, reply_to FROM broadcasts WHERE id = ?`, m.BroadcastID).Scan(&name, &reply)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return id, fmt.Errorf("sending: broadcast %d sender: %w", m.BroadcastID, err)
		}
		if name.Valid {
			id.fromName = name.String
		}
		if reply.Valid {
			id.replyTo = reply.String
		}
	}
	if m.Kind == core.KindTest {
		// A test may carry its origin's sender (a broadcast's overrides).
		if v, ok := m.Data[core.DataFromName].(string); ok {
			id.fromName = v
		}
		if v, ok := m.Data[core.DataReplyTo].(string); ok {
			id.replyTo = v
		}
	}
	if id.from == "" {
		return id, core.Invalid("sender.from_email", "is not set in the settings")
	}
	if !onDomain(id.from, s.domain) {
		return id, core.Invalid("sender.from_email", "is not on the sending domain %s", s.domain)
	}
	if id.replyTo != "" && !onDomain(id.replyTo, s.domain) {
		return id, core.Invalid("reply_to", "is not on the sending domain %s", s.domain)
	}
	return id, nil
}

func onDomain(addr, domain string) bool {
	if domain == "" {
		return true
	}
	return strings.HasSuffix(strings.ToLower(addr), "@"+strings.ToLower(domain))
}

// feedbackID is "<campaign>:<kind>:mailroom:<domain>", the Gmail
// Feedback-ID: campaign is b<broadcast id>, s<step id> or the kind.
func feedbackID(m message, domain string) string {
	campaign := string(m.Kind)
	switch {
	case m.BroadcastID != 0:
		campaign = "b" + strconv.FormatInt(m.BroadcastID, 10)
	case m.StepID != 0:
		campaign = "s" + strconv.FormatInt(m.StepID, 10)
	}
	return campaign + ":" + string(m.Kind) + ":mailroom:" + domain
}

// handleSendError records a provider failure: a global pause (SES sending
// paused, or the access key rejected) re-queues the message and pauses all
// sending; a retryable error re-queues it with
// backoff until MaxAttempts; anything else fails it. An error that is not
// a *core.ProviderError says nothing about whether the provider accepted
// the message, so it fails the message as CodeDeliveryUnknown instead of
// risking a second delivery (at most once).
func (s *Service) handleSendError(ctx context.Context, m message, subject string, err error) error {
	var pe *core.ProviderError
	if !errors.As(err, &pe) {
		pe = &core.ProviderError{Code: CodeDeliveryUnknown, Message: "delivery unknown: " + err.Error()}
	}
	s.log.Warn("sending: provider error", "message_id", m.ID, "code", pe.Code, "retryable", pe.Retryable)
	if pe.Code == CodeSendingPaused || pe.Code == CodeCredentialsRejected {
		pause := s.pauseForSES
		if pe.Code == CodeCredentialsRejected {
			pause = s.pauseForCredentials
		}
		return s.persist(ctx, m.ID, "requeue (sending paused)", func() error {
			return s.db.WithTx(ctx, func(tx *sql.Tx) error {
				if err := pause(ctx, tx, pe.Message); err != nil {
					return err
				}
				return s.requeue(ctx, tx, m.ID, s.clock.Now(), m.Attempts, subject, pe.Message)
			})
		})
	}
	attempts := m.Attempts + 1
	if !pe.Retryable || attempts >= MaxAttempts {
		msg := pe.Message
		if pe.Retryable {
			msg = fmt.Sprintf("gave up after %d attempts: %s", attempts, msg)
		}
		return s.fail(ctx, m.ID, attempts, subject, pe.Code, msg)
	}
	next := s.clock.Now().Add(retryBackoff[attempts-1])
	return s.persist(ctx, m.ID, "requeue (retry)", func() error {
		return s.requeue(ctx, s.db.Write, m.ID, next, attempts, subject, pe.Code+": "+pe.Message)
	})
}

// defaultOutcomeBackoff is how long the sender waits between attempts to
// record a message's outcome (about 30 s in total).
var defaultOutcomeBackoff = []time.Duration{
	100 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second,
	5 * time.Second, 10 * time.Second, 10 * time.Second,
}

// persist runs write, retrying it with s.outcomeBackoff while it fails
// (e.g. SQLITE_BUSY while another process holds the database lock). A
// claimed message must leave "sending": otherwise it is failed as
// interrupted at the next start, and after a successful send the provider
// MessageId, which maps delivery, bounce and complaint notifications to the
// message, would be lost. Pass a context that shutdown does not cancel.
func (s *Service) persist(ctx context.Context, id int64, what string, write func() error) error {
	err := write()
	for i := 0; err != nil && i < len(s.outcomeBackoff); i++ {
		s.log.Warn("sending: recording the outcome failed, retrying", "message_id", id, "outcome", what, "err", err)
		if !wait(ctx, s.outcomeBackoff[i], nil) {
			break
		}
		err = write()
	}
	return err
}

// holdForFooter re-queues a marketing message for FooterWait, without
// counting an attempt, and alerts the owner: every marketing email must
// carry the sender's name and postal address (E-1), so none goes out
// while settings company_footer is empty.
func (s *Service) holdForFooter(ctx context.Context, m message) error {
	const why = "waiting for a company footer: marketing email must show the sender's name and postal address (set company_footer in the settings)"
	s.log.Warn("sending: marketing message held: the company footer is empty", "message_id", m.ID)
	return s.persist(ctx, m.ID, "requeue (no footer)", func() error {
		return s.db.WithTx(ctx, func(tx *sql.Tx) error {
			if err := s.Alert(ctx, tx, AlertNoFooter, "Marketing email is on hold: the company footer is empty",
				"Every marketing email must show your company name and postal address, and the company footer in the "+
					"settings is empty. Broadcast and sequence emails wait until it is set (Settings in the admin UI, or "+
					"PATCH /api/v1/settings with company_footer); nothing is lost."); err != nil {
				return err
			}
			return s.requeue(ctx, tx, m.ID, s.clock.Now().Add(FooterWait), m.Attempts, "", why)
		})
	})
}

// requeue puts a sending message back to queued, due at next.
func (s *Service) requeue(ctx context.Context, q db.Querier, id int64, next time.Time, attempts int, subject, errText string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE messages SET status = 'queued', attempts = ?, next_attempt_at = ?, error = ?,
		       subject = CASE WHEN ? = '' THEN subject ELSE ? END, updated_at = ?
		WHERE id = ? AND status = 'sending'`,
		attempts, db.FormatTime(next), truncate(errText), subject, subject, db.FormatTime(s.clock.Now()), id)
	if err != nil {
		return fmt.Errorf("sending: requeue message %d: %w", id, err)
	}
	return nil
}

// retryLater re-queues a message after a local error (not counted as an
// attempt), so a database hiccup never loses or fails a message.
func (s *Service) retryLater(ctx context.Context, id int64, cause error) error {
	s.log.Error("sending: message deferred after local error", "message_id", id, "err", cause)
	return s.persist(ctx, id, "requeue (local error)", func() error {
		var attempts int
		if err := s.db.Read.QueryRowContext(ctx, `SELECT attempts FROM messages WHERE id = ?`, id).Scan(&attempts); err != nil {
			return fmt.Errorf("sending: read attempts of message %d: %w", id, err)
		}
		return s.requeue(ctx, s.db.Write, id, s.clock.Now().Add(dbRetryDelay), attempts, "", "deferred: "+cause.Error())
	})
}

// markSent records a successful send. If that cannot be recorded even
// after retries, the provider MessageId is logged at error level so the
// mapping can be repaired by hand.
func (s *Service) markSent(ctx context.Context, m message, providerID, subject, to string) error {
	err := s.persist(ctx, m.ID, "sent", func() error {
		now := db.FormatTime(s.clock.Now())
		_, err := s.db.Write.ExecContext(ctx, `
			UPDATE messages SET status = 'sent', ses_message_id = ?, sent_at = ?, subject = ?,
			       to_email = CASE WHEN to_email IS NULL THEN NULL ELSE ? END, -- erased meanwhile: keep it erased
			       attempts = attempts + 1, error = '', updated_at = ?
			WHERE id = ? AND status = 'sending'`, providerID, now, subject, to, now, m.ID)
		return err
	})
	if err != nil {
		s.log.Error("sending: message sent but not recorded; it stays in sending and is failed as interrupted at the next start",
			"message_id", m.ID, "ses_message_id", providerID, "err", err)
		return fmt.Errorf("sending: mark message %d sent: %w", m.ID, err)
	}
	s.log.Debug("sending: sent", "message_id", m.ID, "kind", m.Kind)
	return nil
}

func (s *Service) markSkipped(ctx context.Context, id int64, reason string) error {
	err := s.persist(ctx, id, "skipped", func() error {
		_, err := s.db.Write.ExecContext(ctx, `
			UPDATE messages SET status = 'skipped', error = ?, updated_at = ? WHERE id = ? AND status = 'sending'`,
			reason, db.FormatTime(s.clock.Now()), id)
		return err
	})
	if err != nil {
		return fmt.Errorf("sending: mark message %d skipped: %w", id, err)
	}
	s.log.Info("sending: skipped", "message_id", id, "reason", reason)
	return nil
}

// markFailed fails a message without a provider call (render or
// configuration problem).
func (s *Service) markFailed(ctx context.Context, m message, subject, code, msg string) error {
	return s.fail(ctx, m.ID, m.Attempts, subject, code, msg)
}

// fail sets a message failed and records a send_error event.
func (s *Service) fail(ctx context.Context, id int64, attempts int, subject, code, msg string) error {
	msg = truncate(msg)
	s.log.Warn("sending: message failed", "message_id", id, "code", code)
	return s.persist(ctx, id, "failed", func() error {
		return s.db.WithTx(ctx, func(tx *sql.Tx) error {
			now := db.FormatTime(s.clock.Now())
			res, err := tx.ExecContext(ctx, `
				UPDATE messages SET status = 'failed', attempts = ?, error = ?,
				       subject = CASE WHEN ? = '' THEN subject ELSE ? END, updated_at = ?
				WHERE id = ? AND status = 'sending'`, attempts, msg, subject, subject, now, id)
			if err != nil {
				return fmt.Errorf("sending: mark message %d failed: %w", id, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil
			}
			detail, err := db.MarshalJSON(map[string]any{"code": code, "message": msg})
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO events (message_id, type, subtype, detail, created_at) VALUES (?, 'send_error', ?, ?, ?)`,
				id, code, detail, now)
			return err
		})
	})
}

// maxErrorLen caps stored error texts.
const maxErrorLen = 1000

func truncate(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	s = s[:maxErrorLen]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}
