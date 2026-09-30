package sending

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Message is one messages row as returned by the API. Nullable IDs are 0
// when not set. The template data is not exposed: it can hold one-time
// links such as opt-in confirmation URLs.
type Message struct {
	ID             int64      `json:"id" example:"1042"`
	Kind           string     `json:"kind" enum:"broadcast,sequence,optin,test,alert" example:"broadcast"`
	Status         string     `json:"status" enum:"queued,sending,sent,delivered,bounced,complained,failed,skipped" example:"sent"`
	BroadcastID    int64      `json:"broadcast_id" doc:"Broadcast of a broadcast message; 0 otherwise" example:"7"`
	EnrollmentID   int64      `json:"enrollment_id" doc:"Enrollment of a sequence message; 0 otherwise" example:"0"`
	StepID         int64      `json:"step_id" doc:"Sequence step of a sequence message; 0 otherwise" example:"0"`
	EmailID        int64      `json:"email_id" doc:"Email the content comes from; 0 for system templates" example:"12"`
	SystemTemplate string     `json:"system_template" doc:"optin_confirm, resubscribe_confirm or alert; empty when email_id is set" example:""`
	ContactID      int64      `json:"contact_id" doc:"Recipient contact; 0 for alerts, tests to an address, and erased contacts" example:"311"`
	ToEmail        string     `json:"to_email" doc:"Recipient address; empty after a GDPR erase" example:"alice@example.org"`
	Subject        string     `json:"subject" doc:"Rendered subject, stored when the message is sent" example:"October news"`
	Attempts       int        `json:"attempts" doc:"Provider calls made so far" example:"1"`
	NextAttemptAt  time.Time  `json:"next_attempt_at" doc:"Earliest time the message is (re)tried while queued"`
	SESMessageID   string     `json:"ses_message_id" doc:"SES MessageId once sent" example:"0102018f-example"`
	Error          string     `json:"error" doc:"Why the message was skipped or failed, or the last retryable error" example:""`
	QueuedAt       time.Time  `json:"queued_at"`
	SentAt         *time.Time `json:"sent_at,omitempty"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Event is one events row (delivery, bounce, complaint, reject,
// unsubscribe, send_error).
type Event struct {
	ID        int64           `json:"id" example:"88"`
	MessageID int64           `json:"message_id" doc:"0 when the message was deleted" example:"1042"`
	Type      string          `json:"type" enum:"delivery,bounce,complaint,reject,unsubscribe,send_error" example:"bounce"`
	Subtype   string          `json:"subtype" doc:"bounce: permanent, transient or undetermined; unsubscribe: link or one_click; send_error: the error code" example:"permanent"`
	Detail    json.RawMessage `json:"detail" doc:"Raw event data, without the message headers SES copies into its events (emptied after 90 days or by erase)"`
	CreatedAt time.Time       `json:"created_at"`
}

// Click is one tracked click (K-1) with the link's URL.
type Click struct {
	ID          int64     `json:"id" example:"501"`
	LinkID      int64     `json:"link_id" example:"9"`
	URL         string    `json:"url" example:"https://example.com/offer"`
	MessageID   int64     `json:"message_id" doc:"0 when the message was deleted" example:"1042"`
	ContactID   int64     `json:"contact_id" doc:"0 after the contact was erased" example:"311"`
	BroadcastID int64     `json:"broadcast_id" doc:"Broadcast of the clicked message; 0 otherwise" example:"7"`
	CreatedAt   time.Time `json:"created_at"`
}

// MessageDetail is a message with its events and clicks.
type MessageDetail struct {
	Message
	Events []Event `json:"events" doc:"Events of this message, oldest first"`
	Clicks []Click `json:"clicks" doc:"Clicks on links in this message, oldest first"`
}

// MessageFilter filters ListMessages. Zero values mean "any". BeforeID is
// the keyset cursor (newest first).
type MessageFilter struct {
	Kind, Status                                 string
	BroadcastID, ContactID, EnrollmentID, StepID int64
	BeforeID                                     int64
	Limit                                        int
}

// EventFilter filters ListEvents.
type EventFilter struct {
	Type      string
	MessageID int64
	Since     time.Time // zero = any
	BeforeID  int64
	Limit     int
}

// ClickFilter filters ListClicks.
type ClickFilter struct {
	ContactID, BroadcastID, LinkID int64
	BeforeID                       int64
	Limit                          int
}

// where builds "WHERE a AND b" from optional conditions.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, arg any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, arg)
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

func limitOf(n int) string {
	if n <= 0 {
		n = 50
	}
	return " LIMIT " + strconv.Itoa(n)
}

const messageColumns = `m.id, m.kind, m.status, m.broadcast_id, m.enrollment_id, m.step_id, m.email_id, m.system_template,
	m.contact_id, m.to_email, m.subject, m.attempts, m.next_attempt_at, m.ses_message_id, m.error,
	m.queued_at, m.sent_at, m.delivered_at, m.updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanMessage(r scanner) (Message, error) {
	var (
		m                          Message
		bID, eID, stID, emID, cID  sql.NullInt64
		to, sesID, sent, delivered sql.NullString
		next, queued, updated      string
	)
	if err := r.Scan(&m.ID, &m.Kind, &m.Status, &bID, &eID, &stID, &emID, &m.SystemTemplate,
		&cID, &to, &m.Subject, &m.Attempts, &next, &sesID, &m.Error, &queued, &sent, &delivered, &updated); err != nil {
		return Message{}, err
	}
	m.BroadcastID, m.EnrollmentID, m.StepID, m.EmailID, m.ContactID = bID.Int64, eID.Int64, stID.Int64, emID.Int64, cID.Int64
	m.ToEmail, m.SESMessageID = to.String, sesID.String
	var err error
	if m.NextAttemptAt, err = db.ParseTime(next); err != nil {
		return Message{}, err
	}
	if m.QueuedAt, err = db.ParseTime(queued); err != nil {
		return Message{}, err
	}
	if m.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return Message{}, err
	}
	if m.SentAt, err = db.ParseNullTime(sent); err != nil {
		return Message{}, err
	}
	if m.DeliveredAt, err = db.ParseNullTime(delivered); err != nil {
		return Message{}, err
	}
	return m, nil
}

// ListMessages returns messages, newest first.
func (s *Service) ListMessages(ctx context.Context, f MessageFilter) ([]Message, error) {
	var w where
	if f.Kind != "" {
		w.add("m.kind = ?", f.Kind)
	}
	if f.Status != "" {
		w.add("m.status = ?", f.Status)
	}
	for _, c := range []struct {
		col string
		v   int64
	}{{"m.broadcast_id", f.BroadcastID}, {"m.contact_id", f.ContactID}, {"m.enrollment_id", f.EnrollmentID}, {"m.step_id", f.StepID}} {
		if c.v != 0 {
			w.add(c.col+" = ?", c.v)
		}
	}
	if f.BeforeID > 0 {
		w.add("m.id < ?", f.BeforeID)
	}
	rows, err := s.db.Read.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages m`+w.sql()+` ORDER BY m.id DESC`+limitOf(f.Limit), w.args...)
	if err != nil {
		return nil, fmt.Errorf("sending: list messages: %w", err)
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("sending: list messages: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMessage returns one message with its events and clicks.
func (s *Service) GetMessage(ctx context.Context, id int64) (MessageDetail, error) {
	m, err := scanMessage(s.db.Read.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages m WHERE m.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MessageDetail{}, fmt.Errorf("message %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return MessageDetail{}, fmt.Errorf("sending: get message %d: %w", id, err)
	}
	d := MessageDetail{Message: m}
	if d.Events, err = s.queryEvents(ctx, ` WHERE e.message_id = ? ORDER BY e.id`, id); err != nil {
		return MessageDetail{}, err
	}
	if d.Clicks, err = s.queryClicks(ctx, ` WHERE k.message_id = ? ORDER BY k.id`, id); err != nil {
		return MessageDetail{}, err
	}
	return d, nil
}

// GetEvent returns one event; a missing ID wraps ErrNotFound.
func (s *Service) GetEvent(ctx context.Context, id int64) (Event, error) {
	es, err := s.queryEvents(ctx, ` WHERE e.id = ?`, id)
	if err != nil {
		return Event{}, err
	}
	if len(es) == 0 {
		return Event{}, fmt.Errorf("event %d: %w", id, core.ErrNotFound)
	}
	return es[0], nil
}

// GetClick returns one click; a missing ID wraps ErrNotFound.
func (s *Service) GetClick(ctx context.Context, id int64) (Click, error) {
	cs, err := s.queryClicks(ctx, ` WHERE k.id = ?`, id)
	if err != nil {
		return Click{}, err
	}
	if len(cs) == 0 {
		return Click{}, fmt.Errorf("click %d: %w", id, core.ErrNotFound)
	}
	return cs[0], nil
}

// ListEvents returns events, newest first.
func (s *Service) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	var w where
	if f.Type != "" {
		w.add("e.type = ?", f.Type)
	}
	if f.MessageID != 0 {
		w.add("e.message_id = ?", f.MessageID)
	}
	if !f.Since.IsZero() {
		w.add("e.created_at >= ?", db.FormatTime(f.Since))
	}
	if f.BeforeID > 0 {
		w.add("e.id < ?", f.BeforeID)
	}
	return s.queryEvents(ctx, w.sql()+` ORDER BY e.id DESC`+limitOf(f.Limit), w.args...)
}

func (s *Service) queryEvents(ctx context.Context, tail string, args ...any) ([]Event, error) {
	rows, err := s.db.Read.QueryContext(ctx,
		`SELECT e.id, e.message_id, e.type, e.subtype, e.detail, e.created_at FROM events e`+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("sending: list events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var (
			e              Event
			msgID          sql.NullInt64
			detail, create string
		)
		if err := rows.Scan(&e.ID, &msgID, &e.Type, &e.Subtype, &detail, &create); err != nil {
			return nil, fmt.Errorf("sending: list events: %w", err)
		}
		e.MessageID, e.Detail = msgID.Int64, json.RawMessage(detail)
		if e.CreatedAt, err = db.ParseTime(create); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListClicks returns clicks with their URL, newest first.
func (s *Service) ListClicks(ctx context.Context, f ClickFilter) ([]Click, error) {
	var w where
	if f.ContactID != 0 {
		w.add("k.contact_id = ?", f.ContactID)
	}
	if f.BroadcastID != 0 {
		w.add("m.broadcast_id = ?", f.BroadcastID)
	}
	if f.LinkID != 0 {
		w.add("k.link_id = ?", f.LinkID)
	}
	if f.BeforeID > 0 {
		w.add("k.id < ?", f.BeforeID)
	}
	return s.queryClicks(ctx, w.sql()+` ORDER BY k.id DESC`+limitOf(f.Limit), w.args...)
}

func (s *Service) queryClicks(ctx context.Context, tail string, args ...any) ([]Click, error) {
	rows, err := s.db.Read.QueryContext(ctx, `
		SELECT k.id, k.link_id, l.url, k.message_id, k.contact_id, m.broadcast_id, k.created_at
		FROM clicks k JOIN links l ON l.id = k.link_id LEFT JOIN messages m ON m.id = k.message_id`+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("sending: list clicks: %w", err)
	}
	defer rows.Close()
	out := []Click{}
	for rows.Next() {
		var (
			c               Click
			msgID, cID, bID sql.NullInt64
			created         string
		)
		if err := rows.Scan(&c.ID, &c.LinkID, &c.URL, &msgID, &cID, &bID, &created); err != nil {
			return nil, fmt.Errorf("sending: list clicks: %w", err)
		}
		c.MessageID, c.ContactID, c.BroadcastID = msgID.Int64, cID.Int64, bID.Int64
		if c.CreatedAt, err = db.ParseTime(created); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
