package segments

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Preview limits.
const (
	DefaultSampleLimit = 10
	MaxSampleLimit     = 50
)

// SegmentCounts is the size of a segment right now.
type SegmentCounts struct {
	Total  int64 `json:"total" doc:"Contacts matching the filter, in any status" example:"1250"`
	Active int64 `json:"active" doc:"Matching contacts with status active: the recipients of a broadcast to this segment" example:"1180"`
	// MissingTags are tag names the filter refers to that no tag has: a
	// tag that was renamed, merged or deleted without the segment being
	// updated, or one not created yet.
	MissingTags []string `json:"missing_tags" doc:"Tag names the filter refers to that no tag has (renamed, merged, deleted or not created yet). A has/has_all/has_any rule on such a name matches nobody and a not_has/has_none rule matches everybody, so check the filter when this is not empty." example:"[]"`
}

// SegmentMember is a contact in a segment's member list or preview.
type SegmentMember struct {
	ID         int64          `json:"id" example:"42"`
	Email      string         `json:"email" example:"ann@example.com"`
	FirstName  string         `json:"first_name" example:"Ann"`
	LastName   string         `json:"last_name" example:"Berg"`
	Status     string         `json:"status" enum:"pending,active,unsubscribed,bounced,complained,suppressed_manual" example:"active"`
	Tags       []string       `json:"tags" example:"[\"customer\"]"`
	Attributes map[string]any `json:"attributes"`
	CreatedAt  time.Time      `json:"created_at"`
}

// SegmentPreview is the live result of a filter (T-3).
type SegmentPreview struct {
	SegmentCounts
	Sample []SegmentMember `json:"sample" doc:"The most recently created matching contacts"`
}

// MemberQuery pages through a segment's members.
type MemberQuery struct {
	AfterID int64              // members with a greater ID; 0 = from the start
	Limit   int                // maximum number of members
	Status  core.ContactStatus // "" = any status
}

// Compile compiles f with the service clock; see the package-level Compile.
func (s *Service) Compile(f SegmentFilter) (string, []any, error) {
	return Compile(f, s.clock.Now())
}

// WhereSQL implements core.SegmentResolver: it returns the SQL boolean
// expression (over alias "c") and arguments of the saved segment's filter,
// evaluated now. It never adds a status condition. Unknown IDs wrap
// core.ErrNotFound.
func (s *Service) WhereSQL(ctx context.Context, segmentID int64) (string, []any, error) {
	sg, err := s.Get(ctx, segmentID)
	if err != nil {
		return "", nil, err
	}
	expr, args, err := s.Compile(sg.Filter)
	if err != nil {
		return "", nil, fmt.Errorf("segment %d: %w", segmentID, prefixField(err, "filter"))
	}
	return expr, args, nil
}

// Count returns how many contacts match f in total and with status active,
// and which of its tag names don't exist. Validation errors carry
// "filter." field paths.
func (s *Service) Count(ctx context.Context, f SegmentFilter) (SegmentCounts, error) {
	expr, args, err := s.Compile(f)
	if err != nil {
		return SegmentCounts{}, prefixField(err, "filter")
	}
	return s.count(ctx, f, expr, args)
}

// CountSegment counts the saved segment id (see Count).
func (s *Service) CountSegment(ctx context.Context, id int64) (SegmentCounts, error) {
	sg, err := s.Get(ctx, id)
	if err != nil {
		return SegmentCounts{}, err
	}
	expr, args, err := s.Compile(sg.Filter)
	if err != nil {
		return SegmentCounts{}, fmt.Errorf("segment %d: %w", id, prefixField(err, "filter"))
	}
	return s.count(ctx, sg.Filter, expr, args)
}

// count evaluates the compiled filter f.
func (s *Service) count(ctx context.Context, f SegmentFilter, expr string, args []any) (SegmentCounts, error) {
	var c SegmentCounts
	err := s.db.Read.QueryRowContext(ctx,
		`SELECT count(*), IFNULL(sum(c.status = 'active'), 0) FROM contacts c WHERE `+expr, args...).
		Scan(&c.Total, &c.Active)
	if err != nil {
		return SegmentCounts{}, fmt.Errorf("count segment: %w", err)
	}
	if c.MissingTags, err = missingTags(ctx, s.db.Read, TagNames(f)); err != nil {
		return SegmentCounts{}, err
	}
	return c, nil
}

// Members returns a page of contacts matching f, ordered by ID.
func (s *Service) Members(ctx context.Context, f SegmentFilter, mq MemberQuery) ([]SegmentMember, error) {
	expr, args, err := s.Compile(f)
	if err != nil {
		return nil, prefixField(err, "filter")
	}
	return s.members(ctx, expr, args, mq)
}

// SegmentMembers returns a page of the saved segment's members (see Members).
func (s *Service) SegmentMembers(ctx context.Context, id int64, mq MemberQuery) ([]SegmentMember, error) {
	expr, args, err := s.WhereSQL(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.members(ctx, expr, args, mq)
}

func (s *Service) members(ctx context.Context, expr string, args []any, mq MemberQuery) ([]SegmentMember, error) {
	if mq.Status != "" && !mq.Status.Valid() {
		return nil, core.Invalid("status", "is not a contact status")
	}
	if mq.Limit <= 0 {
		return []SegmentMember{}, nil
	}
	q := `SELECT ` + memberColumns + ` FROM contacts c WHERE ` + expr + ` AND c.id > ?`
	args = append(append([]any{}, args...), mq.AfterID)
	if mq.Status != "" {
		q += ` AND c.status = ?`
		args = append(args, string(mq.Status))
	}
	q += ` ORDER BY c.id LIMIT ?`
	return s.queryMembers(ctx, q, append(args, mq.Limit))
}

// Preview returns the live counts of f and up to sampleLimit of the most
// recently created matching contacts (T-3). sampleLimit 0 means
// DefaultSampleLimit; it is at most MaxSampleLimit.
func (s *Service) Preview(ctx context.Context, f SegmentFilter, sampleLimit int) (SegmentPreview, error) {
	switch {
	case sampleLimit < 0 || sampleLimit > MaxSampleLimit:
		return SegmentPreview{}, core.Invalid("sample_limit", "must be between 0 and %d", MaxSampleLimit)
	case sampleLimit == 0:
		sampleLimit = DefaultSampleLimit
	}
	expr, args, err := s.Compile(f)
	if err != nil {
		return SegmentPreview{}, prefixField(err, "filter")
	}
	counts, err := s.count(ctx, f, expr, args)
	if err != nil {
		return SegmentPreview{}, err
	}
	sample, err := s.queryMembers(ctx,
		`SELECT `+memberColumns+` FROM contacts c WHERE `+expr+` ORDER BY c.id DESC LIMIT ?`,
		append(append([]any{}, args...), sampleLimit))
	if err != nil {
		return SegmentPreview{}, err
	}
	return SegmentPreview{SegmentCounts: counts, Sample: sample}, nil
}

const memberColumns = `c.id, c.email, c.first_name, c.last_name, c.attributes, c.status, c.created_at`

// queryMembers runs a member query and loads the members' tags.
func (s *Service) queryMembers(ctx context.Context, query string, args []any) ([]SegmentMember, error) {
	rows, err := s.db.Read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list segment members: %w", err)
	}
	defer rows.Close()
	out := []SegmentMember{}
	index := map[int64]int{}
	for rows.Next() {
		var (
			m              SegmentMember
			attrs, created string
		)
		if err := rows.Scan(&m.ID, &m.Email, &m.FirstName, &m.LastName, &attrs, &m.Status, &created); err != nil {
			return nil, fmt.Errorf("scan segment member: %w", err)
		}
		m.Attributes = map[string]any{}
		if err := db.UnmarshalJSON(attrs, &m.Attributes); err != nil {
			return nil, fmt.Errorf("contact %d attributes: %w", m.ID, err)
		}
		if m.CreatedAt, err = db.ParseTime(created); err != nil {
			return nil, err
		}
		m.Tags = []string{}
		index[m.ID] = len(out)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list segment members: %w", err)
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}
	return out, s.loadTags(ctx, out, index)
}

// loadTags fills the Tags of members with one query.
func (s *Service) loadTags(ctx context.Context, members []SegmentMember, index map[int64]int) error {
	ids := make([]int64, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encode member ids: %w", err)
	}
	rows, err := s.db.Read.QueryContext(ctx, `
		SELECT ct.contact_id, t.name FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id
		WHERE ct.contact_id IN (SELECT value FROM json_each(?))
		ORDER BY ct.contact_id, t.name COLLATE NOCASE`, string(list))
	if err != nil {
		return fmt.Errorf("load member tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return fmt.Errorf("scan member tag: %w", err)
		}
		if i, ok := index[id]; ok {
			members[i].Tags = append(members[i].Tags, name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load member tags: %w", err)
	}
	return nil
}
