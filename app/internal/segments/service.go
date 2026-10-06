package segments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// MaxNameLength is the longest segment name, in characters.
const MaxNameLength = 100

// Deps are the Service's dependencies.
type Deps struct {
	DB    *db.DB
	Clock core.Clock // nil = core.SystemClock
}

// Service manages segments and evaluates their filters.
type Service struct {
	db    *db.DB
	clock core.Clock
}

var (
	_ core.SegmentResolver = (*Service)(nil)
	_ core.TagReferrer     = (*Service)(nil)
)

// New returns a segments service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	return &Service{db: d.DB, clock: d.Clock}
}

// Segment is a saved filter.
type Segment struct {
	ID        int64         `json:"id" example:"3"`
	Name      string        `json:"name" example:"Customers without VIP"`
	Filter    SegmentFilter `json:"filter"`
	Version   int64         `json:"version" doc:"Incremented by every change; the ETag is \"v<version>\"" example:"1"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// CreateInput creates a segment.
type CreateInput struct {
	Name   string
	Filter SegmentFilter
}

// UpdateInput changes a segment; nil members stay unchanged.
type UpdateInput struct {
	Name   *string
	Filter *SegmentFilter
}

const segmentColumns = `id, name, filter, version, created_at, updated_at`

// Create validates and stores a new segment. With dryRun nothing is stored
// and the segment that would be created is returned. A name already used
// (ignoring ASCII case) is a conflict.
func (s *Service) Create(ctx context.Context, in CreateInput, dryRun bool) (Segment, error) {
	name, err := validName(in.Name)
	if err != nil {
		return Segment{}, err
	}
	f, raw, err := prepareFilter(in.Filter)
	if err != nil {
		return Segment{}, err
	}
	var out Segment
	err = api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		if err := nameFree(ctx, tx, name, 0); err != nil {
			return err
		}
		now := s.clock.Now()
		ts := db.FormatTime(now)
		var id int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO segments (name, filter, created_at, updated_at) VALUES (?, ?, ?, ?) RETURNING id`,
			name, raw, ts, ts).Scan(&id)
		if db.IsUniqueViolation(err) {
			return nameTaken(name)
		}
		if err != nil {
			return fmt.Errorf("insert segment: %w", err)
		}
		out = Segment{ID: id, Name: name, Filter: f, Version: 1, CreatedAt: now, UpdatedAt: now}
		return s.audit(ctx, tx, "segment.create", id, map[string]any{"name": name, "filter": f})
	})
	if err != nil {
		return Segment{}, err
	}
	return out, nil
}

// Get returns a segment; unknown IDs wrap core.ErrNotFound.
func (s *Service) Get(ctx context.Context, id int64) (Segment, error) {
	return getSegment(ctx, s.db.Read, id)
}

// List returns up to limit segments with ID greater than afterID, by ID.
func (s *Service) List(ctx context.Context, afterID int64, limit int) ([]Segment, error) {
	rows, err := s.db.Read.QueryContext(ctx,
		`SELECT `+segmentColumns+` FROM segments WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}
	defer rows.Close()
	out := []Segment{}
	for rows.Next() {
		sg, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}
	return out, nil
}

// Update changes a segment's name and/or filter. ifVersion (from If-Match)
// must match unless it is 0. An update that changes nothing returns the
// segment without a new version. With dryRun nothing is stored.
func (s *Service) Update(ctx context.Context, id int64, in UpdateInput, ifVersion int64, dryRun bool) (Segment, error) {
	var (
		name   string
		f      SegmentFilter
		raw    string
		err    error
		hasNew = in.Filter != nil
	)
	if in.Name != nil {
		if name, err = validName(*in.Name); err != nil {
			return Segment{}, err
		}
	}
	if hasNew {
		if f, raw, err = prepareFilter(*in.Filter); err != nil {
			return Segment{}, err
		}
	}
	var out Segment
	err = api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		cur, err := getSegment(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		next := cur
		changes := map[string]any{}
		if in.Name != nil && name != cur.Name {
			if err := nameFree(ctx, tx, name, id); err != nil {
				return err
			}
			changes["name"] = audit.Change{From: cur.Name, To: name}
			next.Name = name
		}
		if hasNew {
			curRaw, err := MarshalFilter(cur.Filter)
			if err != nil {
				return err
			}
			if curRaw != raw {
				changes["filter"] = audit.Change{From: cur.Filter, To: f}
				next.Filter = f
			}
		}
		if len(changes) == 0 {
			out = cur
			return nil
		}
		nextRaw, err := MarshalFilter(next.Filter)
		if err != nil {
			return err
		}
		next.Version, next.UpdatedAt = cur.Version+1, s.clock.Now()
		_, err = tx.ExecContext(ctx,
			`UPDATE segments SET name = ?, filter = ?, version = ?, updated_at = ? WHERE id = ?`,
			next.Name, nextRaw, next.Version, db.FormatTime(next.UpdatedAt), id)
		if db.IsUniqueViolation(err) {
			return nameTaken(next.Name)
		}
		if err != nil {
			return fmt.Errorf("update segment %d: %w", id, err)
		}
		out = next
		return s.audit(ctx, tx, "segment.update", id, changes)
	})
	if err != nil {
		return Segment{}, err
	}
	return out, nil
}

// Delete removes a segment. It is refused with core.ErrConflict while a
// broadcast that is not finished (draft, scheduled, sending or paused)
// targets it. With dryRun nothing is deleted.
func (s *Service) Delete(ctx context.Context, id int64, ifVersion int64, dryRun bool) error {
	return api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		cur, err := getSegment(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		used, err := activeBroadcasts(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(used) > 0 {
			return fmt.Errorf("segment %d is the audience of unfinished broadcast(s) %s; change their audience or cancel them first: %w",
				id, strings.Join(used, ", "), core.ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM segments WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete segment %d: %w", id, err)
		}
		return s.audit(ctx, tx, "segment.delete", id, map[string]any{"name": cur.Name})
	})
}

// activeBroadcasts returns the IDs of broadcasts in draft, scheduled,
// sending or paused whose audience is segment id.
func activeBroadcasts(ctx context.Context, q db.Querier, id int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM broadcasts
		WHERE status IN ('draft', 'scheduled', 'sending', 'paused')
		  AND json_extract(audience, '$.segment_id') = ?
		ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("find broadcasts of segment %d: %w", id, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		ids = append(ids, strconv.FormatInt(b, 10))
	}
	return ids, rows.Err()
}

func (s *Service) audit(ctx context.Context, tx *sql.Tx, op string, id int64, changes any) error {
	return audit.Write(ctx, tx, audit.Entry{
		Operation: op, TargetType: "segment", TargetID: strconv.FormatInt(id, 10), Changes: changes, At: s.clock.Now(),
	})
}

// prepareFilter validates f for storage and returns it with its canonical
// JSON. Validation fields are prefixed with "filter.".
func prepareFilter(f SegmentFilter) (SegmentFilter, string, error) {
	vf, err := Validate(f)
	if err != nil {
		return SegmentFilter{}, "", prefixField(err, "filter")
	}
	raw, err := MarshalFilter(vf)
	if err != nil {
		return SegmentFilter{}, "", err
	}
	return vf, raw, nil
}

// prefixField prefixes the field of a validation error ("rules[0].op"
// becomes "filter.rules[0].op"; "" becomes "filter").
func prefixField(err error, prefix string) error {
	var ve *core.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	field := prefix
	if ve.Field != "" {
		field += "." + ve.Field
	}
	return &core.ValidationError{Field: field, Message: ve.Message}
}

func validName(n string) (string, error) {
	n = strings.TrimSpace(n)
	switch {
	case n == "":
		return "", core.Invalid("name", "is required")
	case utf8.RuneCountInString(n) > MaxNameLength:
		return "", core.Invalid("name", "is longer than %d characters", MaxNameLength)
	case !utf8.ValidString(n) || strings.IndexFunc(n, unicode.IsControl) >= 0:
		return "", core.Invalid("name", "contains invalid characters")
	}
	return n, nil
}

// nameFree fails with a conflict when another segment has name (ignoring
// ASCII case).
func nameFree(ctx context.Context, q db.Querier, name string, exceptID int64) error {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT id FROM segments WHERE name = ? COLLATE NOCASE AND id <> ? LIMIT 1`, name, exceptID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check segment name: %w", err)
	}
	return nameTaken(name)
}

func nameTaken(name string) error {
	return fmt.Errorf("a segment named %q already exists: %w", name, core.ErrConflict)
}

func getSegment(ctx context.Context, q db.Querier, id int64) (Segment, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+segmentColumns+` FROM segments WHERE id = ?`, id)
	if err != nil {
		return Segment{}, fmt.Errorf("get segment %d: %w", id, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Segment{}, fmt.Errorf("get segment %d: %w", id, err)
		}
		return Segment{}, fmt.Errorf("segment %d: %w", id, core.ErrNotFound)
	}
	return scanSegment(rows)
}

func scanSegment(rows *sql.Rows) (Segment, error) {
	var (
		sg               Segment
		raw, created, up string
	)
	if err := rows.Scan(&sg.ID, &sg.Name, &raw, &sg.Version, &created, &up); err != nil {
		return Segment{}, fmt.Errorf("scan segment: %w", err)
	}
	// Stored filters are read without validation, so a segment whose filter
	// no longer validates can still be shown and fixed; evaluating it
	// (Compile, Count, WhereSQL) reports the validation error.
	if err := json.Unmarshal([]byte(raw), &sg.Filter); err != nil {
		return Segment{}, fmt.Errorf("segment %d: decode stored filter: %w", sg.ID, err)
	}
	var err error
	if sg.CreatedAt, err = db.ParseTime(created); err != nil {
		return Segment{}, err
	}
	if sg.UpdatedAt, err = db.ParseTime(up); err != nil {
		return Segment{}, err
	}
	return sg, nil
}
