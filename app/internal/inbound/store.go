package inbound

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Message statuses (inbound_messages.status).
const (
	StatusPending  = "pending"
	StatusReceived = "received"
	StatusFailed   = "failed"
)

// Verdicts is what SES found when it received the message.
type Verdicts struct {
	Spam  string `json:"spam" doc:"SES spam scan: PASS, FAIL, GRAY, PROCESSING_FAILED or DISABLED" example:"PASS"`
	Virus string `json:"virus" doc:"SES virus scan, as spam" example:"PASS"`
	SPF   string `json:"spf" example:"PASS"`
	DKIM  string `json:"dkim" example:"PASS"`
	DMARC string `json:"dmarc" example:"PASS"`
}

// Summary is a received message in a list.
type Summary struct {
	ID          int64      `json:"id" example:"12"`
	Status      string     `json:"status" enum:"pending,received,failed" doc:"pending: received by SES, not fetched yet; failed: it could not be fetched (fetch_error)" example:"received"`
	FromEmail   string     `json:"from_email" example:"anna@example.org"`
	FromName    string     `json:"from_name" example:"Anna Jensen"`
	To          []string   `json:"to" doc:"The To addresses as sent" example:"[\"hello@example.com\"]"`
	Subject     string     `json:"subject" example:"Re: April news"`
	Snippet     string     `json:"snippet" doc:"The start of the text, without quoted lines" example:"Thanks, that sounds great!"`
	ReceivedAt  time.Time  `json:"received_at"`
	ContactID   *int64     `json:"contact_id" doc:"The contact with the sender's address, when there is one" example:"42"`
	Read        bool       `json:"read"`
	ReadAt      *time.Time `json:"read_at"`
	Archived    bool       `json:"archived"`
	Attachments int        `json:"attachments" doc:"Number of attachments" example:"1"`
	Replies     int        `json:"replies" doc:"Number of replies sent from Mailroom" example:"0"`
	Spam        bool       `json:"spam" doc:"SES's spam or virus scan failed"`
	Version     int64      `json:"version" example:"2"`
}

// Attachment describes an attachment (its content is a separate download).
type Attachment struct {
	Position    int    `json:"position" doc:"1-based; use it to download the attachment" example:"1"`
	Filename    string `json:"filename" example:"invoice.pdf"`
	ContentType string `json:"content_type" example:"application/pdf"`
	ContentID   string `json:"content_id" doc:"For inline images referenced as cid: in the HTML" example:""`
	Inline      bool   `json:"inline"`
	SizeBytes   int64  `json:"size_bytes" example:"48213"`
}

// Reply is a reply sent from Mailroom to a received message.
type Reply struct {
	MessageID int64      `json:"message_id" doc:"The messages row (GET /messages/{id})" example:"901"`
	To        string     `json:"to" example:"anna@example.org"`
	Subject   string     `json:"subject" example:"Re: April news"`
	Body      string     `json:"body" doc:"The text written"`
	Status    string     `json:"status" doc:"The message status: queued, sent, delivered, bounced, failed, skipped, ..." example:"sent"`
	QueuedAt  time.Time  `json:"queued_at"`
	SentAt    *time.Time `json:"sent_at"`
	Actor     string     `json:"actor" doc:"Who wrote it" example:"user:1"`
}

// Message is one received message with its bodies.
type Message struct {
	Summary
	Cc               []string     `json:"cc"`
	ReplyTo          string       `json:"reply_to" doc:"The Reply-To address, when the sender set one; replies go there"`
	MessageIDHeader  string       `json:"message_id_header" doc:"The Message-ID header (without <>)"`
	InReplyTo        string       `json:"in_reply_to" doc:"The In-Reply-To header: the Message-ID it answers"`
	SentDate         *time.Time   `json:"sent_date" doc:"The Date header"`
	Text             string       `json:"text" doc:"The plain-text body (from the HTML when the message has no text part)"`
	HTML             string       `json:"html" doc:"The HTML body as received (untrusted: show it sandboxed)"`
	SizeBytes        int64        `json:"size_bytes"`
	Verdicts         Verdicts     `json:"verdicts"`
	FetchError       string       `json:"fetch_error"`
	AttachmentsList  []Attachment `json:"attachment_list"`
	RepliesSent      []Reply      `json:"replies_sent"`
	InReplyToMessage *int64       `json:"in_reply_to_message_id" doc:"The Mailroom message (messages row) this mail answers, when its In-Reply-To is one of ours" example:"377"`
}

// ListFilter selects messages for List.
type ListFilter struct {
	Box       string // "inbox" (not archived, the default), "unread", "archived", "all"
	ContactID int64
	Query     string // matches subject, sender name or address
	BeforeID  int64  // cursor: only IDs below this
	Limit     int
}

const summaryColumns = `m.id, m.status, m.from_email, m.from_name, m.to_addrs, m.subject, m.snippet, m.received_at,
	m.contact_id, m.read_at, m.archived_at, m.spam_verdict, m.virus_verdict, m.version,
	(SELECT count(*) FROM inbound_attachments a WHERE a.inbound_id = m.id AND a.inline = 0),
	(SELECT count(*) FROM messages r WHERE r.inbound_id = m.id AND r.kind = 'reply')`

// scanSummary scans summaryColumns, followed by the extra destinations.
func scanSummary(sc interface{ Scan(...any) error }, extra ...any) (Summary, error) {
	var (
		m                   Summary
		to, received, spam  string
		virus               string
		contactID           sql.NullInt64
		readAt, archivedAtS sql.NullString
	)
	dest := []any{&m.ID, &m.Status, &m.FromEmail, &m.FromName, &to, &m.Subject, &m.Snippet, &received,
		&contactID, &readAt, &archivedAtS, &spam, &virus, &m.Version, &m.Attachments, &m.Replies}
	err := sc.Scan(append(dest, extra...)...)
	if err != nil {
		return m, err
	}
	m.To = []string{}
	_ = db.UnmarshalJSON(to, &m.To)
	m.ReceivedAt, _ = db.ParseTime(received)
	if contactID.Valid {
		id := contactID.Int64
		m.ContactID = &id
	}
	if readAt.Valid {
		t, _ := db.ParseTime(readAt.String)
		m.ReadAt, m.Read = &t, true
	}
	m.Archived = archivedAtS.Valid
	m.Spam = strings.EqualFold(spam, "FAIL") || strings.EqualFold(virus, "FAIL")
	return m, nil
}

// List returns received messages, newest first.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Summary, error) {
	where := []string{"1=1"}
	var args []any
	switch f.Box {
	case "", "inbox":
		where = append(where, "m.archived_at IS NULL")
	case "unread":
		where = append(where, "m.archived_at IS NULL", "m.read_at IS NULL")
	case "archived":
		where = append(where, "m.archived_at IS NOT NULL")
	case "all":
	default:
		return nil, core.Invalid("query.box", "must be inbox, unread, archived or all")
	}
	if f.ContactID > 0 {
		where = append(where, "m.contact_id = ?")
		args = append(args, f.ContactID)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		if len(q) > 200 {
			return nil, core.Invalid("query.q", "is longer than 200 characters")
		}
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(q)) + "%"
		where = append(where, `(lower(m.subject) LIKE ? ESCAPE '\' OR lower(m.from_email) LIKE ? ESCAPE '\' OR lower(m.from_name) LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if f.BeforeID > 0 {
		where = append(where, "m.id < ?")
		args = append(args, f.BeforeID)
	}
	if f.Limit <= 0 {
		f.Limit = api.DefaultLimit
	}
	args = append(args, f.Limit)
	rows, err := s.db.Read.QueryContext(ctx, `SELECT `+summaryColumns+` FROM inbound_messages m WHERE `+
		strings.Join(where, " AND ")+` ORDER BY m.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("inbound: list: %w", err)
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		m, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Counts are the inbox numbers for the navigation and dashboard.
type Counts struct {
	Enabled bool   `json:"enabled" doc:"Receiving is set up (INBOUND_BUCKET and INBOUND_DOMAIN)"`
	Domain  string `json:"domain" doc:"The domain whose mail Mailroom receives" example:"example.com"`
	Unread  int    `json:"unread" doc:"Unread messages in the inbox (not archived)" example:"3"`
	Inbox   int    `json:"inbox" doc:"Messages in the inbox (not archived)" example:"17"`
	Pending int    `json:"pending" doc:"Messages received by SES but not fetched yet" example:"0"`
	Failed  int    `json:"failed" doc:"Messages that could not be fetched" example:"0"`
}

// Counts returns the inbox counts.
func (s *Service) Counts(ctx context.Context) (Counts, error) {
	c := Counts{Enabled: s.Enabled(), Domain: s.cfg.InboundDomain}
	err := s.db.Read.QueryRowContext(ctx, `
		SELECT
			coalesce(sum(archived_at IS NULL AND read_at IS NULL AND status <> 'pending'), 0),
			coalesce(sum(archived_at IS NULL), 0),
			coalesce(sum(status = 'pending'), 0),
			coalesce(sum(status = 'failed'), 0)
		FROM inbound_messages`).Scan(&c.Unread, &c.Inbox, &c.Pending, &c.Failed)
	if err != nil {
		return c, fmt.Errorf("inbound: counts: %w", err)
	}
	return c, nil
}

// Get returns one message with its bodies, attachments and replies.
func (s *Service) Get(ctx context.Context, id int64) (Message, error) {
	return s.get(ctx, s.db.Read, id)
}

func (s *Service) get(ctx context.Context, q db.Querier, id int64) (Message, error) {
	var (
		m                                        Message
		cc, sentDate                             sql.NullString
		spam, virus, spf, dkim, dmarc, inReplyTo string
	)
	row := q.QueryRowContext(ctx, `SELECT `+summaryColumns+`, m.cc_addrs, m.reply_to, m.message_id_header, m.in_reply_to,
		m.sent_date, m.text_body, m.html_body, m.size_bytes, m.spam_verdict, m.virus_verdict, m.spf_verdict, m.dkim_verdict,
		m.dmarc_verdict, m.fetch_error
		FROM inbound_messages m WHERE m.id = ?`, id)
	var err error
	m.Summary, err = scanSummary(row, &cc, &m.ReplyTo, &m.MessageIDHeader, &inReplyTo, &sentDate, &m.Text, &m.HTML,
		&m.SizeBytes, &spam, &virus, &spf, &dkim, &dmarc, &m.FetchError)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, fmt.Errorf("received message %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return Message{}, fmt.Errorf("inbound: get %d: %w", id, err)
	}
	m.InReplyTo = inReplyTo
	m.Verdicts = Verdicts{Spam: spam, Virus: virus, SPF: spf, DKIM: dkim, DMARC: dmarc}
	m.Cc = []string{}
	if cc.Valid {
		_ = db.UnmarshalJSON(cc.String, &m.Cc)
	}
	if sentDate.Valid {
		if t, err := db.ParseTime(sentDate.String); err == nil {
			m.SentDate = &t
		}
	}
	if m.AttachmentsList, err = s.attachments(ctx, q, id); err != nil {
		return Message{}, err
	}
	if m.RepliesSent, err = s.replies(ctx, q, id); err != nil {
		return Message{}, err
	}
	if id, ok := ourMessageID(inReplyTo); ok {
		var exists int64
		if err := q.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ?`, id).Scan(&exists); err == nil {
			m.InReplyToMessage = &exists
		}
	}
	return m, nil
}

func (s *Service) attachments(ctx context.Context, q db.Querier, id int64) ([]Attachment, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT position, filename, content_type, content_id, inline, size_bytes
		FROM inbound_attachments WHERE inbound_id = ? ORDER BY position`, id)
	if err != nil {
		return nil, fmt.Errorf("inbound: attachments: %w", err)
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.Position, &a.Filename, &a.ContentType, &a.ContentID, &a.Inline, &a.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) replies(ctx context.Context, q db.Querier, id int64) ([]Reply, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, coalesce(to_email, ''), coalesce(json_extract(data, '$.subject'), ''), coalesce(json_extract(data, '$.body'), ''),
			status, queued_at, sent_at, coalesce(json_extract(data, '$.actor'), '')
		FROM messages WHERE inbound_id = ? AND kind = 'reply' ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("inbound: replies: %w", err)
	}
	defer rows.Close()
	out := []Reply{}
	for rows.Next() {
		var r Reply
		var queued string
		var sent sql.NullString
		if err := rows.Scan(&r.MessageID, &r.To, &r.Subject, &r.Body, &r.Status, &queued, &sent, &r.Actor); err != nil {
			return nil, err
		}
		r.QueuedAt, _ = db.ParseTime(queued)
		if sent.Valid {
			if t, err := db.ParseTime(sent.String); err == nil {
				r.SentAt = &t
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AttachmentData returns one attachment with its content.
func (s *Service) AttachmentData(ctx context.Context, id int64, position int) (Attachment, []byte, error) {
	var a Attachment
	var data []byte
	err := s.db.Read.QueryRowContext(ctx, `
		SELECT position, filename, content_type, content_id, inline, size_bytes, data
		FROM inbound_attachments WHERE inbound_id = ? AND position = ?`, id, position).
		Scan(&a.Position, &a.Filename, &a.ContentType, &a.ContentID, &a.Inline, &a.SizeBytes, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil, fmt.Errorf("attachment %d of received message %d: %w", position, id, core.ErrNotFound)
	}
	if err != nil {
		return a, nil, fmt.Errorf("inbound: attachment: %w", err)
	}
	return a, data, nil
}

// Update is a change of a message's flags; nil fields stay.
type Update struct {
	Read     *bool `json:"read,omitempty" doc:"Mark as read (true) or unread (false)" example:"true"`
	Archived *bool `json:"archived,omitempty" doc:"Move to the archive (true) or back to the inbox (false)" example:"false"`
}

// Update changes the read and archived flags. ifVersion 0 skips the
// version check.
func (s *Service) Update(ctx context.Context, id int64, u Update, ifVersion int64) (Message, error) {
	var out Message
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		now := db.FormatTime(s.clock.Now())
		sets, args, changes := []string{}, []any{}, map[string]any{}
		if u.Read != nil && *u.Read != cur.Read {
			if *u.Read {
				sets, args = append(sets, "read_at = ?"), append(args, now)
			} else {
				sets = append(sets, "read_at = NULL")
			}
			changes["read"] = *u.Read
		}
		if u.Archived != nil && *u.Archived != cur.Archived {
			if *u.Archived {
				sets, args = append(sets, "archived_at = ?"), append(args, now)
			} else {
				sets = append(sets, "archived_at = NULL")
			}
			changes["archived"] = *u.Archived
		}
		if len(sets) > 0 {
			args = append(args, now, id)
			if _, err := tx.ExecContext(ctx, `UPDATE inbound_messages SET `+strings.Join(sets, ", ")+
				`, version = version + 1, updated_at = ? WHERE id = ?`, args...); err != nil {
				return fmt.Errorf("inbound: update %d: %w", id, err)
			}
			if err := s.audit(ctx, tx, "inbound.update", id, changes); err != nil {
				return err
			}
		}
		out, err = s.get(ctx, tx, id)
		return err
	})
	return out, err
}

// Delete removes a message and its attachments for good. Replies sent to
// it stay (as messages) but lose the link.
func (s *Service) Delete(ctx context.Context, id int64, ifVersion int64) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbound_messages WHERE id = ?`, id); err != nil {
			return fmt.Errorf("inbound: delete %d: %w", id, err)
		}
		return s.audit(ctx, tx, "inbound.delete", id, map[string]any{"subject": cur.Subject})
	})
}

// ourMessageID returns the messages row ID of a Message-ID Mailroom made
// ("mr.<id>.<random>@<domain>", sending.newMessageID).
func ourMessageID(h string) (int64, bool) {
	h = strings.Trim(strings.TrimSpace(h), "<>")
	rest, ok := strings.CutPrefix(h, "mr.")
	if !ok {
		return 0, false
	}
	num, _, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, false
	}
	var id int64
	if _, err := fmt.Sscan(num, &id); err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
