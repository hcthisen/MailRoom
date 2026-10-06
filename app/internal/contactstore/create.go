package contactstore

import (
	"context"
	"database/sql"
	"fmt"
	"maps"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/hooks"
)

// CreateInput is a new contact.
type CreateInput struct {
	Email      string
	FirstName  string
	LastName   string
	Attributes map[string]any
	Tags       []string
	Status     core.ContactStatus // "" = active
	Source     string             // hook/tag source, e.g. "api", "form:ab12", "import"
}

// create implements Create.
func (s *Store) create(ctx context.Context, tx *sql.Tx, in CreateInput) (core.Contact, error) {
	email, err := core.NormalizeEmail(in.Email)
	if err != nil {
		return core.Contact{}, err
	}
	first, err := ValidateName("first_name", in.FirstName)
	if err != nil {
		return core.Contact{}, err
	}
	last, err := ValidateName("last_name", in.LastName)
	if err != nil {
		return core.Contact{}, err
	}
	if err := ValidateAttributes(in.Attributes); err != nil {
		return core.Contact{}, err
	}
	tags, err := NormalizeTags(in.Tags)
	if err != nil {
		return core.Contact{}, err
	}
	status := in.Status
	if status == "" {
		status = core.StatusActive
	}
	if !status.Valid() {
		return core.Contact{}, core.Invalid("status", "unknown status %q", status)
	}

	suppressed, err := s.IsSuppressed(ctx, tx, email)
	if err != nil {
		return core.Contact{}, err
	}
	if suppressed {
		return core.Contact{}, fmt.Errorf("create contact: %w", core.ErrSuppressed)
	}

	attrs, err := db.MarshalJSON(maps.Clone(in.Attributes))
	if err != nil {
		return core.Contact{}, core.Invalid("attributes", "must be JSON-serialisable")
	}
	now := db.FormatTime(s.Clock.Now())
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO contacts (email, first_name, last_name, attributes, status, status_changed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		email, first, last, attrs, string(status), now, now, now).Scan(&id)
	if db.IsUniqueViolation(err) {
		return core.Contact{}, fmt.Errorf("contact with that email already exists: %w", core.ErrConflict)
	}
	if err != nil {
		return core.Contact{}, fmt.Errorf("insert contact: %w", err)
	}
	if status == core.StatusSuppressedManual {
		// Keep the suppressions table in step with the status (see
		// syncManualSuppression).
		if err := syncSuppressionRow(ctx, tx, core.EmailHash(email), true, "", now); err != nil {
			return core.Contact{}, err
		}
	}

	if err := s.Bus.PublishContactCreated(ctx, tx, hooks.ContactCreated{ContactID: id, Source: in.Source}); err != nil {
		return core.Contact{}, err
	}
	if len(tags) > 0 {
		// Creating a contact with tags is one change: keep version 1.
		if _, err := s.addTags(ctx, tx, id, tags, in.Source); err != nil {
			return core.Contact{}, err
		}
	}
	return s.Get(ctx, tx, id)
}
