// Package audit writes and reads the audit log (PRD G-8): every write
// through the API or UI is logged with its actor, time, operation, target
// and changed fields, in the same transaction as the change itself.
package audit

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Entry is one audit record to write.
type Entry struct {
	Operation  string    // dotted verb, e.g. "contact.update", "settings.update"
	TargetType string    // e.g. "contact", "settings"; "" when there is none
	TargetID   string    // e.g. "42"; "" when there is none
	Changes    any       // JSON-encodable: changed fields, created values, ...; nil = {}
	At         time.Time // zero = the package clock's now
}

// clockBox lets clocks of different concrete types share one atomic pointer.
type clockBox struct{ c core.Clock }

var clock atomic.Pointer[clockBox]

func init() { clock.Store(&clockBox{c: core.SystemClock{}}) }

// SetClock sets the clock used when an entry has no At time (tests use a
// fake clock). A nil clock restores the system clock.
func SetClock(c core.Clock) {
	if c == nil {
		c = core.SystemClock{}
	}
	clock.Store(&clockBox{c: c})
}

func now() time.Time { return clock.Load().c.Now() }

// Log writes an audit record for the actor in ctx (core.ActorFromContext).
// Call it with the transaction that makes the change, so the record and the
// change commit or roll back together. Never pass secrets in changes.
func Log(ctx context.Context, q db.Querier, op, targetType, targetID string, changes any) error {
	return Write(ctx, q, Entry{Operation: op, TargetType: targetType, TargetID: targetID, Changes: changes})
}

// Write writes e for the actor in ctx. Services with their own clock set
// e.At so tests stay deterministic.
func Write(ctx context.Context, q db.Querier, e Entry) error {
	if e.Operation == "" {
		return fmt.Errorf("audit: operation is required")
	}
	changes, err := db.MarshalJSON(e.Changes)
	if err != nil {
		return fmt.Errorf("audit %s: %w", e.Operation, err)
	}
	at := e.At
	if at.IsZero() {
		at = now()
	}
	a := core.ActorFromContext(ctx)
	if _, err := q.ExecContext(ctx,
		`INSERT INTO audit_log (actor_type, actor_id, actor_name, operation, target_type, target_id, changes, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Type, a.ID, a.Name, e.Operation, e.TargetType, e.TargetID, changes, db.FormatTime(at)); err != nil {
		return fmt.Errorf("audit %s: %w", e.Operation, err)
	}
	return nil
}

// Change is the conventional value of one changed field in Changes.
type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// Diff adds field to changes when from != to (compared with ==, so pass
// comparable values). It returns changes for chaining; a nil map is created.
func Diff[T comparable](changes map[string]any, field string, from, to T) map[string]any {
	if changes == nil {
		changes = map[string]any{}
	}
	if from != to {
		changes[field] = Change{From: from, To: to}
	}
	return changes
}
