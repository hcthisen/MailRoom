package contactstore

import (
	"context"
	"database/sql"
	"fmt"

	"mailroom/internal/db"
	"mailroom/internal/hooks"
)

// addTagsToContact implements AddTags.
func (s *Store) addTagsToContact(ctx context.Context, tx *sql.Tx, contactID int64, names []string, source string) error {
	tags, err := NormalizeTags(names)
	if err != nil {
		return err
	}
	if _, _, err := currentState(ctx, tx, contactID); err != nil {
		return err
	}
	added, err := s.addTags(ctx, tx, contactID, tags, source)
	if err != nil {
		return err
	}
	if added > 0 {
		return s.touch(ctx, tx, contactID)
	}
	return nil
}

// addTags links already-normalised tags and returns how many were new.
func (s *Store) addTags(ctx context.Context, tx *sql.Tx, contactID int64, tags []string, source string) (int, error) {
	now := db.FormatTime(s.Clock.Now())
	added := 0
	for _, name := range tags {
		tagID, stored, err := EnsureTag(ctx, tx, name, now)
		if err != nil {
			return added, err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO contact_tags (contact_id, tag_id, source, added_at) VALUES (?, ?, ?, ?)
			ON CONFLICT DO NOTHING`, contactID, tagID, source, now)
		if err != nil {
			return added, fmt.Errorf("tag contact %d: %w", contactID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		added++
		if err := s.Bus.PublishTagAdded(ctx, tx, hooks.TagAdded{
			ContactID: contactID, TagID: tagID, TagName: stored, Source: source,
		}); err != nil {
			return added, err
		}
	}
	return added, nil
}

// EnsureTag returns the id and stored spelling of the tag named name
// (case-insensitive), creating it with createdAt when missing. name must
// already be normalised (NormalizeTag).
func EnsureTag(ctx context.Context, q db.Querier, name, createdAt string) (id int64, stored string, err error) {
	if _, err := q.ExecContext(ctx,
		`INSERT INTO tags (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, name, createdAt); err != nil {
		return 0, "", fmt.Errorf("create tag %q: %w", name, err)
	}
	if err := q.QueryRowContext(ctx, `SELECT id, name FROM tags WHERE name = ?`, name).Scan(&id, &stored); err != nil {
		return 0, "", fmt.Errorf("find tag %q: %w", name, err)
	}
	return id, stored, nil
}

// removeTags implements RemoveTags.
func (s *Store) removeTags(ctx context.Context, tx *sql.Tx, contactID int64, names []string) error {
	tags, err := NormalizeTags(names)
	if err != nil {
		return err
	}
	if _, _, err := currentState(ctx, tx, contactID); err != nil {
		return err
	}
	removed := int64(0)
	for _, name := range tags {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM contact_tags
			WHERE contact_id = ? AND tag_id = (SELECT id FROM tags WHERE name = ?)`, contactID, name)
		if err != nil {
			return fmt.Errorf("untag contact %d: %w", contactID, err)
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	if removed > 0 {
		return s.touch(ctx, tx, contactID)
	}
	return nil
}
