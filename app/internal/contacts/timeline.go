package contacts

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Timeline item types.
const (
	TimelineConsent      = "consent"
	TimelineStatusChange = "status_change"
	TimelineTagAdded     = "tag_added"
	TimelineMessage      = "message"
	TimelineClick        = "click"
	TimelineReceived     = "received"
	// Events use their own type: delivery, bounce, complaint, reject,
	// unsubscribe, send_error.
)

// TimelineItem is one entry of a contact's timeline (C-7).
type TimelineItem struct {
	At      time.Time      `json:"at"`
	Type    string         `json:"type" doc:"consent, status_change, tag_added, message, click, received (an email from the contact, in the Inbox), or a message event type (delivery, bounce, complaint, reject, unsubscribe, send_error)" example:"message"`
	Summary string         `json:"summary" doc:"One-line human-readable description" example:"Broadcast email sent: April news"`
	Detail  map[string]any `json:"detail" doc:"Structured details; the keys depend on the type"`

	rank  int    // source order among items at the same time
	id    int64  // row id within the source
	atRaw string // stored time, for the cursor
}

// TimelinePage is a page of the timeline, newest first.
type TimelinePage struct {
	Items      []TimelineItem `json:"items"`
	NextCursor string         `json:"next_cursor" doc:"Cursor for older items; empty on the last page"`
}

// Timeline limits.
const (
	DefaultTimelineLimit = 100
	MaxTimelineLimit     = 500
)

// timelineCursor is the position after an item: (time, source rank, row id).
type timelineCursor struct {
	at   string
	rank int
	id   int64
}

func encodeTimelineCursor(it TimelineItem) string {
	return base64.RawURLEncoding.EncodeToString([]byte(it.atRaw + "|" + strconv.Itoa(it.rank) + "|" + strconv.FormatInt(it.id, 10)))
}

func decodeTimelineCursor(s string) (*timelineCursor, error) {
	if s == "" {
		return nil, nil
	}
	bad := core.Invalid("query.cursor", "is not a cursor returned by this API")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, bad
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return nil, bad
	}
	if _, err := db.ParseTime(parts[0]); err != nil {
		return nil, bad
	}
	rank, err1 := strconv.Atoi(parts[1])
	id, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil {
		return nil, bad
	}
	return &timelineCursor{at: parts[0], rank: rank, id: id}, nil
}

// timelineSource is one table feeding the timeline. query selects the
// time and row id expressions first, then the columns scan reads.
type timelineSource struct {
	rank  int
	at    string // SQL expression of the item time
	id    string // SQL expression of the row id
	query string // SELECT <at>, <id>, ... FROM ... WHERE <contact = ?>
	scan  func(rows *sql.Rows) (TimelineItem, error)
}

// Timeline returns a contact's history, newest first: consent events,
// status changes, tags added, messages, message events, clicks and email
// received from the contact.
func (s *Service) Timeline(ctx context.Context, contactID int64, cursor string, limit int) (TimelinePage, error) {
	if limit <= 0 {
		limit = DefaultTimelineLimit
	}
	limit = min(limit, MaxTimelineLimit)
	cur, err := decodeTimelineCursor(cursor)
	if err != nil {
		return TimelinePage{}, err
	}
	if err := ensureContact(ctx, s.db.Read, contactID); err != nil {
		return TimelinePage{}, err
	}
	var items []TimelineItem
	for _, src := range timelineSources {
		got, err := src.fetch(ctx, s.db.Read, contactID, cur, limit+1)
		if err != nil {
			return TimelinePage{}, err
		}
		items = append(items, got...)
	}
	slices.SortFunc(items, func(a, b TimelineItem) int {
		if c := strings.Compare(b.atRaw, a.atRaw); c != 0 {
			return c
		}
		if a.rank != b.rank {
			return b.rank - a.rank
		}
		switch {
		case a.id > b.id:
			return -1
		case a.id < b.id:
			return 1
		}
		return 0
	})
	page := TimelinePage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeTimelineCursor(page.Items[limit-1])
	}
	if page.Items == nil {
		page.Items = []TimelineItem{}
	}
	return page, nil
}

// fetch returns up to limit items of the source older than cur.
func (src timelineSource) fetch(ctx context.Context, q db.Querier, contactID int64, cur *timelineCursor, limit int) ([]TimelineItem, error) {
	query := src.query
	args := []any{contactID}
	if cur != nil {
		switch {
		case src.rank < cur.rank:
			query += " AND " + src.at + " <= ?"
			args = append(args, cur.at)
		case src.rank == cur.rank:
			query += " AND (" + src.at + " < ? OR (" + src.at + " = ? AND " + src.id + " < ?))"
			args = append(args, cur.at, cur.at, cur.id)
		default:
			query += " AND " + src.at + " < ?"
			args = append(args, cur.at)
		}
	}
	query += " ORDER BY " + src.at + " DESC, " + src.id + " DESC LIMIT ?"
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timeline: %w", err)
	}
	defer rows.Close()
	var out []TimelineItem
	for rows.Next() {
		it, err := src.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("timeline: scan: %w", err)
		}
		it.rank = src.rank
		if it.At, err = db.ParseTime(it.atRaw); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

var timelineSources = []timelineSource{
	{
		rank: 1, at: "occurred_at", id: "id",
		query: `SELECT occurred_at, id, type, source, form_id, ip, user_agent, consent_text
			FROM consent_events WHERE contact_id = ?`,
		scan: func(rows *sql.Rows) (TimelineItem, error) {
			var it TimelineItem
			var typ, source, ip, ua, text string
			var formID sql.NullInt64
			err := rows.Scan(&it.atRaw, &it.id, &typ, &source, &formID, &ip, &ua, &text)
			it.Type = TimelineConsent
			it.Summary = "Consent: " + consentSource(typ, source)
			it.Detail = map[string]any{"type": typ, "source": source, "form_id": nullInt(formID),
				"ip": ip, "user_agent": ua, "consent_text": text}
			return it, err
		},
	},
	{
		rank: 2, at: "created_at", id: "id",
		query: `SELECT created_at, id, from_status, to_status, source, message_id, actor, note
			FROM contact_status_changes WHERE contact_id = ?`,
		scan: func(rows *sql.Rows) (TimelineItem, error) {
			var it TimelineItem
			var from, to, source, actor, note string
			var msgID sql.NullInt64
			err := rows.Scan(&it.atRaw, &it.id, &from, &to, &source, &msgID, &actor, &note)
			it.Type = TimelineStatusChange
			it.Summary = fmt.Sprintf("Status changed from %s to %s (%s)", from, to, source)
			it.Detail = map[string]any{"from": from, "to": to, "source": source,
				"message_id": nullInt(msgID), "actor": actor, "note": note}
			return it, err
		},
	},
	{
		rank: 3, at: "ct.added_at", id: "ct.tag_id",
		query: `SELECT ct.added_at, ct.tag_id, t.name, ct.source
			FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id WHERE ct.contact_id = ?`,
		scan: func(rows *sql.Rows) (TimelineItem, error) {
			var it TimelineItem
			var name, source string
			err := rows.Scan(&it.atRaw, &it.id, &name, &source)
			it.Type = TimelineTagAdded
			it.Summary = "Tag added: " + name
			it.Detail = map[string]any{"tag_id": it.id, "tag": name, "source": source}
			return it, err
		},
	},
	{
		rank: 4, at: "COALESCE(sent_at, queued_at)", id: "id",
		query: `SELECT COALESCE(sent_at, queued_at), id, kind, subject, status, queued_at, sent_at, delivered_at,
				broadcast_id, enrollment_id
			FROM messages WHERE contact_id = ?`,
		scan: func(rows *sql.Rows) (TimelineItem, error) {
			var it TimelineItem
			var kind, subject, status, queued string
			var sent, delivered sql.NullString
			var broadcastID, enrollmentID sql.NullInt64
			err := rows.Scan(&it.atRaw, &it.id, &kind, &subject, &status, &queued, &sent, &delivered,
				&broadcastID, &enrollmentID)
			it.Type = TimelineMessage
			it.Summary = fmt.Sprintf("%s email %s", titleCase(kind), status)
			if subject != "" {
				it.Summary += ": " + subject
			}
			it.Detail = map[string]any{"message_id": it.id, "kind": kind, "subject": subject, "status": status,
				"queued_at": nullTime(sql.NullString{String: queued, Valid: true}), "sent_at": nullTime(sent),
				"delivered_at": nullTime(delivered), "broadcast_id": nullInt(broadcastID), "enrollment_id": nullInt(enrollmentID)}
			return it, err
		},
	},
	{
		rank: 5, at: "e.created_at", id: "e.id",
		query: `SELECT e.created_at, e.id, e.type, e.subtype, e.message_id, m.subject
			FROM events e JOIN messages m ON m.id = e.message_id WHERE m.contact_id = ?`,
		scan: func(rows *sql.Rows) (TimelineItem, error) {
			var it TimelineItem
			var typ, subtype, subject string
			var msgID int64
			err := rows.Scan(&it.atRaw, &it.id, &typ, &subtype, &msgID, &subject)
			it.Type = typ
			label := titleCase(strings.ReplaceAll(typ, "_", " "))
			if subtype != "" {
				label += " (" + subtype + ")"
			}
			it.Summary = label + ": " + subject
			it.Detail = map[string]any{"event_id": it.id, "subtype": subtype, "message_id": msgID, "subject": subject}
			return it, err
		},
	},
	{
		rank: 6, at: "k.created_at", id: "k.id",
		query: `SELECT k.created_at, k.id, l.url, k.message_id
			FROM clicks k JOIN links l ON l.id = k.link_id WHERE k.contact_id = ?`,
		scan: func(rows *sql.Rows) (TimelineItem, error) {
			var it TimelineItem
			var url string
			var msgID sql.NullInt64
			err := rows.Scan(&it.atRaw, &it.id, &url, &msgID)
			it.Type = TimelineClick
			it.Summary = "Clicked " + url
			it.Detail = map[string]any{"url": url, "message_id": nullInt(msgID)}
			return it, err
		},
	},
	{
		rank: 7, at: "received_at", id: "id",
		query: `SELECT received_at, id, subject, snippet
			FROM inbound_messages WHERE status = 'received' AND contact_id = ?`,
		scan: func(rows *sql.Rows) (TimelineItem, error) {
			var it TimelineItem
			var subject, snippet string
			err := rows.Scan(&it.atRaw, &it.id, &subject, &snippet)
			it.Type = TimelineReceived
			it.Summary = "Emailed you: " + subject
			it.Detail = map[string]any{"inbound_id": it.id, "subject": subject, "snippet": snippet}
			return it, err
		},
	},
}

// nullInt returns nil for SQL NULL, else the value.
func nullInt(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// nullTime returns nil for SQL NULL, else the parsed time (or the raw text
// when it doesn't parse).
func nullTime(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	if t, err := db.ParseTime(s.String); err == nil {
		return t
	}
	return s.String
}

// titleCase upper-cases the first ASCII letter.
func titleCase(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-('a'-'A')) + s[1:]
}

// Enrollment is one of a contact's sequence enrollments.
type Enrollment struct {
	ID               int64      `json:"id" example:"9"`
	SequenceID       int64      `json:"sequence_id" example:"2"`
	SequenceName     string     `json:"sequence_name" example:"Community welcome"`
	Status           string     `json:"status" enum:"active,completed,exited" example:"active"`
	NextStepID       *int64     `json:"next_step_id,omitempty" doc:"The step sent next (active enrollments)"`
	NextStepPosition *int       `json:"next_step_position,omitempty" doc:"1-based position of the next step"`
	NextRunAt        *time.Time `json:"next_run_at,omitempty"`
	ExitReason       string     `json:"exit_reason" example:""`
	Source           string     `json:"source" example:"form:ab12cd34ef"`
	StartedAt        time.Time  `json:"started_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

const enrollmentQuery = `
	SELECT e.id, e.sequence_id, s.name, e.status, e.next_step_id, st.position, e.next_run_at,
		e.exit_reason, e.source, e.started_at, e.updated_at, e.finished_at
	FROM enrollments e
	JOIN sequences s ON s.id = e.sequence_id
	LEFT JOIN sequence_steps st ON st.id = e.next_step_id
	WHERE e.contact_id = ?`

// Enrollments returns a contact's enrollments, newest first. beforeID and
// limit paginate like List.
func (s *Service) Enrollments(ctx context.Context, contactID, beforeID int64, limit int) ([]Enrollment, error) {
	if err := ensureContact(ctx, s.db.Read, contactID); err != nil {
		return nil, err
	}
	return listEnrollments(ctx, s.db.Read, contactID, beforeID, limit)
}

func listEnrollments(ctx context.Context, q db.Querier, contactID, beforeID int64, limit int) ([]Enrollment, error) {
	query := enrollmentQuery
	args := []any{contactID}
	if beforeID > 0 {
		query += " AND e.id < ?"
		args = append(args, beforeID)
	}
	query += " ORDER BY e.id DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list enrollments: %w", err)
	}
	defer rows.Close()
	out := []Enrollment{}
	for rows.Next() {
		var e Enrollment
		var stepID sql.NullInt64
		var pos sql.NullInt64
		var next, finished sql.NullString
		var started, updated string
		if err := rows.Scan(&e.ID, &e.SequenceID, &e.SequenceName, &e.Status, &stepID, &pos, &next,
			&e.ExitReason, &e.Source, &started, &updated, &finished); err != nil {
			return nil, fmt.Errorf("scan enrollment: %w", err)
		}
		if stepID.Valid {
			e.NextStepID = &stepID.Int64
		}
		if pos.Valid {
			p := int(pos.Int64)
			e.NextStepPosition = &p
		}
		if e.StartedAt, err = db.ParseTime(started); err != nil {
			return nil, err
		}
		if e.UpdatedAt, err = db.ParseTime(updated); err != nil {
			return nil, err
		}
		if e.NextRunAt, err = db.ParseNullTime(next); err != nil {
			return nil, err
		}
		if e.FinishedAt, err = db.ParseNullTime(finished); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// consentSource describes where a consent came from in plain words
// (the timeline summary; the raw values stay in Detail).
func consentSource(typ, source string) string {
	kind, rest, _ := strings.Cut(source, ":")
	switch {
	case typ == "import" || kind == "import":
		return "imported from a CSV file"
	case kind == "form":
		return "signed up with a form"
	case typ == "optin":
		return "confirmed by email (double opt-in)"
	case kind == "admin" || typ == "admin":
		if rest != "" && rest != "admin" {
			return "added by hand: " + rest
		}
		return "added by hand"
	case kind == "api" || typ == "api":
		if rest != "" && rest != "api" {
			return "added through the API (" + rest + ")"
		}
		return "added through the API"
	}
	if source == "" {
		return typ
	}
	return typ + " (" + source + ")"
}
