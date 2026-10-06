// Package contactstore holds the core contact operations every package uses
// (ARCHITECTURE.md §4.5): reads, create, update, tags, status transitions
// (§6) and the consent log. Writes take the caller's *sql.Tx and publish
// hooks inside it. Each write runs in a SAVEPOINT: when it fails (a
// validation error, a refused transition, a hook handler error) everything
// it did, including earlier hook handlers' writes, is undone and the tx is
// left as it was, so a bulk caller may record the failure and continue.
package contactstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/hooks"
)

// Store is the contact store. Bus may be nil (no hooks). Clock is required.
type Store struct {
	DB    *db.DB
	Bus   *hooks.Bus
	Clock core.Clock
}

// querier returns q, or the read pool when q is nil.
func (s *Store) querier(q db.Querier) db.Querier {
	if q == nil {
		return s.DB.Read
	}
	return q
}

const contactColumns = `id, email, first_name, last_name, attributes, status, status_changed_at, created_at, updated_at, version, unsubscribe_requested_at`

// Get returns the contact with its tags. Pass nil q to read from the read
// pool, or a tx to read inside a transaction. Missing IDs wrap ErrNotFound.
func (s *Store) Get(ctx context.Context, q db.Querier, id int64) (core.Contact, error) {
	q = s.querier(q)
	c, err := scanContact(q.QueryRowContext(ctx, `SELECT `+contactColumns+` FROM contacts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Contact{}, fmt.Errorf("contact %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return core.Contact{}, fmt.Errorf("get contact %d: %w", id, err)
	}
	if c.Tags, err = contactTags(ctx, q, id); err != nil {
		return core.Contact{}, err
	}
	return c, nil
}

// GetByEmail returns the contact with the given address (normalised first).
// An invalid address is a validation error; an unknown one wraps ErrNotFound.
func (s *Store) GetByEmail(ctx context.Context, q db.Querier, email string) (core.Contact, error) {
	norm, err := core.NormalizeEmail(email)
	if err != nil {
		return core.Contact{}, err
	}
	q = s.querier(q)
	var id int64
	err = q.QueryRowContext(ctx, `SELECT id FROM contacts WHERE email = ?`, norm).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Contact{}, fmt.Errorf("contact with that email: %w", core.ErrNotFound)
	}
	if err != nil {
		return core.Contact{}, fmt.Errorf("get contact by email: %w", err)
	}
	return s.Get(ctx, q, id)
}

// IsSuppressed reports whether the normalised address is in the
// suppressions table (erased or manually suppressed addresses).
func (s *Store) IsSuppressed(ctx context.Context, q db.Querier, normalizedEmail string) (bool, error) {
	var one int
	err := s.querier(q).QueryRowContext(ctx,
		`SELECT 1 FROM suppressions WHERE email_hash = ?`, core.EmailHash(normalizedEmail)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check suppression: %w", err)
	}
	return true, nil
}

func scanContact(row *sql.Row) (core.Contact, error) {
	var (
		c                          core.Contact
		attrs, status              string
		statusChanged, created, up string
		optOut                     sql.NullString
	)
	if err := row.Scan(&c.ID, &c.Email, &c.FirstName, &c.LastName, &attrs, &status,
		&statusChanged, &created, &up, &c.Version, &optOut); err != nil {
		return core.Contact{}, err
	}
	if optOut.Valid {
		t, err := db.ParseTime(optOut.String)
		if err != nil {
			return core.Contact{}, err
		}
		c.UnsubscribeRequestedAt = &t
	}
	c.Status = core.ContactStatus(status)
	c.Attributes = map[string]any{}
	if err := db.UnmarshalJSON(attrs, &c.Attributes); err != nil {
		return core.Contact{}, err
	}
	var err error
	if c.StatusChangedAt, err = db.ParseTime(statusChanged); err != nil {
		return core.Contact{}, err
	}
	if c.CreatedAt, err = db.ParseTime(created); err != nil {
		return core.Contact{}, err
	}
	if c.UpdatedAt, err = db.ParseTime(up); err != nil {
		return core.Contact{}, err
	}
	return c, nil
}

// contactTags returns the contact's tag names, sorted case-insensitively.
func contactTags(ctx context.Context, q db.Querier, contactID int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT t.name FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id
		WHERE ct.contact_id = ? ORDER BY t.name COLLATE NOCASE`, contactID)
	if err != nil {
		return nil, fmt.Errorf("list contact tags: %w", err)
	}
	defer rows.Close()
	tags := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tags = append(tags, name)
	}
	return tags, rows.Err()
}

// currentState returns the version and status of a contact; missing IDs wrap ErrNotFound.
func currentState(ctx context.Context, q db.Querier, id int64) (version int64, status core.ContactStatus, err error) {
	var st string
	err = q.QueryRowContext(ctx, `SELECT version, status FROM contacts WHERE id = ?`, id).Scan(&version, &st)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("contact %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return 0, "", fmt.Errorf("read contact %d: %w", id, err)
	}
	return version, core.ContactStatus(st), nil
}

// touch bumps a contact's version and updated_at.
func (s *Store) touch(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE contacts SET version = version + 1, updated_at = ? WHERE id = ?`,
		db.FormatTime(s.Clock.Now()), id)
	if err != nil {
		return fmt.Errorf("update contact %d: %w", id, err)
	}
	return nil
}
