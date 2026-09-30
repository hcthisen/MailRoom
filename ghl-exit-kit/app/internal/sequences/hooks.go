package sequences

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"mailroom/internal/core"
	"mailroom/internal/hooks"
)

// RegisterHooks subscribes the service to the bus (Deps.Bus when bus is
// nil):
//
//   - TagAdded: the contact is enrolled in every active sequence whose
//     trigger tag it is (source "tag:<name>"), except when the tag comes
//     from an import (source "import..."): imports never mass-enrol, and
//     migrated contacts are enrolled explicitly through the enrollments
//     API. Active enrollments of sequences that list the tag as an exit tag
//     end (reason exit_tag), and the sequence's exit_add_tag is added.
//   - StatusChanged away from active: all the contact's active enrollments
//     end (reason not_active), and exit_add_tags are added.
//
// Handlers run inside the publisher's transaction and only use it.
func (s *Service) RegisterHooks(bus *hooks.Bus) {
	if bus == nil {
		bus = s.bus
	}
	if bus == nil {
		panic("sequences: RegisterHooks needs a bus")
	}
	bus.OnTagAdded(s.onTagAdded)
	bus.OnStatusChanged(s.onStatusChanged)
}

// onTagAdded handles TagAdded.
func (s *Service) onTagAdded(ctx context.Context, tx *sql.Tx, e hooks.TagAdded) error {
	exits, err := activeEnrollments(ctx, tx, e.ContactID,
		`EXISTS (SELECT 1 FROM json_each(s.exit_tag_ids) WHERE value = ?)`, e.TagID)
	if err != nil {
		return err
	}
	if err := s.exitEnrollments(ctx, tx, exits, ExitTag); err != nil {
		return err
	}
	if strings.HasPrefix(e.Source, "import") {
		return nil
	}
	ids, err := triggeredBy(ctx, tx, e.TagID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.Enroll(ctx, tx, id, e.ContactID, core.EnrollOptions{Source: "tag:" + e.TagName}); err != nil {
			return fmt.Errorf("sequences: tag trigger %q, sequence %d: %w", e.TagName, id, err)
		}
	}
	return nil
}

// triggeredBy returns the active sequences whose trigger tag is tagID.
func triggeredBy(ctx context.Context, tx *sql.Tx, tagID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sequences WHERE status = 'active' AND trigger_tag_id = ? ORDER BY id`, tagID)
	if err != nil {
		return nil, fmt.Errorf("sequences: triggered by tag %d: %w", tagID, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// onStatusChanged handles StatusChanged.
func (s *Service) onStatusChanged(ctx context.Context, tx *sql.Tx, e hooks.StatusChanged) error {
	if e.To == core.StatusActive {
		return nil
	}
	list, err := activeEnrollments(ctx, tx, e.ContactID, "")
	if err != nil {
		return err
	}
	return s.exitEnrollments(ctx, tx, list, ExitNotActive)
}
