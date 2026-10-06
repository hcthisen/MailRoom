package sequences

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Get returns a sequence with its steps.
func (s *Service) Get(ctx context.Context, id int64) (Sequence, error) {
	return s.get(ctx, s.db.Read, id)
}

func (s *Service) get(ctx context.Context, q db.Querier, id int64) (Sequence, error) {
	r, err := loadSeq(ctx, q, id)
	if err != nil {
		return Sequence{}, err
	}
	return view(ctx, q, r)
}

// List returns sequences with IDs greater than afterID in ID order, at
// most limit (callers ask one more to paginate).
func (s *Service) List(ctx context.Context, afterID int64, limit int) ([]Sequence, error) {
	if limit <= 0 {
		limit = api.DefaultLimit
	}
	rows, err := s.db.Read.QueryContext(ctx, `SELECT `+seqColumns+` FROM sequences WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("sequences: list: %w", err)
	}
	var list []seqRow
	for rows.Next() {
		r, err := scanSeq(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("sequences: list: %w", err)
		}
		list = append(list, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Sequence, 0, len(list))
	for _, r := range list {
		v, err := view(ctx, s.db.Read, r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Create validates in and stores a new sequence without steps. Tags are
// created as needed. A duplicate name is core.ErrConflict.
func (s *Service) Create(ctx context.Context, in SequenceInput) (Sequence, error) {
	var out Sequence
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.create(ctx, tx, in)
		return err
	})
	return out, err
}

// create stores a new sequence in tx with an audit record.
func (s *Service) create(ctx context.Context, tx *sql.Tx, in SequenceInput) (Sequence, error) {
	n, err := normalize(in)
	if err != nil {
		return Sequence{}, err
	}
	refs, err := s.ensureTags(ctx, tx, &n)
	if err != nil {
		return Sequence{}, err
	}
	exits, err := db.MarshalJSON(refs.Exits)
	if err != nil {
		return Sequence{}, err
	}
	window, err := windowJSON(n.Window)
	if err != nil {
		return Sequence{}, err
	}
	now := db.FormatTime(s.now())
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO sequences (name, status, trigger_tag_id, exit_tag_ids, exit_add_tag_id, reentry, send_window, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?) RETURNING id`,
		n.Name, n.Status, db.NullID(refs.Trigger), exits, db.NullID(refs.ExitAdd), n.Reentry, window, now, now).Scan(&id)
	if db.IsUniqueViolation(err) {
		return Sequence{}, fmt.Errorf("a sequence named %q already exists: %w", n.Name, core.ErrConflict)
	}
	if err != nil {
		return Sequence{}, fmt.Errorf("sequences: create: %w", err)
	}
	out, err := s.get(ctx, tx, id)
	if err != nil {
		return Sequence{}, err
	}
	return out, s.logAudit(ctx, tx, "sequence.create", "sequence", id, map[string]any{
		"name": out.Name, "status": out.Status, "trigger_tag": out.TriggerTag, "exit_tags": out.ExitTags,
		"exit_add_tag": out.ExitAddTag, "reentry": out.Reentry, "send_window": out.SendWindow,
	})
}

// Update applies p to sequence id. ifVersion (If-Match) must match unless
// it is 0. A patch that changes nothing returns the sequence unchanged.
func (s *Service) Update(ctx context.Context, id int64, p SequencePatch, ifVersion int64) (Sequence, error) {
	var out Sequence
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		n, err := normalize(p.apply(cur.input()))
		if err != nil {
			return err
		}
		refs, err := s.ensureTags(ctx, tx, &n)
		if err != nil {
			return err
		}
		exits, err := db.MarshalJSON(refs.Exits)
		if err != nil {
			return err
		}
		window, err := windowJSON(n.Window)
		if err != nil {
			return err
		}
		changes := s.configChanges(cur, n)
		if len(changes) == 0 {
			out = cur
			return nil
		}
		if n.Status == StatusActive && cur.Status != StatusActive {
			if err := s.requireFooter(ctx); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE sequences SET name = ?, status = ?, trigger_tag_id = ?, exit_tag_ids = ?, exit_add_tag_id = ?,
				reentry = ?, send_window = ?, version = version + 1, updated_at = ?
			WHERE id = ?`,
			n.Name, n.Status, db.NullID(refs.Trigger), exits, db.NullID(refs.ExitAdd), n.Reentry, window,
			db.FormatTime(s.now()), id)
		if db.IsUniqueViolation(err) {
			return fmt.Errorf("a sequence named %q already exists: %w", n.Name, core.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("sequences: update %d: %w", id, err)
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "sequence.update", "sequence", id, changes)
	})
	return out, err
}

// configChanges lists the fields that differ between cur and n.
func (s *Service) configChanges(cur Sequence, n normalized) map[string]any {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	changes := audit.Diff(nil, "name", cur.Name, n.Name)
	audit.Diff(changes, "status", cur.Status, n.Status)
	audit.Diff(changes, "reentry", cur.Reentry, n.Reentry)
	audit.Diff(changes, "trigger_tag", str(cur.TriggerTag), n.TriggerTag)
	audit.Diff(changes, "exit_add_tag", str(cur.ExitAddTag), n.ExitAddTag)
	if !sameTagList(cur.ExitTags, n.ExitTags) {
		changes["exit_tags"] = audit.Change{From: cur.ExitTags, To: n.ExitTags}
	}
	if !sameWindow(cur, n) {
		changes["send_window"] = audit.Change{From: cur.SendWindow, To: n.Window}
	}
	return changes
}

func sameTagList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameWindow(cur Sequence, n normalized) bool {
	a, b := cur.SendWindow, n.Window
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Start != b.Start || a.End != b.End || len(a.Days) != len(b.Days) {
		return false
	}
	for i := range a.Days {
		if a.Days[i] != b.Days[i] {
			return false
		}
	}
	return true
}

// SetStatus pauses (paused) or resumes (active) a sequence.
func (s *Service) SetStatus(ctx context.Context, id int64, status string, ifVersion int64) (Sequence, error) {
	return s.Update(ctx, id, SequencePatch{Status: &status}, ifVersion)
}

// DeleteResult reports a sequence deletion.
type DeleteResult struct {
	ExitedEnrollments int `json:"exited_enrollments" doc:"Active enrollments that were ended (reason sequence_deleted)" example:"0"`
}

// Delete deletes a sequence with its steps and enrollments. While
// enrollments are active it refuses with core.ErrConflict unless force is
// set; with force they exit (reason sequence_deleted) first. Their queued
// messages are skipped by the sender. The steps' emails are kept.
func (s *Service) Delete(ctx context.Context, id, ifVersion int64, force bool) (DeleteResult, error) {
	var res DeleteResult
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := loadSeq(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM enrollments WHERE sequence_id = ? AND status = 'active'`, id).Scan(&active); err != nil {
			return fmt.Errorf("sequences: count enrollments of %d: %w", id, err)
		}
		if active > 0 && !force {
			return fmt.Errorf("sequence %q has %d active enrollment(s); delete with force=true to end them: %w", cur.Name, active, core.ErrConflict)
		}
		n, err := s.exitWhere(ctx, tx, `sequence_id = ?`, []any{id}, ExitSequenceDeleted)
		if err != nil {
			return err
		}
		res.ExitedEnrollments = n
		if _, err := tx.ExecContext(ctx, `DELETE FROM sequences WHERE id = ?`, id); err != nil {
			return fmt.Errorf("sequences: delete %d: %w", id, err)
		}
		return s.logAudit(ctx, tx, "sequence.delete", "sequence", id, map[string]any{
			"name": cur.Name, "force": force, "exited_enrollments": n,
		})
	})
	return res, err
}

// requireFooter refuses to resume a sequence while settings company_footer
// is empty (E-1): its emails would only wait for one in the sender.
func (s *Service) requireFooter(ctx context.Context) error {
	if s.settings == nil {
		return nil
	}
	st, err := s.settings.Get(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(st.CompanyFooter) == "" {
		return core.Invalid("company_footer", "is not set in the settings: every marketing email must show the sender's name and postal address; set it before resuming a sequence")
	}
	return nil
}
