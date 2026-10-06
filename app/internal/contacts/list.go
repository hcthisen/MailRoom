package contacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// ListFilter narrows List. Zero fields don't filter; all filters combine
// with AND.
type ListFilter struct {
	Query         string             // email, first, last or full name contains (case-insensitive)
	Email         string             // exactly this address (normalised)
	Status        core.ContactStatus // exact status
	Tag           string             // has this tag (case-insensitive)
	CreatedAfter  *time.Time         // created at or after
	CreatedBefore *time.Time         // created before
	Attributes    []string           // "key:value": attribute key equals value exactly
	SegmentID     int64              // member of this segment (evaluated now)
	BeforeID      int64              // keyset pagination: only IDs below this (newest first)
	Limit         int                // default 50
}

// maxAttributeFilters bounds the attr filters of one request.
const maxAttributeFilters = 10

// List returns contacts matching f, newest first (by ID), with their tags.
// Callers paginate by asking for Limit+1 and passing the last ID as
// BeforeID.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Contact, error) {
	where, args, err := s.listWhere(ctx, f)
	if err != nil {
		return nil, err
	}
	if f.BeforeID > 0 {
		where = append(where, "c.id < ?")
		args = append(args, f.BeforeID)
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q := `SELECT c.id, c.email, c.first_name, c.last_name, c.attributes, c.status, c.status_changed_at,
			c.created_at, c.updated_at, c.version, c.unsubscribe_requested_at
		FROM contacts c`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY c.id DESC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := s.db.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	defer rows.Close()
	out := []Contact{}
	for rows.Next() {
		c, err := scanContactRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	if err := attachTags(ctx, s.db.Read, out); err != nil {
		return nil, err
	}
	return out, nil
}

// listWhere compiles the filters of f (except pagination) to SQL
// conditions over alias c.
func (s *Service) listWhere(ctx context.Context, f ListFilter) ([]string, []any, error) {
	var where []string
	var args []any
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + escapeLike(q) + "%"
		where = append(where, `(c.email LIKE ? ESCAPE '\' OR c.first_name LIKE ? ESCAPE '\' OR c.last_name LIKE ? ESCAPE '\'
			OR (c.first_name || ' ' || c.last_name) LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	if f.Email != "" {
		norm, err := core.NormalizeEmail(f.Email)
		if err != nil {
			return nil, nil, core.Invalid("query.email", "is not a valid email address")
		}
		where = append(where, "c.email = ?")
		args = append(args, norm)
	}
	if f.Status != "" {
		if !f.Status.Valid() {
			return nil, nil, core.Invalid("query.status", "unknown status %q", f.Status)
		}
		where = append(where, "c.status = ?")
		args = append(args, string(f.Status))
	}
	if f.Tag != "" {
		tag, err := contactstore.NormalizeTag(f.Tag)
		if err != nil {
			return nil, nil, withField(err, "query.tag")
		}
		where = append(where, `EXISTS (SELECT 1 FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id
			WHERE ct.contact_id = c.id AND t.name = ?)`)
		args = append(args, tag)
	}
	if f.CreatedAfter != nil {
		where = append(where, "c.created_at >= ?")
		args = append(args, db.FormatTime(*f.CreatedAfter))
	}
	if f.CreatedBefore != nil {
		where = append(where, "c.created_at < ?")
		args = append(args, db.FormatTime(*f.CreatedBefore))
	}
	if len(f.Attributes) > maxAttributeFilters {
		return nil, nil, core.Invalid("query.attr", "at most %d attribute filters are allowed", maxAttributeFilters)
	}
	for _, a := range f.Attributes {
		key, value, ok := strings.Cut(a, ":")
		if !ok || contactstore.ValidateAttributes(map[string]any{key: nil}) != nil {
			return nil, nil, core.Invalid("query.attr", `must be "key:value" with a key of letters, digits or underscores`)
		}
		where = append(where, attrEquals)
		path := "$." + key
		args = append(args, path, path, value)
	}
	if f.SegmentID != 0 {
		expr, sargs, err := s.segmentWhere(ctx, f.SegmentID)
		if err != nil {
			return nil, nil, err
		}
		where = append(where, "("+expr+")")
		args = append(args, sargs...)
	}
	return where, args, nil
}

// attrEquals matches an attribute's value, as text, exactly: strings by
// their value, numbers by their JSON form, booleans as true/false. Its
// args are the JSON path twice and the value.
const attrEquals = `(CASE json_type(c.attributes, ?)
		WHEN 'true' THEN 'true' WHEN 'false' THEN 'false'
		ELSE CAST(json_extract(c.attributes, ?) AS TEXT) END) = ?`

// segmentWhere resolves a segment to its SQL condition over alias c.
func (s *Service) segmentWhere(ctx context.Context, segmentID int64) (string, []any, error) {
	if s.segments == nil {
		return "", nil, core.Invalid("segment_id", "segments are not available")
	}
	expr, args, err := s.segments.WhereSQL(ctx, segmentID)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(expr) == "" {
		expr = "1=1"
	}
	return expr, args, nil
}

// escapeLike escapes LIKE wildcards with backslash.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanContactRow(r rowScanner) (Contact, error) {
	var (
		c                          Contact
		attrs, status              string
		statusChanged, created, up string
		optOut                     sql.NullString
	)
	if err := r.Scan(&c.ID, &c.Email, &c.FirstName, &c.LastName, &attrs, &status,
		&statusChanged, &created, &up, &c.Version, &optOut); err != nil {
		return Contact{}, fmt.Errorf("scan contact: %w", err)
	}
	if optOut.Valid {
		t, err := db.ParseTime(optOut.String)
		if err != nil {
			return Contact{}, err
		}
		c.UnsubscribeRequestedAt = &t
	}
	c.Status = core.ContactStatus(status)
	c.Attributes = map[string]any{}
	if err := db.UnmarshalJSON(attrs, &c.Attributes); err != nil {
		return Contact{}, err
	}
	var err error
	if c.StatusChangedAt, err = db.ParseTime(statusChanged); err != nil {
		return Contact{}, err
	}
	if c.CreatedAt, err = db.ParseTime(created); err != nil {
		return Contact{}, err
	}
	if c.UpdatedAt, err = db.ParseTime(up); err != nil {
		return Contact{}, err
	}
	c.Tags = []string{}
	return c, nil
}

// attachTags loads the tags of every contact in cs with one query.
func attachTags(ctx context.Context, q db.Querier, cs []Contact) error {
	if len(cs) == 0 {
		return nil
	}
	idx := make(map[int64]int, len(cs))
	args := make([]any, len(cs))
	for i, c := range cs {
		idx[c.ID] = i
		args[i] = c.ID
	}
	rows, err := q.QueryContext(ctx, `
		SELECT ct.contact_id, t.name FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id
		WHERE ct.contact_id IN (`+placeholders(len(cs))+`)
		ORDER BY t.name COLLATE NOCASE`, args...)
	if err != nil {
		return fmt.Errorf("list contact tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return fmt.Errorf("scan contact tag: %w", err)
		}
		if i, ok := idx[id]; ok {
			cs[i].Tags = append(cs[i].Tags, name)
		}
	}
	return rows.Err()
}

// placeholders returns "?, ?, ..." with n placeholders.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// ensureContact returns ErrNotFound when contact id doesn't exist.
func ensureContact(ctx context.Context, q db.Querier, id int64) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM contacts WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("contact %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("read contact %d: %w", id, err)
	}
	return nil
}

// withField re-targets a validation error at another field (e.g. a query
// parameter); other errors are returned unchanged.
func withField(err error, field string) error {
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		return core.Invalid(field, "%s", ve.Message)
	}
	return err
}
