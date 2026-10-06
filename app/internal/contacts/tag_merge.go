package contacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mailroom/internal/core"
)

// MergeResult reports a tag merge.
type MergeResult struct {
	Tag                Tag `json:"tag" doc:"The tag the source was merged into"`
	Moved              int `json:"moved" doc:"Contacts that gained the target tag" example:"12"`
	ExitingEnrollments int `json:"exiting_enrollments" doc:"Active enrollments whose contact now has one of the sequence's exit tags (only with force); they leave the sequence before its next step" example:"0"`
}

// MergeTag moves every membership of tag id to intoID and deletes tag id.
// Contacts that already had both keep one membership. Sequences and forms
// that referenced the source tag (trigger, exit tags, exit tag to add,
// form tags) are repointed to the target, and saved segments that select
// on the source name are rewritten to the target name. Moving memberships
// does not fire
// TagAdded hooks: a merge is housekeeping, not new tagging.
//
// Because sequence behaviour depends on which tags a contact has, a merge
// that would change it is refused with ErrConflict naming the sequence:
//   - always, when a sequence's trigger tag would become one of its own
//     exit tags (every contact it enrols would exit before the first step);
//   - unless force is set, when contacts actively enrolled in a sequence
//     would gain one of its exit tags (they would leave it at their next
//     step). With force the merge goes ahead and the result counts them.
func (s *Service) MergeTag(ctx context.Context, id, intoID int64, force bool) (MergeResult, error) {
	if id == intoID {
		return MergeResult{}, core.Invalid("into_tag_id", "must be a different tag")
	}
	var res MergeResult
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		src, err := getTag(ctx, tx, id)
		if err != nil {
			return err
		}
		into, err := getTag(ctx, tx, intoID)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return core.Invalid("into_tag_id", "tag %d does not exist", intoID)
			}
			return err
		}
		if name, err := triggerExitClash(ctx, tx, id, intoID); err != nil {
			return err
		} else if name != "" {
			return fmt.Errorf("merging %q into %q would make the trigger tag of sequence %q one of its exit tags; change the sequence first: %w",
				src.Name, into.Name, name, core.ErrConflict)
		}
		name, exiting, err := exitingEnrollments(ctx, tx, id, intoID)
		if err != nil {
			return err
		}
		if exiting > 0 && !force {
			return fmt.Errorf("merging %q into %q would give %d actively enrolled contact(s) an exit tag of sequence %q (and possibly others), so they would leave it; change the exit tags first or merge with force: %w",
				src.Name, into.Name, exiting, name, core.ErrConflict)
		}
		res.ExitingEnrollments = exiting
		if err := s.touchTagged(ctx, tx, id); err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx, `
			INSERT INTO contact_tags (contact_id, tag_id, source, added_at)
			SELECT contact_id, ?, source, added_at FROM contact_tags WHERE tag_id = ?
			ON CONFLICT DO NOTHING`, intoID, id)
		if err != nil {
			return fmt.Errorf("merge tag %d: %w", id, err)
		}
		moved, _ := r.RowsAffected()
		res.Moved = int(moved)
		if err := s.repointTag(ctx, tx, id, intoID); err != nil {
			return err
		}
		segs, err := s.replaceTagRefs(ctx, tx, src.Name, into.Name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, id); err != nil {
			return fmt.Errorf("merge tag %d: delete: %w", id, err)
		}
		if res.Tag, err = getTag(ctx, tx, intoID); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "tag.merge", "tag", id, map[string]any{
			"name": src.Name, "into_tag_id": intoID, "into": res.Tag.Name, "moved": res.Moved,
			"force": force, "exiting_enrollments": res.ExitingEnrollments, "segments_updated": segs,
		})
	})
	return res, err
}

// mapped is the SQL value of tag ID expression x after merging ?1 into ?2.
func mapped(x string) string {
	return `(CASE WHEN ` + x + ` = ?1 THEN ?2 ELSE ` + x + ` END)`
}

// triggerExitClash names the first sequence referencing tag from whose
// trigger tag would be one of its exit tags after merging from into to,
// or "".
func triggerExitClash(ctx context.Context, tx *sql.Tx, from, to int64) (string, error) {
	var name string
	err := tx.QueryRowContext(ctx, `
		SELECT s.name FROM sequences s
		WHERE s.trigger_tag_id IS NOT NULL
			AND (s.trigger_tag_id = ?1 OR `+containsID("s.exit_tag_ids")+`)
			AND EXISTS (SELECT 1 FROM json_each(s.exit_tag_ids) je
				WHERE `+mapped("je.value")+` = `+mapped("s.trigger_tag_id")+`)
		ORDER BY s.name LIMIT 1`, from, to).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check sequences of tag %d: %w", from, err)
	}
	return name, nil
}

// exitingEnrollments counts the active enrollments whose contact has none
// of the sequence's exit tags now but would have one after merging from
// into to: the contact has from and the exits contain to, or the contact
// has to and the exits contain from. name is the first such sequence.
func exitingEnrollments(ctx context.Context, tx *sql.Tx, from, to int64) (name string, n int, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT s.name, count(*) FROM enrollments e JOIN sequences s ON s.id = e.sequence_id
		WHERE e.status = 'active'
			AND NOT EXISTS (SELECT 1 FROM contact_tags ct JOIN json_each(s.exit_tag_ids) je ON je.value = ct.tag_id
				WHERE ct.contact_id = e.contact_id)
			AND ((EXISTS (SELECT 1 FROM contact_tags ct WHERE ct.contact_id = e.contact_id AND ct.tag_id = ?1)
					AND EXISTS (SELECT 1 FROM json_each(s.exit_tag_ids) WHERE value = ?2))
				OR (EXISTS (SELECT 1 FROM contact_tags ct WHERE ct.contact_id = e.contact_id AND ct.tag_id = ?2)
					AND EXISTS (SELECT 1 FROM json_each(s.exit_tag_ids) WHERE value = ?1)))
		GROUP BY s.id ORDER BY s.name`, from, to)
	if err != nil {
		return "", 0, fmt.Errorf("check enrollments of tag %d: %w", from, err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq string
		var count int
		if err := rows.Scan(&seq, &count); err != nil {
			return "", 0, fmt.Errorf("check enrollments of tag %d: %w", from, err)
		}
		if name == "" {
			name = seq
		}
		n += count
	}
	return name, n, rows.Err()
}

// repointTag replaces tag from with tag to in sequences (trigger, exit tag
// to add, exit tags) and forms (tags), bumping each changed row's version
// once.
func (s *Service) repointTag(ctx context.Context, tx *sql.Tx, from, to int64) error {
	now := s.now()
	stmts := []string{
		`UPDATE sequences SET
			trigger_tag_id = ` + mapped("trigger_tag_id") + `,
			exit_add_tag_id = ` + mapped("exit_add_tag_id") + `,
			exit_tag_ids = ` + replacedIDs("sequences.exit_tag_ids") + `,
			version = version + 1, updated_at = ?3
		WHERE trigger_tag_id = ?1 OR exit_add_tag_id = ?1 OR ` + containsID("sequences.exit_tag_ids"),
		`UPDATE forms SET tag_ids = ` + replacedIDs("forms.tag_ids") + `, version = version + 1, updated_at = ?3
		WHERE ` + containsID("forms.tag_ids"),
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q, from, to, now); err != nil {
			return fmt.Errorf("repoint tag %d: %w", from, err)
		}
	}
	return nil
}

// replacedIDs is the SQL value of JSON array col with ID ?1 replaced by ?2
// (without duplicates, in the original order).
func replacedIDs(col string) string {
	return `(SELECT json_group_array(v) FROM (
			SELECT ` + mapped("je.value") + ` AS v, min(je.key) AS k
			FROM json_each(` + col + `) je GROUP BY v ORDER BY k))`
}

// containsID is the SQL condition "JSON array col contains ID ?1".
func containsID(col string) string {
	return `EXISTS (SELECT 1 FROM json_each(` + col + `) WHERE value = ?1)`
}
