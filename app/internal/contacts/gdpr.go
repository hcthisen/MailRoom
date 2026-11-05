package contacts

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Export is everything Mailroom holds about one contact (GDPR export, C-5).
// It covers the same data Erase removes or anonymises: the contact's own
// rows, every message to its address (also test sends and alerts, which
// carry no contact) and test sends rendered with its data, the SES events
// of the address, pending (and confirmed) opt-ins with the submitted
// values, and form submissions.
type Export struct {
	ExportedAt    time.Time       `json:"exported_at"`
	Contact       Contact         `json:"contact"`
	Tags          []ExportTag     `json:"tags"`
	ConsentLog    []ConsentRecord `json:"consent_log"`
	StatusHistory []StatusRecord  `json:"status_history"`
	Messages      []ExportMessage `json:"messages" doc:"Messages to the contact or its address, and test sends that used its data"`
	// OtherEvents are SES events of the address that belong to none of
	// Messages (e.g. of a message whose row is gone).
	OtherEvents     []ExportEvent        `json:"other_events" doc:"SES events of the address not linked to one of the messages"`
	Clicks          []ExportClick        `json:"clicks"`
	Opens           []ExportOpen         `json:"opens"`
	Enrollments     []Enrollment         `json:"enrollments"`
	PendingOptins   []ExportPendingOptin `json:"pending_optins" doc:"Confirmation requests (double opt-in, resubscribe) with what was submitted, the IP and user agent, open or confirmed"`
	FormSubmissions []ExportSubmission   `json:"form_submissions"`
	Received        []ExportReceived     `json:"received" doc:"Email received from the contact or its address (the Inbox); attachments are listed, not included"`
}

// ExportReceived is one email received from the contact (inbound_messages).
type ExportReceived struct {
	ID          int64     `json:"id"`
	FromEmail   string    `json:"from_email"`
	FromName    string    `json:"from_name"`
	Subject     string    `json:"subject"`
	ReceivedAt  time.Time `json:"received_at"`
	Text        string    `json:"text"`
	Attachments []string  `json:"attachments" doc:"File names"`
}

// ExportPendingOptin is one confirmation request (pending_optins): what a
// form submission asked for, kept until confirmed or expired.
type ExportPendingOptin struct {
	ID          int64           `json:"id"`
	Kind        string          `json:"kind" enum:"optin,resubscribe"`
	FormID      *int64          `json:"form_id,omitempty"`
	Submitted   json.RawMessage `json:"submitted" doc:"The submitted fields and the form's tags and sequence at the time"`
	IP          string          `json:"ip"`
	UserAgent   string          `json:"user_agent"`
	ConsentText string          `json:"consent_text"`
	CreatedAt   time.Time       `json:"created_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	ConfirmedAt *time.Time      `json:"confirmed_at,omitempty"`
}

// ExportSubmission is one form submission of the contact.
type ExportSubmission struct {
	FormID    int64     `json:"form_id"`
	Outcome   string    `json:"outcome"`
	CreatedAt time.Time `json:"created_at"`
}

// ExportTag is one of the contact's tags.
type ExportTag struct {
	Name    string    `json:"name"`
	Source  string    `json:"source"`
	AddedAt time.Time `json:"added_at"`
}

// ConsentRecord is one consent log entry (C-3).
type ConsentRecord struct {
	ID          int64     `json:"id"`
	Type        string    `json:"type"`
	Source      string    `json:"source"`
	FormID      *int64    `json:"form_id,omitempty"`
	IP          string    `json:"ip"`
	UserAgent   string    `json:"user_agent"`
	ConsentText string    `json:"consent_text"`
	OccurredAt  time.Time `json:"occurred_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// StatusRecord is one status change.
type StatusRecord struct {
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Source    string    `json:"source"`
	MessageID *int64    `json:"message_id,omitempty"`
	Actor     string    `json:"actor"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

// ExportMessage is one email sent (or queued) to the contact.
type ExportMessage struct {
	ID          int64         `json:"id"`
	Kind        string        `json:"kind"`
	ToEmail     string        `json:"to_email"`
	Subject     string        `json:"subject"`
	Status      string        `json:"status"`
	QueuedAt    time.Time     `json:"queued_at"`
	SentAt      *time.Time    `json:"sent_at,omitempty"`
	DeliveredAt *time.Time    `json:"delivered_at,omitempty"`
	Events      []ExportEvent `json:"events"`
}

// ExportEvent is a delivery, bounce, complaint or other event of a message.
type ExportEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	Detail    json.RawMessage `json:"detail" doc:"The raw provider event (emptied after 90 days)"`
	CreatedAt time.Time       `json:"created_at"`
}

// ExportClick is one tracked click.
type ExportClick struct {
	URL       string    `json:"url"`
	MessageID *int64    `json:"message_id,omitempty"`
	At        time.Time `json:"at"`
}

// ExportOpen is one tracked open.
type ExportOpen struct {
	MessageID *int64    `json:"message_id,omitempty"`
	At        time.Time `json:"at"`
}

// Export collects one contact's data, consent log, status history,
// messages with their events, other SES events of the address, clicks,
// opens, enrollments, pending opt-ins and form submissions, read in one
// consistent snapshot. Messages and events are selected like Erase does
// (messagesOf, eventsOf), so the export holds everything an erase removes.
func (s *Service) Export(ctx context.Context, id int64) (Export, error) {
	tx, err := s.db.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Export{}, fmt.Errorf("export: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := s.store.Get(ctx, tx, id)
	if err != nil {
		return Export{}, err
	}
	ex := Export{ExportedAt: s.clock.Now(), Contact: fromCore(c)}
	steps := []func(context.Context, *sql.Tx, int64, *Export) error{
		exportTags, exportConsent, exportStatus, exportMessages, exportClicks, exportOpens,
		exportPendingOptins, exportSubmissions, exportReceived,
	}
	for _, step := range steps {
		if err := step(ctx, tx, id, &ex); err != nil {
			return Export{}, fmt.Errorf("export contact %d: %w", id, err)
		}
	}
	if ex.Enrollments, err = listEnrollments(ctx, tx, id, 0, 0); err != nil {
		return Export{}, err
	}
	return ex, nil
}

// each runs query and calls scan for every row.
func each(ctx context.Context, q db.Querier, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportTags(ctx context.Context, tx *sql.Tx, id int64, ex *Export) error {
	ex.Tags = []ExportTag{}
	return each(ctx, tx, `SELECT t.name, ct.source, ct.added_at FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id
		WHERE ct.contact_id = ? ORDER BY t.name COLLATE NOCASE`, []any{id}, func(r *sql.Rows) error {
		var t ExportTag
		var added string
		if err := r.Scan(&t.Name, &t.Source, &added); err != nil {
			return err
		}
		var err error
		t.AddedAt, err = db.ParseTime(added)
		ex.Tags = append(ex.Tags, t)
		return err
	})
}

func exportConsent(ctx context.Context, tx *sql.Tx, id int64, ex *Export) error {
	ex.ConsentLog = []ConsentRecord{}
	return each(ctx, tx, `SELECT id, type, source, form_id, ip, user_agent, consent_text, occurred_at, created_at
		FROM consent_events WHERE contact_id = ? ORDER BY id`, []any{id}, func(r *sql.Rows) error {
		var c ConsentRecord
		var formID sql.NullInt64
		var occurred, created string
		if err := r.Scan(&c.ID, &c.Type, &c.Source, &formID, &c.IP, &c.UserAgent, &c.ConsentText, &occurred, &created); err != nil {
			return err
		}
		c.FormID = optID(formID)
		var err error
		if c.OccurredAt, err = db.ParseTime(occurred); err != nil {
			return err
		}
		c.CreatedAt, err = db.ParseTime(created)
		ex.ConsentLog = append(ex.ConsentLog, c)
		return err
	})
}

func exportStatus(ctx context.Context, tx *sql.Tx, id int64, ex *Export) error {
	ex.StatusHistory = []StatusRecord{}
	return each(ctx, tx, `SELECT id, from_status, to_status, source, message_id, actor, note, created_at
		FROM contact_status_changes WHERE contact_id = ? ORDER BY id`, []any{id}, func(r *sql.Rows) error {
		var s StatusRecord
		var msgID sql.NullInt64
		var created string
		if err := r.Scan(&s.ID, &s.From, &s.To, &s.Source, &msgID, &s.Actor, &s.Note, &created); err != nil {
			return err
		}
		s.MessageID = optID(msgID)
		var err error
		s.CreatedAt, err = db.ParseTime(created)
		ex.StatusHistory = append(ex.StatusHistory, s)
		return err
	})
}

func exportMessages(ctx context.Context, tx *sql.Tx, id int64, ex *Export) error {
	ex.Messages, ex.OtherEvents = []ExportMessage{}, []ExportEvent{}
	email := ex.Contact.Email
	of := []any{id, core.EmailHash(email), email} // messagesOf / eventsOf
	index := map[int64]int{}
	err := each(ctx, tx, `SELECT id, kind, COALESCE(to_email, ''), subject, status, queued_at, sent_at, delivered_at
		FROM messages WHERE `+messagesOf+` ORDER BY id`, of, func(r *sql.Rows) error {
		var m ExportMessage
		var queued string
		var sent, delivered sql.NullString
		if err := r.Scan(&m.ID, &m.Kind, &m.ToEmail, &m.Subject, &m.Status, &queued, &sent, &delivered); err != nil {
			return err
		}
		var err error
		if m.QueuedAt, err = db.ParseTime(queued); err != nil {
			return err
		}
		if m.SentAt, err = db.ParseNullTime(sent); err != nil {
			return err
		}
		if m.DeliveredAt, err = db.ParseNullTime(delivered); err != nil {
			return err
		}
		m.Events = []ExportEvent{}
		index[m.ID] = len(ex.Messages)
		ex.Messages = append(ex.Messages, m)
		return nil
	})
	if err != nil {
		return err
	}
	return each(ctx, tx, `SELECT e.message_id, e.type, e.subtype, e.detail, e.created_at
		FROM events e WHERE `+eventsOf+` ORDER BY e.id`, of, func(r *sql.Rows) error {
		var msgID sql.NullInt64
		var e ExportEvent
		var detail, created string
		if err := r.Scan(&msgID, &e.Type, &e.Subtype, &detail, &created); err != nil {
			return err
		}
		e.Detail = json.RawMessage(detail)
		var err error
		if e.CreatedAt, err = db.ParseTime(created); err != nil {
			return err
		}
		if i, ok := index[msgID.Int64]; ok && msgID.Valid {
			ex.Messages[i].Events = append(ex.Messages[i].Events, e)
		} else {
			ex.OtherEvents = append(ex.OtherEvents, e)
		}
		return nil
	})
}

func exportPendingOptins(ctx context.Context, tx *sql.Tx, id int64, ex *Export) error {
	ex.PendingOptins = []ExportPendingOptin{}
	return each(ctx, tx, `SELECT id, kind, form_id, payload, ip, user_agent, consent_text, created_at, expires_at, confirmed_at
		FROM pending_optins WHERE contact_id = ? ORDER BY id`, []any{id}, func(r *sql.Rows) error {
		var p ExportPendingOptin
		var formID sql.NullInt64
		var payload, created, expires string
		var confirmed sql.NullString
		if err := r.Scan(&p.ID, &p.Kind, &formID, &payload, &p.IP, &p.UserAgent, &p.ConsentText, &created, &expires, &confirmed); err != nil {
			return err
		}
		p.FormID, p.Submitted = optID(formID), json.RawMessage(payload)
		var err error
		if p.CreatedAt, err = db.ParseTime(created); err != nil {
			return err
		}
		if p.ExpiresAt, err = db.ParseTime(expires); err != nil {
			return err
		}
		p.ConfirmedAt, err = db.ParseNullTime(confirmed)
		ex.PendingOptins = append(ex.PendingOptins, p)
		return err
	})
}

func exportSubmissions(ctx context.Context, tx *sql.Tx, id int64, ex *Export) error {
	ex.FormSubmissions = []ExportSubmission{}
	return each(ctx, tx, `SELECT form_id, outcome, created_at FROM form_submissions WHERE contact_id = ? ORDER BY id`,
		[]any{id}, func(r *sql.Rows) error {
			var s ExportSubmission
			var created string
			if err := r.Scan(&s.FormID, &s.Outcome, &created); err != nil {
				return err
			}
			var err error
			s.CreatedAt, err = db.ParseTime(created)
			ex.FormSubmissions = append(ex.FormSubmissions, s)
			return err
		})
}

func exportReceived(ctx context.Context, tx *sql.Tx, id int64, ex *Export) error {
	ex.Received = []ExportReceived{}
	err := each(ctx, tx, `SELECT id, from_email, from_name, subject, received_at, text_body FROM inbound_messages
		WHERE contact_id = ?1 OR from_email = (SELECT email FROM contacts WHERE id = ?1) ORDER BY id`,
		[]any{id}, func(r *sql.Rows) error {
			var m ExportReceived
			var at string
			if err := r.Scan(&m.ID, &m.FromEmail, &m.FromName, &m.Subject, &at, &m.Text); err != nil {
				return err
			}
			var err error
			m.ReceivedAt, err = db.ParseTime(at)
			m.Attachments = []string{}
			ex.Received = append(ex.Received, m)
			return err
		})
	if err != nil {
		return err
	}
	for i := range ex.Received {
		err := each(ctx, tx, `SELECT filename FROM inbound_attachments WHERE inbound_id = ? ORDER BY position`,
			[]any{ex.Received[i].ID}, func(r *sql.Rows) error {
				var name string
				err := r.Scan(&name)
				ex.Received[i].Attachments = append(ex.Received[i].Attachments, name)
				return err
			})
		if err != nil {
			return err
		}
	}
	return nil
}

func exportClicks(ctx context.Context, tx *sql.Tx, id int64, ex *Export) error {
	ex.Clicks = []ExportClick{}
	return each(ctx, tx, `SELECT l.url, k.message_id, k.created_at FROM clicks k JOIN links l ON l.id = k.link_id
		WHERE k.contact_id = ? ORDER BY k.id`, []any{id}, func(r *sql.Rows) error {
		var c ExportClick
		var msgID sql.NullInt64
		var at string
		if err := r.Scan(&c.URL, &msgID, &at); err != nil {
			return err
		}
		c.MessageID = optID(msgID)
		var err error
		c.At, err = db.ParseTime(at)
		ex.Clicks = append(ex.Clicks, c)
		return err
	})
}

func exportOpens(ctx context.Context, tx *sql.Tx, id int64, ex *Export) error {
	ex.Opens = []ExportOpen{}
	return each(ctx, tx, `SELECT message_id, created_at FROM opens WHERE contact_id = ? ORDER BY id`, []any{id}, func(r *sql.Rows) error {
		var o ExportOpen
		var msgID sql.NullInt64
		var at string
		if err := r.Scan(&msgID, &at); err != nil {
			return err
		}
		o.MessageID = optID(msgID)
		var err error
		o.At, err = db.ParseTime(at)
		ex.Opens = append(ex.Opens, o)
		return err
	})
}

func optID(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}
