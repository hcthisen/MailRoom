package inbound

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/outbox"
)

// Reply limits.
const (
	MaxReplyBody  = 50000
	maxQuoteBytes = 20000
	maxReferences = 20
)

// ReplyInput is a reply to a received message.
type ReplyInput struct {
	Body    string `json:"body" minLength:"1" maxLength:"50000" doc:"The reply, as plain text" example:"Hi Anna,\n\nThanks for your note. See you Friday!\n\nBest, Ben"`
	Subject string `json:"subject,omitempty" maxLength:"998" doc:"Default: \"Re: \" and the received subject" example:"Re: April news"`
	// Quote is a pointer so an omitted field means true.
	Quote *bool `json:"quote,omitempty" doc:"Quote the received text below the reply (default true)"`
}

// ReplyResult is what Reply queued (or, in a dry run, would queue).
type ReplyResult struct {
	MessageID int64  `json:"message_id" doc:"The queued messages row; 0 in a dry run" example:"901"`
	To        string `json:"to" doc:"Where the reply goes: the Reply-To of the received message, else its sender" example:"anna@example.org"`
	Subject   string `json:"subject" example:"Re: April news"`
	FromName  string `json:"from_name" doc:"The From name (the settings sender)" example:"Example Co"`
	From      string `json:"from" doc:"The From address (the settings sender)" example:"news@example.com"`
}

// Reply queues a reply (message kind reply) to a received message: plain
// text from the settings sender, threaded with In-Reply-To and
// References, with the received text quoted unless in.Quote is false. The
// message is marked read. Refused with 409 when the address is suppressed
// or its contact bounced or complained.
func (s *Service) Reply(ctx context.Context, id int64, in ReplyInput, dryRun bool) (ReplyResult, error) {
	body := strings.TrimSpace(strings.ReplaceAll(in.Body, "\r\n", "\n"))
	if body == "" {
		return ReplyResult{}, core.Invalid("body", "is required")
	}
	if utf8.RuneCountInString(body) > MaxReplyBody {
		return ReplyResult{}, core.Invalid("body", "is longer than %d characters", MaxReplyBody)
	}
	st, err := s.settings.Get(ctx)
	if err != nil {
		return ReplyResult{}, err
	}
	if st.Sender.FromEmail == "" {
		return ReplyResult{}, core.Invalid("sender.from_email", "is not set in the settings: replies are sent from it")
	}
	var res ReplyResult
	err = api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		m, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if m.Status != StatusReceived {
			return fmt.Errorf("received message %d is %s; only a fetched message can be answered: %w", id, m.Status, core.ErrConflict)
		}
		to := m.ReplyTo
		if to == "" {
			to = m.FromEmail
		}
		if to, err = core.NormalizeEmail(to); err != nil {
			return fmt.Errorf("the sender's address %q can't receive a reply: %w", m.FromEmail, core.ErrConflict)
		}
		if err := s.canReply(ctx, tx, to); err != nil {
			return err
		}
		subject := singleLine(in.Subject)
		if subject == "" {
			subject = replySubject(m.Subject)
		}
		data := map[string]any{
			core.DataReplySubject: subject,
			core.DataReplyBody:    body,
			"actor":               actorString(ctx),
		}
		if in.Quote == nil || *in.Quote {
			if q := strings.TrimSpace(m.Text); q != "" {
				data[core.DataReplyQuote] = limit(q, maxQuoteBytes)
				data[core.DataReplyQuoteHead] = quoteHeader(m, st.Location())
			}
		}
		if m.MessageIDHeader != "" {
			data[core.DataReplyInReplyTo] = m.MessageIDHeader
			data[core.DataReplyReferences] = references(ctx, tx, id, m.MessageIDHeader)
		}
		var contactID int64
		if m.ContactID != nil {
			contactID = *m.ContactID
		}
		res = ReplyResult{To: to, Subject: subject, FromName: st.Sender.FromName, From: st.Sender.FromEmail}
		msgID, _, err := outbox.Enqueue(ctx, tx, s.clock, outbox.MessageSpec{
			Kind: core.KindReply, SystemTemplate: core.TemplateReply, InboundID: id,
			ContactID: contactID, ToEmail: to, Data: data,
		})
		if err != nil {
			return err
		}
		if !dryRun {
			res.MessageID = msgID
		}
		if !m.Read {
			now := db.FormatTime(s.clock.Now())
			if _, err := tx.ExecContext(ctx, `UPDATE inbound_messages SET read_at = ?, version = version + 1, updated_at = ? WHERE id = ?`,
				now, now, id); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, "inbound.reply", id, map[string]any{"message_id": msgID, "subject": subject})
	})
	return res, err
}

// canReply refuses addresses the sender would skip: suppressed ones and
// contacts that bounced or complained (core.CanReceive for kind reply).
func (s *Service) canReply(ctx context.Context, tx *sql.Tx, to string) error {
	sup, err := s.contacts.IsSuppressed(ctx, tx, to)
	if err != nil {
		return err
	}
	if sup {
		return fmt.Errorf("%s is on the suppression list, so no email can be sent to it: %w", to, core.ErrSuppressed)
	}
	c, err := s.contacts.GetByEmail(ctx, tx, to)
	switch {
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrValidation):
		return nil
	case err != nil:
		return err
	case !core.CanReceive(c.Status, core.KindReply):
		return fmt.Errorf("the contact %s is %s, so no email can be sent to it: %w", to, c.Status, core.ErrSuppressed)
	}
	return nil
}

// replySubject is "Re: subject", without stacking prefixes.
func replySubject(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Re: your email"
	}
	l := strings.ToLower(s)
	if strings.HasPrefix(l, "re:") || strings.HasPrefix(l, "sv:") || strings.HasPrefix(l, "aw:") {
		return s
	}
	return "Re: " + s
}

// quoteHeader is "On <date>, <sender> wrote:".
func quoteHeader(m Message, loc *time.Location) string {
	at := m.ReceivedAt
	if m.SentDate != nil {
		at = *m.SentDate
	}
	who := m.FromEmail
	if m.FromName != "" {
		who = m.FromName + " <" + m.FromEmail + ">"
	}
	return "On " + at.In(loc).Format("Mon, 2 Jan 2006 at 15:04 MST") + ", " + who + " wrote:"
}

// references is the References of the reply: those of the received
// message, then its Message-ID (RFC 5322 §3.6.4), at most maxReferences.
func references(ctx context.Context, tx *sql.Tx, id int64, msgID string) []string {
	var raw string
	_ = tx.QueryRowContext(ctx, `SELECT references_header FROM inbound_messages WHERE id = ?`, id).Scan(&raw)
	var refs []string
	for _, r := range strings.Fields(raw) {
		if r = strings.Trim(r, "<>,"); r != "" && strings.Contains(r, "@") {
			refs = append(refs, r)
		}
	}
	refs = append(refs, msgID)
	if len(refs) > maxReferences {
		refs = append(refs[:1], refs[len(refs)-maxReferences+1:]...)
	}
	return refs
}

func actorString(ctx context.Context) string {
	a := core.ActorFromContext(ctx)
	if a.Type == "" {
		return ""
	}
	if a.Name != "" {
		return a.Type + ":" + a.Name
	}
	return a.Type + ":" + a.ID
}
