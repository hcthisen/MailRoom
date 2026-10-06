// Package hooks is Mailroom's synchronous in-process event bus
// (ARCHITECTURE.md §4.3). Handlers run inside the publisher's transaction:
// a handler error rolls the publisher back, so handlers must be quick and
// must not do network I/O.
package hooks

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"mailroom/internal/core"
)

// TagAdded is published when a tag is newly added to a contact.
type TagAdded struct {
	ContactID int64
	TagID     int64
	TagName   string
	Source    string
}

// StatusChanged is published when a contact's status actually changes.
type StatusChanged struct {
	ContactID int64
	From      core.ContactStatus
	To        core.ContactStatus
	Source    string
	MessageID int64 // 0 when not caused by a message
}

// ContactCreated is published when a contact row is inserted.
type ContactCreated struct {
	ContactID int64
	Source    string
}

// Handler types, one per event.
type (
	TagAddedHandler       func(ctx context.Context, tx *sql.Tx, e TagAdded) error
	StatusChangedHandler  func(ctx context.Context, tx *sql.Tx, e StatusChanged) error
	ContactCreatedHandler func(ctx context.Context, tx *sql.Tx, e ContactCreated) error
)

// Bus dispatches events to registered handlers in registration order. The
// zero value is ready to use; a nil *Bus publishes to nobody. Registration
// and publishing are safe for concurrent use, but handlers are expected to
// be registered during wiring, before the app starts.
type Bus struct {
	mu             sync.RWMutex
	tagAdded       []TagAddedHandler
	statusChanged  []StatusChangedHandler
	contactCreated []ContactCreatedHandler
}

// New returns an empty bus.
func New() *Bus { return &Bus{} }

// OnTagAdded registers fn for TagAdded events.
func (b *Bus) OnTagAdded(fn func(ctx context.Context, tx *sql.Tx, e TagAdded) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tagAdded = append(b.tagAdded, fn)
}

// OnStatusChanged registers fn for StatusChanged events.
func (b *Bus) OnStatusChanged(fn func(ctx context.Context, tx *sql.Tx, e StatusChanged) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.statusChanged = append(b.statusChanged, fn)
}

// OnContactCreated registers fn for ContactCreated events.
func (b *Bus) OnContactCreated(fn func(ctx context.Context, tx *sql.Tx, e ContactCreated) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contactCreated = append(b.contactCreated, fn)
}

// PublishTagAdded calls every TagAdded handler in order and stops at the
// first error, which it returns wrapped.
func (b *Bus) PublishTagAdded(ctx context.Context, tx *sql.Tx, e TagAdded) error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	hs := b.tagAdded
	b.mu.RUnlock()
	for i, h := range hs {
		if err := h(ctx, tx, e); err != nil {
			return fmt.Errorf("hooks: TagAdded handler %d: %w", i, err)
		}
	}
	return nil
}

// PublishStatusChanged calls every StatusChanged handler in order and stops
// at the first error, which it returns wrapped.
func (b *Bus) PublishStatusChanged(ctx context.Context, tx *sql.Tx, e StatusChanged) error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	hs := b.statusChanged
	b.mu.RUnlock()
	for i, h := range hs {
		if err := h(ctx, tx, e); err != nil {
			return fmt.Errorf("hooks: StatusChanged handler %d: %w", i, err)
		}
	}
	return nil
}

// PublishContactCreated calls every ContactCreated handler in order and
// stops at the first error, which it returns wrapped.
func (b *Bus) PublishContactCreated(ctx context.Context, tx *sql.Tx, e ContactCreated) error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	hs := b.contactCreated
	b.mu.RUnlock()
	for i, h := range hs {
		if err := h(ctx, tx, e); err != nil {
			return fmt.Errorf("hooks: ContactCreated handler %d: %w", i, err)
		}
	}
	return nil
}
