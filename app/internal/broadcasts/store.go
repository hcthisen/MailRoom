package broadcasts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// selectColumns reads a broadcast joined with its email. The body is
// replaced by an empty string in lists.
const selectColumns = `b.id, b.name, b.status, b.audience, b.from_name, b.reply_to, b.paused_reason,
	b.scheduled_at, b.started_at, b.finished_at, b.recipients_total, b.snapshot_job_id, b.snapshot_done_at,
	b.version, b.created_at, b.updated_at,
	e.id, e.subject, e.preheader, e.format, %s, e.version`

const fromJoin = ` FROM broadcasts b JOIN emails e ON e.id = b.email_id`

func scanBroadcast(sc interface{ Scan(...any) error }) (Broadcast, error) {
	var b Broadcast
	var audience, created, updated string
	var fromName, replyTo, scheduled, started, finished, snapDone sql.NullString
	var jobID sql.NullInt64
	err := sc.Scan(&b.ID, &b.Name, &b.Status, &audience, &fromName, &replyTo, &b.PausedReason,
		&scheduled, &started, &finished, &b.RecipientsTotal, &jobID, &snapDone,
		&b.rowVersion, &created, &updated,
		&b.Email.ID, &b.Email.Subject, &b.Email.Preheader, &b.Email.Format, &b.Email.Body, &b.emailVersion)
	if err != nil {
		return Broadcast{}, err
	}
	if err := db.UnmarshalJSON(audience, &b.Audience); err != nil {
		return Broadcast{}, fmt.Errorf("broadcast %d audience: %w", b.ID, err)
	}
	if b.Audience.Type == "" {
		b.Audience.Type = AudienceAll
	}
	if fromName.Valid {
		b.FromName = &fromName.String
	}
	if replyTo.Valid {
		b.ReplyTo = &replyTo.String
	}
	if jobID.Valid {
		b.SnapshotJobID = &jobID.Int64
	}
	if b.ScheduledAt, err = db.ParseNullTime(scheduled); err != nil {
		return Broadcast{}, err
	}
	if b.StartedAt, err = db.ParseNullTime(started); err != nil {
		return Broadcast{}, err
	}
	if b.FinishedAt, err = db.ParseNullTime(finished); err != nil {
		return Broadcast{}, err
	}
	if b.SnapshotDoneAt, err = db.ParseNullTime(snapDone); err != nil {
		return Broadcast{}, err
	}
	if b.CreatedAt, err = db.ParseTime(created); err != nil {
		return Broadcast{}, err
	}
	if b.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return Broadcast{}, err
	}
	b.Version = combinedVersion(b.rowVersion, b.emailVersion)
	return b, nil
}

// get reads a broadcast with its email body. A nil q reads from the read
// pool. Unknown IDs wrap core.ErrNotFound.
func (s *Service) get(ctx context.Context, q db.Querier, id int64) (Broadcast, error) {
	if q == nil {
		q = s.db.Read
	}
	b, err := scanBroadcast(q.QueryRowContext(ctx,
		`SELECT `+fmt.Sprintf(selectColumns, "e.body")+fromJoin+` WHERE b.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Broadcast{}, fmt.Errorf("broadcast %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return Broadcast{}, fmt.Errorf("broadcasts: get %d: %w", id, err)
	}
	return s.withArchiveURL(b), nil
}

// Get returns a broadcast with its email content.
func (s *Service) Get(ctx context.Context, id int64) (Broadcast, error) {
	return s.get(ctx, nil, id)
}

// ListFilter selects broadcasts for List.
type ListFilter struct {
	Status   string // "" = any
	BeforeID int64  // keyset cursor: only IDs lower than this (newest first)
	Limit    int    // maximum rows (callers pass page size + 1)
}

// List returns broadcasts newest first, without email bodies.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Broadcast, error) {
	if f.Status != "" && !validStatus(f.Status) {
		return nil, core.Invalid("status", "is not a broadcast status")
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q := `SELECT ` + fmt.Sprintf(selectColumns, "''") + fromJoin + ` WHERE 1 = 1`
	var args []any
	if f.Status != "" {
		q += ` AND b.status = ?`
		args = append(args, f.Status)
	}
	if f.BeforeID > 0 {
		q += ` AND b.id < ?`
		args = append(args, f.BeforeID)
	}
	q += ` ORDER BY b.id DESC LIMIT ?`
	rows, err := s.db.Read.QueryContext(ctx, q, append(args, f.Limit)...)
	if err != nil {
		return nil, fmt.Errorf("broadcasts: list: %w", err)
	}
	defer rows.Close()
	var out []Broadcast
	for rows.Next() {
		b, err := scanBroadcast(rows)
		if err != nil {
			return nil, fmt.Errorf("broadcasts: list: %w", err)
		}
		out = append(out, s.withArchiveURL(b))
	}
	return out, rows.Err()
}

func validStatus(s string) bool {
	switch s {
	case StatusDraft, StatusScheduled, StatusSending, StatusPaused, StatusSent, StatusCancelled:
		return true
	}
	return false
}
