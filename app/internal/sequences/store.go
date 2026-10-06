package sequences

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
)

// seqRow is a sequences row with its tag references as IDs (0 = none).
// Tag references are always read from the row at use time: tag merges
// repoint them.
type seqRow struct {
	ID           int64
	Name         string
	Status       string
	TriggerTagID int64
	ExitTagIDs   []int64
	ExitAddTagID int64
	Reentry      string
	Window       *settings.SendWindow
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const seqColumns = `id, name, status, trigger_tag_id, exit_tag_ids, exit_add_tag_id, reentry, send_window, version, created_at, updated_at`

func scanSeq(sc interface{ Scan(...any) error }) (seqRow, error) {
	var (
		r                seqRow
		trigger, exitAdd sql.NullInt64
		exitIDs          string
		window           sql.NullString
		created, updated string
	)
	if err := sc.Scan(&r.ID, &r.Name, &r.Status, &trigger, &exitIDs, &exitAdd, &r.Reentry, &window,
		&r.Version, &created, &updated); err != nil {
		return seqRow{}, err
	}
	r.TriggerTagID, r.ExitAddTagID = trigger.Int64, exitAdd.Int64
	r.ExitTagIDs = []int64{}
	if err := db.UnmarshalJSON(exitIDs, &r.ExitTagIDs); err != nil {
		return seqRow{}, err
	}
	if window.Valid && window.String != "null" {
		var w settings.SendWindow
		if err := db.UnmarshalJSON(window.String, &w); err != nil {
			return seqRow{}, err
		}
		r.Window = &w
	}
	var err error
	if r.CreatedAt, err = db.ParseTime(created); err != nil {
		return seqRow{}, err
	}
	if r.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return seqRow{}, err
	}
	return r, nil
}

// loadSeq reads one sequence; unknown IDs wrap core.ErrNotFound.
func loadSeq(ctx context.Context, q db.Querier, id int64) (seqRow, error) {
	r, err := scanSeq(q.QueryRowContext(ctx, `SELECT `+seqColumns+` FROM sequences WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return seqRow{}, fmt.Errorf("sequence %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return seqRow{}, fmt.Errorf("sequences: load %d: %w", id, err)
	}
	return r, nil
}

// stepRow is a sequence_steps row joined with its email's subject and
// format.
type stepRow struct {
	ID         int64
	SequenceID int64
	Position   int
	Delay      int64
	EmailID    int64
	Subject    string
	Format     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// loadSteps returns the steps of a sequence in position order.
func loadSteps(ctx context.Context, q db.Querier, seqID int64) ([]stepRow, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT st.id, st.sequence_id, st.position, st.delay_seconds, st.email_id,
		       COALESCE(e.subject, ''), COALESCE(e.format, 'markdown'), st.created_at, st.updated_at
		FROM sequence_steps st LEFT JOIN emails e ON e.id = st.email_id
		WHERE st.sequence_id = ? ORDER BY st.position`, seqID)
	if err != nil {
		return nil, fmt.Errorf("sequences: steps of %d: %w", seqID, err)
	}
	defer rows.Close()
	var out []stepRow
	for rows.Next() {
		var r stepRow
		var created, updated string
		if err := rows.Scan(&r.ID, &r.SequenceID, &r.Position, &r.Delay, &r.EmailID, &r.Subject, &r.Format,
			&created, &updated); err != nil {
			return nil, fmt.Errorf("sequences: steps of %d: %w", seqID, err)
		}
		if r.CreatedAt, err = db.ParseTime(created); err != nil {
			return nil, err
		}
		if r.UpdatedAt, err = db.ParseTime(updated); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadStep reads one step of a sequence; a step of another sequence or an
// unknown ID wraps core.ErrNotFound.
func loadStep(ctx context.Context, q db.Querier, seqID, stepID int64) (stepRow, error) {
	steps, err := loadSteps(ctx, q, seqID)
	if err != nil {
		return stepRow{}, err
	}
	for _, st := range steps {
		if st.ID == stepID {
			return st, nil
		}
	}
	return stepRow{}, fmt.Errorf("step %d of sequence %d: %w", stepID, seqID, core.ErrNotFound)
}

// tagNames returns the names of the given tag IDs; IDs of deleted tags
// are missing from the map.
func tagNames(ctx context.Context, q db.Querier, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	var want []any
	for _, id := range ids {
		if id != 0 {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT id, name FROM tags WHERE id IN (`+placeholders(len(want))+`)`, want...)
	if err != nil {
		return nil, fmt.Errorf("sequences: tag names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// view builds the API form of a sequence, resolving tag names and steps.
func view(ctx context.Context, q db.Querier, r seqRow) (Sequence, error) {
	names, err := tagNames(ctx, q, append([]int64{r.TriggerTagID, r.ExitAddTagID}, r.ExitTagIDs...))
	if err != nil {
		return Sequence{}, err
	}
	optName := func(id int64) *string {
		if n, ok := names[id]; ok {
			return &n
		}
		return nil
	}
	out := Sequence{
		ID: r.ID, Name: r.Name, Status: r.Status, Reentry: r.Reentry,
		TriggerTag: optName(r.TriggerTagID), ExitAddTag: optName(r.ExitAddTagID),
		ExitTags: []string{}, SendWindow: r.Window, Steps: []SequenceStep{},
		Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	for _, id := range r.ExitTagIDs {
		if n, ok := names[id]; ok {
			out.ExitTags = append(out.ExitTags, n)
		}
	}
	steps, err := loadSteps(ctx, q, r.ID)
	if err != nil {
		return Sequence{}, err
	}
	for _, st := range steps {
		out.Steps = append(out.Steps, stepView(st))
	}
	return out, nil
}

func stepView(st stepRow) SequenceStep {
	return SequenceStep{
		ID: st.ID, Position: st.Position, DelaySeconds: st.Delay, Delay: FormatDelay(st.Delay),
		EmailID: st.EmailID, Subject: st.Subject, Format: st.Format, CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt,
	}
}

// bumpVersion increases a sequence's version after a change to it or its
// steps.
func (s *Service) bumpVersion(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sequences SET version = version + 1, updated_at = ? WHERE id = ?`,
		db.FormatTime(s.now()), id); err != nil {
		return fmt.Errorf("sequences: bump version of %d: %w", id, err)
	}
	return nil
}

// placeholders returns "?, ?, ..." with n markers.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// int64sToAny converts IDs to query arguments.
func int64sToAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
