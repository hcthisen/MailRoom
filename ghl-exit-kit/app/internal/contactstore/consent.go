package contactstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Consent event types (consent_events.type).
const (
	ConsentSignup         = "signup"
	ConsentOptinRequested = "optin_requested"
	ConsentOptinConfirmed = "optin_confirmed"
	ConsentImport         = "import"
	ConsentAdmin          = "admin"
	ConsentAPI            = "api"
	ConsentResubscribe    = "resubscribe"
)

// ConsentEvent is one entry of the append-only consent log (C-3).
type ConsentEvent struct {
	ContactID   int64
	Type        string // one of the Consent* constants
	Source      string // "form:<public_id>", "import:<consent_source>", "api:<token name>", "admin:<user email>"
	FormID      int64  // 0 = none
	IP          string
	UserAgent   string
	ConsentText string    // the consent text shown
	OccurredAt  time.Time // when consent was given; zero = now (import: consent_date)
}

// MaxUserAgentLength bounds stored user agents, in bytes; longer ones are
// cut to this length (keeping valid UTF-8).
const MaxUserAgentLength = 512

// RecordConsent appends a consent event.
func (s *Store) RecordConsent(ctx context.Context, tx *sql.Tx, e ConsentEvent) error {
	switch e.Type {
	case ConsentSignup, ConsentOptinRequested, ConsentOptinConfirmed, ConsentImport,
		ConsentAdmin, ConsentAPI, ConsentResubscribe:
	default:
		return core.Invalid("type", "unknown consent event type %q", e.Type)
	}
	if e.ContactID <= 0 {
		return core.Invalid("contact_id", "is required")
	}
	source := strings.TrimSpace(e.Source)
	if source == "" {
		return core.Invalid("source", "is required")
	}
	ua := e.UserAgent
	if len(ua) > MaxUserAgentLength {
		ua = strings.ToValidUTF8(ua[:MaxUserAgentLength], "")
	}
	now := s.Clock.Now()
	occurred := e.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO consent_events (contact_id, type, source, form_id, ip, user_agent, consent_text, occurred_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ContactID, e.Type, source, db.NullID(e.FormID), e.IP, ua, e.ConsentText,
		db.FormatTime(occurred), db.FormatTime(now))
	if db.IsForeignKeyViolation(err) {
		return fmt.Errorf("contact %d: %w", e.ContactID, core.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("record consent for contact %d: %w", e.ContactID, err)
	}
	return nil
}
