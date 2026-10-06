package contacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Tag is a tag with the number of contacts that have it.
type Tag struct {
	ID        int64     `json:"id" example:"3"`
	Name      string    `json:"name" example:"customer"`
	Contacts  int       `json:"contacts" doc:"Number of contacts with this tag" example:"120"`
	CreatedAt time.Time `json:"created_at"`
}

const tagQuery = `SELECT t.id, t.name, t.created_at,
		(SELECT count(*) FROM contact_tags ct WHERE ct.tag_id = t.id)
	FROM tags t`

func scanTag(r rowScanner) (Tag, error) {
	var t Tag
	var created string
	if err := r.Scan(&t.ID, &t.Name, &created, &t.Contacts); err != nil {
		return Tag{}, err
	}
	var err error
	t.CreatedAt, err = db.ParseTime(created)
	return t, err
}

// GetTag returns one tag with its contact count.
func (s *Service) GetTag(ctx context.Context, id int64) (Tag, error) {
	return getTag(ctx, s.db.Read, id)
}

func getTag(ctx context.Context, q db.Querier, id int64) (Tag, error) {
	t, err := scanTag(q.QueryRowContext(ctx, tagQuery+` WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Tag{}, fmt.Errorf("tag %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return Tag{}, fmt.Errorf("get tag %d: %w", id, err)
	}
	return t, nil
}

// ListTags returns tags in ID order after afterID, optionally only those
// whose name contains query. Callers paginate with limit+1.
func (s *Service) ListTags(ctx context.Context, query string, afterID int64, limit int) ([]Tag, error) {
	q := tagQuery + ` WHERE t.id > ?`
	args := []any{afterID}
	if query = strings.TrimSpace(query); query != "" {
		q += ` AND t.name LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(query)+"%")
	}
	if limit <= 0 {
		limit = 50
	}
	q += ` ORDER BY t.id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()
	out := []Tag{}
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTag creates a tag. An existing name (case-insensitive) is a
// conflict.
func (s *Service) CreateTag(ctx context.Context, name string) (Tag, error) {
	name, err := normalizeTagField(name, "name")
	if err != nil {
		return Tag{}, err
	}
	var out Tag
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO tags (name, created_at) VALUES (?, ?) RETURNING id`, name, s.now()).Scan(&id)
		if db.IsUniqueViolation(err) {
			return fmt.Errorf("a tag named %q already exists: %w", name, core.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("create tag: %w", err)
		}
		if out, err = getTag(ctx, tx, id); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "tag.create", "tag", id, map[string]any{"name": name})
	})
	return out, err
}

// RenameTag renames a tag; the new name must not belong to another tag.
// Memberships, sequence triggers and forms keep pointing at the tag, and
// saved segments that select on the old name are rewritten to the new one
// in the same transaction (Deps.TagRefs), so they keep selecting the same
// contacts.
func (s *Service) RenameTag(ctx context.Context, id int64, name string) (Tag, error) {
	name, err := normalizeTagField(name, "name")
	if err != nil {
		return Tag{}, err
	}
	var out Tag
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := getTag(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Name == name {
			out = cur
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE tags SET name = ? WHERE id = ?`, name, id)
		if db.IsUniqueViolation(err) {
			return fmt.Errorf("another tag is named %q: %w", name, core.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("rename tag %d: %w", id, err)
		}
		if err := s.touchTagged(ctx, tx, id); err != nil {
			return err
		}
		segs, err := s.replaceTagRefs(ctx, tx, cur.Name, name)
		if err != nil {
			return err
		}
		if out, err = getTag(ctx, tx, id); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "tag.rename", "tag", id, map[string]any{
			"name": map[string]any{"from": cur.Name, "to": name}, "segments_updated": segs,
		})
	})
	return out, err
}

// DeleteTag deletes a tag and removes it from every contact. A tag used by
// a sequence (trigger, exit tag, exit tag to add), a form or a saved
// segment can't be deleted (409): change those first, or merge the tag into
// another. (A segment rule such as "has none of" would otherwise silently
// start matching everyone.)
func (s *Service) DeleteTag(ctx context.Context, id int64) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		t, err := getTag(ctx, tx, id)
		if err != nil {
			return err
		}
		if used, err := tagUsage(ctx, tx, id); err != nil {
			return err
		} else if used != "" {
			return fmt.Errorf("tag %q is used by %s; change it there first or merge the tag: %w", t.Name, used, core.ErrConflict)
		}
		if s.tagRefs != nil {
			ids, err := s.tagRefs.SegmentsUsingTag(ctx, tx, t.Name)
			if err != nil {
				return err
			}
			if len(ids) > 0 {
				return fmt.Errorf("tag %q is used by segment %s; change the segment first or merge the tag: %w",
					t.Name, joinIDs(ids), core.ErrConflict)
			}
		}
		if err := s.touchTagged(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete tag %d: %w", id, err)
		}
		return s.logAudit(ctx, tx, "tag.delete", "tag", id, map[string]any{"name": t.Name, "contacts": t.Contacts})
	})
}

// tagUsage names the first sequence or form that references tag id, or "".
func tagUsage(ctx context.Context, q db.Querier, id int64) (string, error) {
	var name string
	err := q.QueryRowContext(ctx, `
		SELECT 'sequence "' || name || '"' FROM sequences
		WHERE trigger_tag_id = ?1 OR exit_add_tag_id = ?1
			OR EXISTS (SELECT 1 FROM json_each(sequences.exit_tag_ids) WHERE value = ?1)
		UNION ALL
		SELECT 'form "' || name || '"' FROM forms
		WHERE EXISTS (SELECT 1 FROM json_each(forms.tag_ids) WHERE value = ?1)
		LIMIT 1`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check tag %d usage: %w", id, err)
	}
	return name, nil
}

// replaceTagRefs rewrites saved references to tag name from into to
// (segments) inside tx and returns how many records changed.
func (s *Service) replaceTagRefs(ctx context.Context, tx *sql.Tx, from, to string) (int, error) {
	if s.tagRefs == nil {
		return 0, nil
	}
	n, err := s.tagRefs.ReplaceTagName(ctx, tx, from, to)
	if err != nil {
		return 0, fmt.Errorf("update segments of tag %q: %w", from, err)
	}
	return n, nil
}

// joinIDs formats IDs as "1, 2 and 3".
func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// touchTagged bumps the version of every contact with tag id, because its
// tag list (part of the contact's ETag) changes.
func (s *Service) touchTagged(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE contacts SET version = version + 1, updated_at = ?
		WHERE id IN (SELECT contact_id FROM contact_tags WHERE tag_id = ?)`, s.now(), id)
	if err != nil {
		return fmt.Errorf("update contacts of tag %d: %w", id, err)
	}
	return nil
}

// normalizeTagField normalises a tag name, reporting errors for field.
func normalizeTagField(name, field string) (string, error) {
	n, err := contactstore.NormalizeTag(name)
	if err != nil {
		return "", withField(err, field)
	}
	return n, nil
}
