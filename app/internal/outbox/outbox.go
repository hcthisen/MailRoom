// Package outbox is the only way to create messages rows (ARCHITECTURE.md
// §4.4). Every email gets a queued row before it is sent; the UNIQUE
// constraints on (broadcast_id, contact_id) and (enrollment_id, step_id)
// make enqueueing idempotent, so a crash or retry never double-sends.
//
// Suppression is not checked here; the sender checks it immediately before
// the provider call (D-6).
package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// MessageSpec describes one message to enqueue. Zero IDs mean NULL.
type MessageSpec struct {
	Kind           core.MessageKind
	BroadcastID    int64
	EnrollmentID   int64
	StepID         int64
	EmailID        int64 // content source; 0 when SystemTemplate is set
	ContactID      int64
	SystemTemplate string         // core.Template*; "" when EmailID is set
	ToEmail        string         // required; the contact's email for contact messages
	Data           map[string]any // extra template data (messages.data)
	NotBefore      *time.Time     // earliest send time; nil = now
}

// Enqueue inserts one queued message and returns its id with created=true.
// If the row already exists (same broadcast+contact or enrollment+step), it
// returns the existing id with created=false, so callers are idempotent.
// The spec is validated first; errors wrap core.ErrValidation.
func Enqueue(ctx context.Context, q db.Querier, clock core.Clock, s MessageSpec) (id int64, created bool, err error) {
	to, err := Validate(s)
	if err != nil {
		return 0, false, err
	}
	data, err := db.MarshalJSON(s.Data)
	if err != nil {
		return 0, false, core.Invalid("data", "is not JSON-serialisable: %v", err)
	}
	now := db.FormatTime(clock.Now())
	next := now
	if s.NotBefore != nil {
		next = db.FormatTime(*s.NotBefore)
	}

	err = q.QueryRowContext(ctx, `
		INSERT INTO messages (kind, broadcast_id, enrollment_id, step_id, email_id, system_template,
		                      contact_id, to_email, data, status, next_attempt_at, queued_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		string(s.Kind), db.NullID(s.BroadcastID), db.NullID(s.EnrollmentID), db.NullID(s.StepID),
		db.NullID(s.EmailID), s.SystemTemplate, db.NullID(s.ContactID), to, data,
		next, now, now,
	).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("outbox: insert message: %w", err)
	}

	// The insert hit a UNIQUE constraint: return the existing row.
	switch s.Kind {
	case core.KindBroadcast:
		err = q.QueryRowContext(ctx,
			`SELECT id FROM messages WHERE broadcast_id = ? AND contact_id = ?`,
			s.BroadcastID, s.ContactID).Scan(&id)
	case core.KindSequence:
		err = q.QueryRowContext(ctx,
			`SELECT id FROM messages WHERE enrollment_id = ? AND step_id = ?`,
			s.EnrollmentID, s.StepID).Scan(&id)
	default:
		err = fmt.Errorf("unexpected conflict for %s message", s.Kind)
	}
	if err != nil {
		return 0, false, fmt.Errorf("outbox: find existing message: %w", err)
	}
	return id, false, nil
}

// Validate checks a spec against its kind and returns the normalised
// recipient address. Rules per kind:
//
//   - broadcast: BroadcastID, ContactID and EmailID; no enrollment/step.
//   - sequence: EnrollmentID, StepID, ContactID and EmailID; no broadcast.
//   - optin: ContactID and SystemTemplate optin_confirm or resubscribe_confirm.
//   - test: EmailID or a system template; no broadcast/enrollment/step
//     (tests must not count in stats or block real sends).
//   - alert: SystemTemplate alert; no contact, broadcast, enrollment or step.
//
// Every kind needs ToEmail and exactly one of EmailID or SystemTemplate.
func Validate(s MessageSpec) (string, error) {
	if !s.Kind.Valid() {
		return "", core.Invalid("kind", "unknown message kind %q", s.Kind)
	}
	to, err := core.NormalizeEmail(s.ToEmail)
	if err != nil {
		return "", core.Invalid("to_email", "%s", validationMessage(err))
	}
	for _, f := range []struct {
		name string
		v    int64
	}{{"broadcast_id", s.BroadcastID}, {"enrollment_id", s.EnrollmentID}, {"step_id", s.StepID}, {"email_id", s.EmailID}, {"contact_id", s.ContactID}} {
		if f.v < 0 {
			return "", core.Invalid(f.name, "must not be negative")
		}
	}
	switch s.SystemTemplate {
	case "", core.TemplateOptinConfirm, core.TemplateResubscribeConfirm, core.TemplateAlert:
	default:
		return "", core.Invalid("system_template", "unknown system template %q", s.SystemTemplate)
	}
	if (s.EmailID == 0) == (s.SystemTemplate == "") {
		return "", core.Invalid("email_id", "exactly one of email_id and system_template is required")
	}

	require := func(field string, ok bool, msg string) error {
		if !ok {
			return core.Invalid(field, "%s for %s messages", msg, s.Kind)
		}
		return nil
	}
	var checks []error
	switch s.Kind {
	case core.KindBroadcast:
		checks = []error{
			require("broadcast_id", s.BroadcastID > 0, "is required"),
			require("contact_id", s.ContactID > 0, "is required"),
			require("email_id", s.EmailID > 0, "is required"),
			require("enrollment_id", s.EnrollmentID == 0 && s.StepID == 0, "must be empty"),
		}
	case core.KindSequence:
		checks = []error{
			require("enrollment_id", s.EnrollmentID > 0, "is required"),
			require("step_id", s.StepID > 0, "is required"),
			require("contact_id", s.ContactID > 0, "is required"),
			require("email_id", s.EmailID > 0, "is required"),
			require("broadcast_id", s.BroadcastID == 0, "must be empty"),
		}
	case core.KindOptin:
		checks = []error{
			require("contact_id", s.ContactID > 0, "is required"),
			require("system_template", s.SystemTemplate == core.TemplateOptinConfirm || s.SystemTemplate == core.TemplateResubscribeConfirm,
				"must be optin_confirm or resubscribe_confirm"),
			require("broadcast_id", s.BroadcastID == 0 && s.EnrollmentID == 0 && s.StepID == 0, "must be empty"),
		}
	case core.KindTest:
		checks = []error{
			require("broadcast_id", s.BroadcastID == 0 && s.EnrollmentID == 0 && s.StepID == 0, "must be empty"),
		}
	case core.KindAlert:
		checks = []error{
			require("system_template", s.SystemTemplate == core.TemplateAlert, "must be alert"),
			require("contact_id", s.ContactID == 0, "must be empty"),
			require("broadcast_id", s.BroadcastID == 0 && s.EnrollmentID == 0 && s.StepID == 0, "must be empty"),
		}
	}
	for _, err := range checks {
		if err != nil {
			return "", err
		}
	}
	return to, nil
}

func validationMessage(err error) string {
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		return ve.Message
	}
	return err.Error()
}
