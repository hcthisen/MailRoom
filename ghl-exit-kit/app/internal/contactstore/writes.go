package contactstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"

	"mailroom/internal/core"
)

// savepointSeq makes savepoint names unique, also when calls nest.
var savepointSeq atomic.Uint64

// atomically runs fn inside a SAVEPOINT on tx. When fn fails, everything it
// did, including hook handlers' writes, is rolled back and tx is left as
// before the call; fn's error is returned.
func atomically(ctx context.Context, tx *sql.Tx, fn func() error) error {
	name := fmt.Sprintf("contactstore_%d", savepointSeq.Add(1))
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return fmt.Errorf("contactstore: savepoint: %w", err)
	}
	if err := fn(); err != nil {
		bg := context.WithoutCancel(ctx)
		if _, rbErr := tx.ExecContext(bg, "ROLLBACK TO "+name); rbErr != nil {
			return errors.Join(err, fmt.Errorf("contactstore: roll back to savepoint: %w", rbErr))
		}
		if _, relErr := tx.ExecContext(bg, "RELEASE "+name); relErr != nil {
			return errors.Join(err, fmt.Errorf("contactstore: release savepoint: %w", relErr))
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, "RELEASE "+name); err != nil {
		return fmt.Errorf("contactstore: release savepoint: %w", err)
	}
	return nil
}

// Create inserts a contact, adds its tags and publishes ContactCreated
// (then TagAdded per tag). A contact created as suppressed_manual also gets
// its address's manual suppression row. It fails with ErrSuppressed if the
// address is in the suppressions table, ErrConflict if the email already exists, and a
// validation error for bad input. On any error, including a hook handler's,
// nothing of the call remains in tx.
func (s *Store) Create(ctx context.Context, tx *sql.Tx, in CreateInput) (c core.Contact, err error) {
	err = atomically(ctx, tx, func() error {
		c, err = s.create(ctx, tx, in)
		return err
	})
	return c, err
}

// Update applies in to contact id. ifVersion 0 skips the version check;
// otherwise a different current version fails with ErrPrecondition. The
// version is bumped only when something actually changes. On error nothing
// of the call remains in tx.
func (s *Store) Update(ctx context.Context, tx *sql.Tx, id int64, in UpdateInput, ifVersion int64) (c core.Contact, err error) {
	err = atomically(ctx, tx, func() error {
		c, err = s.update(ctx, tx, id, in, ifVersion)
		return err
	})
	return c, err
}

// AddTags adds tags to a contact, creating tags as needed. It publishes
// TagAdded only for tags the contact didn't have yet, and bumps the
// contact's version when any tag was added. On any error, including a hook
// handler's for a later tag, none of the tags is added.
func (s *Store) AddTags(ctx context.Context, tx *sql.Tx, contactID int64, names []string, source string) error {
	return atomically(ctx, tx, func() error {
		return s.addTagsToContact(ctx, tx, contactID, names, source)
	})
}

// RemoveTags removes tags from a contact. Unknown tag names and tags the
// contact doesn't have are ignored. The version is bumped when any tag
// was removed. On error nothing of the call remains in tx.
func (s *Store) RemoveTags(ctx context.Context, tx *sql.Tx, contactID int64, names []string) error {
	return atomically(ctx, tx, func() error {
		return s.removeTags(ctx, tx, contactID, names)
	})
}

// SetStatus moves a contact to status to. It returns changed=false (and no
// error) when the contact already has that status. Disallowed transitions
// (see CanTransition) return an error wrapping core.ErrForbidden. A change
// writes a contact_status_changes row, bumps the version and publishes
// StatusChanged inside tx. It keeps the suppressions table in step with
// suppressed_manual: moving to it adds the address's manual suppression
// row (with the note), moving from it to active removes that row.
//
// A move to active from bounced or suppressed_manual lands on unsubscribed
// instead when the contact asked to unsubscribe meanwhile (RecordOptOut);
// changed is then true and the contact's status is unsubscribed. On any
// error, including a hook handler's, nothing of the call remains in tx.
func (s *Store) SetStatus(ctx context.Context, tx *sql.Tx, contactID int64, to core.ContactStatus, ch StatusChange) (changed bool, err error) {
	err = atomically(ctx, tx, func() error {
		changed, err = s.setStatus(ctx, tx, contactID, to, ch)
		return err
	})
	return changed, err
}
