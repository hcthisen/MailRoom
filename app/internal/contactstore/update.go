package contactstore

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"reflect"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// UpdateInput is a partial update of a contact's names and attributes.
// Nil fields are left unchanged.
type UpdateInput struct {
	FirstName *string
	LastName  *string
	// Attributes are merged into the existing ones; a key with a nil value
	// removes that attribute. With ReplaceAttributes, the map replaces all
	// attributes instead.
	Attributes        map[string]any
	ReplaceAttributes bool
}

// update implements Update.
func (s *Store) update(ctx context.Context, tx *sql.Tx, id int64, in UpdateInput, ifVersion int64) (core.Contact, error) {
	cur, err := s.Get(ctx, tx, id)
	if err != nil {
		return core.Contact{}, err
	}
	if ifVersion != 0 && cur.Version != ifVersion {
		return core.Contact{}, fmt.Errorf("contact %d is at version %d, not %d: %w", id, cur.Version, ifVersion, core.ErrPrecondition)
	}

	first, last := cur.FirstName, cur.LastName
	if in.FirstName != nil {
		if first, err = ValidateName("first_name", *in.FirstName); err != nil {
			return core.Contact{}, err
		}
	}
	if in.LastName != nil {
		if last, err = ValidateName("last_name", *in.LastName); err != nil {
			return core.Contact{}, err
		}
	}
	attrs := maps.Clone(cur.Attributes)
	if in.ReplaceAttributes {
		attrs = map[string]any{}
	}
	for k, v := range in.Attributes {
		if v == nil {
			delete(attrs, k)
		} else {
			attrs[k] = v
		}
	}
	if err := ValidateAttributes(attrs); err != nil {
		return core.Contact{}, err
	}
	attrsJSON, err := db.MarshalJSON(attrs)
	if err != nil {
		return core.Contact{}, core.Invalid("attributes", "must be JSON-serialisable")
	}
	// Compare through JSON so 1 and 1.0 (decoded numbers) are equal.
	var normAttrs map[string]any
	if err := db.UnmarshalJSON(attrsJSON, &normAttrs); err != nil {
		return core.Contact{}, err
	}
	if first == cur.FirstName && last == cur.LastName && reflect.DeepEqual(normAttrs, cur.Attributes) {
		return cur, nil
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE contacts SET first_name = ?, last_name = ?, attributes = ?, version = version + 1, updated_at = ?
		WHERE id = ? AND version = ?`,
		first, last, attrsJSON, db.FormatTime(s.Clock.Now()), id, cur.Version)
	if err != nil {
		return core.Contact{}, fmt.Errorf("update contact %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return core.Contact{}, fmt.Errorf("contact %d changed concurrently: %w", id, core.ErrPrecondition)
	}
	return s.Get(ctx, tx, id)
}
