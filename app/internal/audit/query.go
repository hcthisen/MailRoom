package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/db"
)

// Record is a stored audit entry.
type Record struct {
	ID         int64           `json:"id" example:"812"`
	Actor      Actor           `json:"actor"`
	Operation  string          `json:"operation" example:"settings.update"`
	TargetType string          `json:"target_type" example:"settings"`
	TargetID   string          `json:"target_id" example:"app"`
	Changes    json.RawMessage `json:"changes" doc:"The changed fields; updates use {\"field\": {\"from\": ..., \"to\": ...}}"`
	CreatedAt  time.Time       `json:"created_at"`
}

// Actor is who performed an audited operation.
type Actor struct {
	Type string `json:"type" enum:"user,token,system,public,cli" example:"token"`
	ID   string `json:"id" example:"3"`
	Name string `json:"name" doc:"User email or token name" example:"setup-agent"`
}

// Filter narrows List. Zero fields don't filter.
type Filter struct {
	TargetType string
	TargetID   string
	// Actor matches the actor string ("token:3", "user:1", "cli") or the
	// actor's name (a user email or token name).
	Actor     string
	Operation string
	Since     time.Time // created_at >= Since
	Until     time.Time // created_at < Until
	BeforeID  int64     // keyset pagination: id < BeforeID (newest first)
	Limit     int       // rows to return (callers ask for page size + 1)
}

// Service reads the audit log.
type Service struct {
	DB *db.DB
}

// New returns an audit service.
func New(d *db.DB) *Service { return &Service{DB: d} }

// List returns matching records, newest first.
func (s *Service) List(ctx context.Context, f Filter) ([]Record, error) {
	var where []string
	var args []any
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if f.TargetType != "" {
		add("target_type = ?", f.TargetType)
	}
	if f.TargetID != "" {
		add("target_id = ?", f.TargetID)
	}
	if f.Operation != "" {
		add("operation = ?", f.Operation)
	}
	if f.Actor != "" {
		typ, id, hasID := strings.Cut(f.Actor, ":")
		if hasID {
			add("((actor_type = ? AND actor_id = ?) OR actor_name = ?)", typ, id, f.Actor)
		} else {
			add("((actor_type = ? AND actor_id = '') OR actor_name = ?)", f.Actor, f.Actor)
		}
	}
	if !f.Since.IsZero() {
		add("created_at >= ?", db.FormatTime(f.Since))
	}
	if !f.Until.IsZero() {
		add("created_at < ?", db.FormatTime(f.Until))
	}
	if f.BeforeID > 0 {
		add("id < ?", f.BeforeID)
	}
	q := `SELECT id, actor_type, actor_id, actor_name, operation, target_type, target_id, changes, created_at FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	q += " ORDER BY id DESC LIMIT " + strconv.Itoa(limit)

	rows, err := s.DB.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit log: %w", err)
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		var changes, created string
		if err := rows.Scan(&r.ID, &r.Actor.Type, &r.Actor.ID, &r.Actor.Name, &r.Operation,
			&r.TargetType, &r.TargetID, &changes, &created); err != nil {
			return nil, fmt.Errorf("scan audit log: %w", err)
		}
		r.Changes = json.RawMessage(changes)
		if r.CreatedAt, err = db.ParseTime(created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
