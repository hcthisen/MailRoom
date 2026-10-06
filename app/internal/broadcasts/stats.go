package broadcasts

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// BroadcastLinkClicks are the clicks on one link of a broadcast.
type BroadcastLinkClicks struct {
	URL    string `json:"url" example:"https://example.com/offer"`
	Clicks int64  `json:"clicks" doc:"All clicks" example:"57"`
	Unique int64  `json:"unique" doc:"Recipients who clicked it" example:"41"`
}

// BroadcastRates are fractions (0-1) of the sent messages.
type BroadcastRates struct {
	Delivery    float64 `json:"delivery" doc:"delivered / sent" example:"0.98"`
	Bounce      float64 `json:"bounce" doc:"bounced / sent" example:"0.01"`
	Complaint   float64 `json:"complaint" doc:"complained / sent" example:"0"`
	Unsubscribe float64 `json:"unsubscribe" doc:"unsubscribed / sent" example:"0.002"`
	Click       float64 `json:"click" doc:"unique_clicks / sent" example:"0.035"`
}

// BroadcastStats are a broadcast's numbers (B-8). Test sends are not included.
type BroadcastStats struct {
	BroadcastID   int64                 `json:"broadcast_id" example:"7"`
	Status        string                `json:"status" enum:"draft,scheduled,sending,paused,sent,cancelled" example:"sent"`
	Recipients    int64                 `json:"recipients" doc:"Messages queued by the snapshot" example:"1180"`
	Queued        int64                 `json:"queued" doc:"Messages not sent yet (queued or being sent)" example:"0"`
	Sent          int64                 `json:"sent" doc:"Messages accepted by SES (incl. those delivered, bounced or complained about later)" example:"1175"`
	Delivered     int64                 `json:"delivered" example:"1160"`
	Bounced       int64                 `json:"bounced" doc:"Messages that hard-bounced" example:"12"`
	Complained    int64                 `json:"complained" doc:"Messages the recipient reported as spam (not-spam feedback reports excluded)" example:"1"`
	Failed        int64                 `json:"failed" doc:"Messages the provider refused or that failed after retries" example:"2"`
	Skipped       int64                 `json:"skipped" doc:"Messages not sent because the contact became inactive or suppressed, or the broadcast was cancelled" example:"3"`
	Unsubscribed  int64                 `json:"unsubscribed" doc:"Recipients who unsubscribed from this broadcast" example:"4"`
	UniqueClicks  int64                 `json:"unique_clicks" doc:"Recipients who clicked at least one link" example:"41"`
	Clicks        int64                 `json:"clicks" doc:"All clicks" example:"70"`
	ClicksPerLink []BroadcastLinkClicks `json:"clicks_per_link" doc:"Clicks per link, most clicked first"`
	Rates         BroadcastRates        `json:"rates"`
	MissingTags   []string              `json:"missing_tags" doc:"Tag names the audience segment refers to that no tag has" example:"[]"`
}

// Stats returns the broadcast's numbers.
func (s *Service) Stats(ctx context.Context, id int64) (BroadcastStats, error) {
	b, err := s.get(ctx, nil, id)
	if err != nil {
		return BroadcastStats{}, err
	}
	st := BroadcastStats{BroadcastID: id, Status: b.Status, ClicksPerLink: []BroadcastLinkClicks{}}
	q := s.db.Read
	err = q.QueryRowContext(ctx, `
		SELECT count(*),
		  IFNULL(sum(m.status IN ('queued', 'sending')), 0),
		  IFNULL(sum(m.sent_at IS NOT NULL), 0),
		  IFNULL(sum(m.status = 'delivered' OR m.delivered_at IS NOT NULL), 0),
		  IFNULL(sum(m.status = 'bounced' OR `+hardBounceExpr+`), 0),
		  IFNULL(sum(`+complaintExpr+`), 0),
		  IFNULL(sum(m.status = 'failed'), 0),
		  IFNULL(sum(m.status = 'skipped'), 0)
		FROM messages m WHERE m.broadcast_id = ?`, id).
		Scan(&st.Recipients, &st.Queued, &st.Sent, &st.Delivered, &st.Bounced, &st.Complained, &st.Failed, &st.Skipped)
	if err != nil {
		return BroadcastStats{}, fmt.Errorf("broadcasts: stats %d: %w", id, err)
	}
	if st.Unsubscribed, err = s.unsubscribedCount(ctx, id); err != nil {
		return BroadcastStats{}, err
	}
	if b.SnapshotDoneAt != nil {
		st.Recipients = b.RecipientsTotal
	}
	if err := q.QueryRowContext(ctx, `
		SELECT count(*), count(DISTINCT k.message_id) FROM clicks k JOIN messages m ON m.id = k.message_id
		WHERE m.broadcast_id = ?`, id).Scan(&st.Clicks, &st.UniqueClicks); err != nil {
		return BroadcastStats{}, fmt.Errorf("broadcasts: click stats %d: %w", id, err)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT l.url, count(*) AS n, count(DISTINCT k.message_id)
		FROM clicks k JOIN messages m ON m.id = k.message_id JOIN links l ON l.id = k.link_id
		WHERE m.broadcast_id = ? GROUP BY l.id ORDER BY n DESC, l.url`, id)
	if err != nil {
		return BroadcastStats{}, fmt.Errorf("broadcasts: link stats %d: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var lc BroadcastLinkClicks
		if err := rows.Scan(&lc.URL, &lc.Clicks, &lc.Unique); err != nil {
			return BroadcastStats{}, err
		}
		st.ClicksPerLink = append(st.ClicksPerLink, lc)
	}
	if err := rows.Err(); err != nil {
		return BroadcastStats{}, err
	}
	st.Rates = BroadcastRates{
		Delivery: ratio(st.Delivered, st.Sent), Bounce: ratio(st.Bounced, st.Sent), Complaint: ratio(st.Complained, st.Sent),
		Unsubscribe: ratio(st.Unsubscribed, st.Sent), Click: ratio(st.UniqueClicks, st.Sent),
	}
	if st.MissingTags, err = s.missingTags(ctx, b.Audience); err != nil {
		return BroadcastStats{}, err
	}
	return st, nil
}

// unsubscribedCount counts the broadcast's messages that led to an
// unsubscribe: an unsubscribe event, or a status change to unsubscribed
// that refers to the message.
//
// contact_status_changes has no index on message_id, so a subquery
// correlated per message would scan that table once for every message.
// The broadcast's message IDs are instead materialised once (a
// non-correlated IN list) and each table is read in a single pass.
func (s *Service) unsubscribedCount(ctx context.Context, id int64) (int64, error) {
	var n int64
	err := s.db.Read.QueryRowContext(ctx, `
		SELECT count(*) FROM (
		  SELECT message_id FROM events
		  WHERE type = 'unsubscribe' AND message_id IN (SELECT id FROM messages WHERE broadcast_id = ?1)
		  UNION
		  SELECT message_id FROM contact_status_changes
		  WHERE to_status = 'unsubscribed' AND message_id IN (SELECT id FROM messages WHERE broadcast_id = ?1)
		)`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("broadcasts: unsubscribe stats %d: %w", id, err)
	}
	return n, nil
}

func ratio(n, d int64) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// BroadcastRecipient is one message of a broadcast.
type BroadcastRecipient struct {
	MessageID   int64      `json:"message_id" example:"9001"`
	ContactID   *int64     `json:"contact_id" doc:"null after the contact was erased" example:"42"`
	Email       *string    `json:"email" doc:"The recipient address; null after erase" example:"ann@example.com"`
	Status      string     `json:"status" enum:"queued,sending,sent,delivered,bounced,complained,failed,skipped" example:"delivered"`
	Error       string     `json:"error" doc:"Why the message failed or was skipped" example:""`
	Attempts    int        `json:"attempts" example:"1"`
	QueuedAt    time.Time  `json:"queued_at"`
	SentAt      *time.Time `json:"sent_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
}

// RecipientFilter pages through a broadcast's recipients.
type RecipientFilter struct {
	Status  string // "" = any
	AfterID int64  // message IDs greater than this
	Limit   int    // callers pass page size + 1
}

// Recipients returns the broadcast's messages ordered by message ID.
func (s *Service) Recipients(ctx context.Context, id int64, f RecipientFilter) ([]BroadcastRecipient, error) {
	if _, err := s.get(ctx, nil, id); err != nil {
		return nil, err
	}
	if f.Status != "" && !core.MessageStatus(f.Status).Valid() {
		return nil, core.Invalid("status", "is not a message status")
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q := `SELECT id, contact_id, to_email, status, error, attempts, queued_at, sent_at, delivered_at
		FROM messages WHERE broadcast_id = ? AND id > ?`
	args := []any{id, f.AfterID}
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	q += ` ORDER BY id LIMIT ?`
	rows, err := s.db.Read.QueryContext(ctx, q, append(args, f.Limit)...)
	if err != nil {
		return nil, fmt.Errorf("broadcasts: recipients %d: %w", id, err)
	}
	defer rows.Close()
	var out []BroadcastRecipient
	for rows.Next() {
		var r BroadcastRecipient
		var contactID sql.NullInt64
		var email, sent, delivered sql.NullString
		var queued string
		if err := rows.Scan(&r.MessageID, &contactID, &email, &r.Status, &r.Error, &r.Attempts, &queued, &sent, &delivered); err != nil {
			return nil, err
		}
		if contactID.Valid {
			r.ContactID = &contactID.Int64
		}
		if email.Valid {
			r.Email = &email.String
		}
		if r.QueuedAt, err = db.ParseTime(queued); err != nil {
			return nil, err
		}
		if r.SentAt, err = db.ParseNullTime(sent); err != nil {
			return nil, err
		}
		if r.DeliveredAt, err = db.ParseNullTime(delivered); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
