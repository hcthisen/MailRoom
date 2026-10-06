package contacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"mailroom/internal/api"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
)

// Get returns one contact with its tags.
func (s *Service) Get(ctx context.Context, id int64) (Contact, error) {
	c, err := s.store.Get(ctx, nil, id)
	if err != nil {
		return Contact{}, err
	}
	return fromCore(c), nil
}

// CreateInput is a new contact.
type CreateInput struct {
	Email      string
	FirstName  string
	LastName   string
	Attributes map[string]any
	Tags       []string
	Status     core.ContactStatus // "" = active
	Consent    *Consent           // optional; see Create
}

// Create inserts a contact with its tags. A consent event (type api for
// tokens, admin for UI users) is written when consent is given or the
// status is active or pending. It fails with ErrConflict for an existing
// email and ErrSuppressed for a suppressed or erased address.
func (s *Service) Create(ctx context.Context, in CreateInput) (Contact, error) {
	var out core.Contact
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		c, err := s.store.Create(ctx, tx, contactstore.CreateInput{
			Email: in.Email, FirstName: in.FirstName, LastName: in.LastName,
			Attributes: in.Attributes, Tags: in.Tags, Status: in.Status, Source: changeSource(ctx),
		})
		if err != nil {
			return err
		}
		if in.Consent != nil || c.Status == core.StatusActive || c.Status == core.StatusPending {
			if err := s.recordConsent(ctx, tx, c.ID, in.Consent); err != nil {
				return err
			}
		}
		changes := map[string]any{"status": c.Status, "tags": c.Tags, "attributes": sortedKeys(c.Attributes)}
		if c.Status == core.StatusSuppressedManual {
			// The store added the manual suppression row with the contact,
			// so remove-suppression finds the address.
			changes["suppression_added"] = true
		}
		out = c
		return s.logAudit(ctx, tx, "contact.create", "contact", c.ID, changes)
	})
	if err != nil {
		return Contact{}, err
	}
	return fromCore(out), nil
}

// recordConsent writes the consent event for a contact created or updated
// by the actor in ctx.
func (s *Service) recordConsent(ctx context.Context, tx *sql.Tx, contactID int64, c *Consent) error {
	if c == nil {
		c = &Consent{}
	}
	origin := strings.TrimSpace(c.Source)
	if utf8.RuneCountInString(origin) > 200 {
		return core.Invalid("consent.source", "is longer than 200 characters")
	}
	typ, source := consentFor(ctx, origin)
	e := contactstore.ConsentEvent{ContactID: contactID, Type: typ, Source: source, ConsentText: c.Text}
	if c.OccurredAt != nil {
		if c.OccurredAt.After(s.clock.Now()) {
			return core.Invalid("consent.occurred_at", "must not be in the future")
		}
		e.OccurredAt = c.OccurredAt.UTC()
	}
	return s.store.RecordConsent(ctx, tx, e)
}

// UpsertInput creates or updates a contact by email. Nil names are left
// unchanged; attributes are merged (a null value removes a key) and tags
// are added.
type UpsertInput struct {
	Email      string
	FirstName  *string
	LastName   *string
	Attributes map[string]any
	Tags       []string
	Consent    *Consent // recorded on create, and on update when given
}

// Upsert creates the contact (status active) when the email is new, or
// updates its names and attributes and adds tags. It never changes the
// status of an existing contact. created reports which happened.
func (s *Service) Upsert(ctx context.Context, in UpsertInput) (c Contact, created bool, err error) {
	var out core.Contact
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.store.GetByEmail(ctx, tx, in.Email)
		switch {
		case errors.Is(err, core.ErrNotFound):
			created = true
			attrs := maps.Clone(in.Attributes)
			maps.DeleteFunc(attrs, func(_ string, v any) bool { return v == nil })
			out, err = s.store.Create(ctx, tx, contactstore.CreateInput{
				Email: in.Email, FirstName: deref(in.FirstName), LastName: deref(in.LastName),
				Attributes: attrs, Tags: in.Tags, Source: changeSource(ctx),
			})
			if err != nil {
				return err
			}
			if err := s.recordConsent(ctx, tx, out.ID, in.Consent); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if out, err = s.store.Update(ctx, tx, cur.ID, contactstore.UpdateInput{
				FirstName: in.FirstName, LastName: in.LastName, Attributes: in.Attributes,
			}, 0); err != nil {
				return err
			}
			if len(in.Tags) > 0 {
				if err := s.store.AddTags(ctx, tx, cur.ID, in.Tags, changeSource(ctx)); err != nil {
					return err
				}
			}
			if in.Consent != nil {
				if err := s.recordConsent(ctx, tx, cur.ID, in.Consent); err != nil {
					return err
				}
			}
			if out, err = s.store.Get(ctx, tx, cur.ID); err != nil {
				return err
			}
			if out.Version == cur.Version && in.Consent == nil {
				return nil // nothing changed: no audit entry
			}
		}
		return s.logAudit(ctx, tx, "contact.upsert", "contact", out.ID, map[string]any{
			"created": created, "tags": in.Tags, "attributes": sortedKeys(in.Attributes),
		})
	})
	if err != nil {
		return Contact{}, false, err
	}
	return fromCore(out), created, nil
}

// UpdateInput is a partial update of a contact.
type UpdateInput struct {
	FirstName  *string
	LastName   *string
	Attributes map[string]any // merged; a nil value removes the key
}

// Update changes names and attributes. ifVersion 0 skips the version check;
// a different current version fails with ErrPrecondition.
func (s *Service) Update(ctx context.Context, id int64, in UpdateInput, ifVersion int64) (Contact, error) {
	var out core.Contact
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.store.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if out, err = s.store.Update(ctx, tx, id, contactstore.UpdateInput{
			FirstName: in.FirstName, LastName: in.LastName, Attributes: in.Attributes,
		}, ifVersion); err != nil {
			return err
		}
		if out.Version == cur.Version {
			return nil
		}
		return s.logAudit(ctx, tx, "contact.update", "contact", id, map[string]any{"fields": changedFields(cur, out)})
	})
	if err != nil {
		return Contact{}, err
	}
	return fromCore(out), nil
}

// changedFields lists the changed field names (attributes as
// "attributes.<key>"), never their values: the audit log outlives an erase.
func changedFields(a, b core.Contact) []string {
	var f []string
	if a.FirstName != b.FirstName {
		f = append(f, "first_name")
	}
	if a.LastName != b.LastName {
		f = append(f, "last_name")
	}
	keys := map[string]bool{}
	for k := range a.Attributes {
		keys[k] = true
	}
	for k := range b.Attributes {
		keys[k] = true
	}
	for _, k := range sortedKeys(keys) {
		av, aok := a.Attributes[k]
		bv, bok := b.Attributes[k]
		if aok != bok || !reflect.DeepEqual(av, bv) {
			f = append(f, "attributes."+k)
		}
	}
	return f
}

// SetStatus moves a contact to status to under the §6 rules, recorded with
// source api (tokens) or admin (UI users) and the note. Refused transitions
// wrap ErrForbidden. changed is false when the contact already had it.
//
// The status and the suppressions table stay in step: moving to
// suppressed_manual adds the address's hash (reason manual), and moving
// suppressed_manual → active removes that manual row (§6: "removing a
// manual suppression"). Reactivating any other contact whose address is
// suppressed fails with ErrConflict, because the sender would skip every
// message: remove the suppression first.
//
// Reactivating a bounced or manually suppressed contact that asked to
// unsubscribe meanwhile lands on unsubscribed instead of active
// (contactstore.RecordOptOut); the returned contact shows it. Setting a
// bounced, complained or suppressed_manual contact to unsubscribed (a
// request received by phone or email) keeps its status but records the
// request the same way, with an audit entry: changed is true, and the
// returned contact has UnsubscribeRequestedAt set. A move to
// active from pending, unsubscribed or suppressed_manual is new consent
// for marketing email: it is appended to the consent log (C-3) as an
// admin or api event with the note as its text.
func (s *Service) SetStatus(ctx context.Context, id int64, to core.ContactStatus, note string, ifVersion int64) (c Contact, changed bool, err error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxNoteLength {
		return Contact{}, false, core.Invalid("note", "is longer than %d characters", maxNoteLength)
	}
	var out core.Contact
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.store.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		hash := core.EmailHash(cur.Email)
		reason, err := suppressionReason(ctx, tx, hash)
		if err != nil {
			return err
		}
		if to == core.StatusUnsubscribed && keepsStatusOnOptOut(cur.Status) {
			if changed, err = s.store.RecordOptOut(ctx, tx, id); err != nil {
				return err
			}
			if out, err = s.store.Get(ctx, tx, id); err != nil || !changed {
				return err
			}
			return s.logAudit(ctx, tx, "contact.optout_recorded", "contact", id, map[string]any{
				"status_kept": cur.Status, "note": note,
			})
		}
		unsuppress := false
		if to == core.StatusActive && cur.Status != core.StatusActive {
			switch {
			case reason == "":
			case reason == ReasonManual && cur.Status == core.StatusSuppressedManual:
				unsuppress = true
			default:
				return fmt.Errorf("the address is suppressed (%s); remove the suppression first: %w", reason, core.ErrConflict)
			}
		}
		changed, err = s.store.SetStatus(ctx, tx, id, to, contactstore.StatusChange{
			Source: changeSource(ctx), Actor: core.ActorFromContext(ctx).String(), Note: note,
		})
		if err != nil {
			return err
		}
		if out, err = s.store.Get(ctx, tx, id); err != nil {
			return err
		}
		if !changed {
			return nil
		}
		if err := s.recordReactivation(ctx, tx, id, cur.Status, out.Status, note); err != nil {
			return err
		}
		// The store keeps the manual suppression row in step with the
		// status change; record what it did.
		changes := map[string]any{"status": map[string]any{"from": cur.Status, "to": out.Status}}
		switch {
		case unsuppress:
			changes["suppression_removed"] = true
		case to == core.StatusSuppressedManual:
			changes["suppression_added"] = reason == ""
		}
		return s.logAudit(ctx, tx, "contact.status", "contact", id, changes)
	})
	if err != nil {
		return Contact{}, false, err
	}
	return fromCore(out), changed, nil
}

// keepsStatusOnOptOut reports whether an unsubscribe request from a
// contact with this status is recorded (contactstore.RecordOptOut) instead
// of changing the status.
func keepsStatusOnOptOut(st core.ContactStatus) bool {
	switch st {
	case core.StatusBounced, core.StatusComplained, core.StatusSuppressedManual:
		return true
	}
	return false
}

// recordReactivation appends the consent event of a status change from
// "from" to "to" made by the actor in ctx, when it is one: a move to
// active from pending (consent confirmed elsewhere), unsubscribed
// (resubscribed elsewhere) or suppressed_manual (suppression lifted). A
// bounced contact's reactivation is about deliverability, not consent, and
// records nothing. The note is the consent text.
func (s *Service) recordReactivation(ctx context.Context, tx *sql.Tx, contactID int64, from, to core.ContactStatus, note string) error {
	if to != core.StatusActive {
		return nil
	}
	switch from {
	case core.StatusPending, core.StatusUnsubscribed, core.StatusSuppressedManual:
	default:
		return nil
	}
	typ, source := consentFor(ctx, "status:"+string(from))
	return s.store.RecordConsent(ctx, tx, contactstore.ConsentEvent{
		ContactID: contactID, Type: typ, Source: source, ConsentText: note,
	})
}

// AddTags adds tags to a contact (creating tags as needed). TagAdded hooks
// fire for tags the contact didn't have, e.g. to start a sequence.
func (s *Service) AddTags(ctx context.Context, id int64, tags []string) (Contact, error) {
	if len(tags) == 0 {
		return Contact{}, core.Invalid("tags", "must contain at least one tag")
	}
	return s.changeTags(ctx, id, "contact.tags.add", tags, func(tx *sql.Tx) error {
		return s.store.AddTags(ctx, tx, id, tags, changeSource(ctx))
	})
}

// RemoveTag removes one tag from a contact. Removing a tag the contact
// doesn't have is not an error.
func (s *Service) RemoveTag(ctx context.Context, id int64, tag string) (Contact, error) {
	return s.changeTags(ctx, id, "contact.tags.remove", []string{tag}, func(tx *sql.Tx) error {
		return s.store.RemoveTags(ctx, tx, id, []string{tag})
	})
}

func (s *Service) changeTags(ctx context.Context, id int64, op string, tags []string, fn func(tx *sql.Tx) error) (Contact, error) {
	var out core.Contact
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.store.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		if out, err = s.store.Get(ctx, tx, id); err != nil {
			return err
		}
		if out.Version == cur.Version {
			return nil
		}
		return s.logAudit(ctx, tx, op, "contact", id, map[string]any{"tags": tags})
	})
	if err != nil {
		return Contact{}, err
	}
	return fromCore(out), nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// sortedKeys returns the keys of m in order (an empty slice for none).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
