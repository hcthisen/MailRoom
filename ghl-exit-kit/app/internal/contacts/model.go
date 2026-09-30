package contacts

import (
	"time"

	"mailroom/internal/core"
)

// Contact is a contact as returned by the API.
type Contact struct {
	ID              int64              `json:"id" example:"42"`
	Email           string             `json:"email" example:"alice@example.com"`
	FirstName       string             `json:"first_name" example:"Alice"`
	LastName        string             `json:"last_name" example:"Smith"`
	Attributes      map[string]any     `json:"attributes" doc:"Custom attributes (JSON object)"`
	Tags            []string           `json:"tags" doc:"Tag names, sorted case-insensitively" example:"[\"customer\",\"newsletter\"]"`
	Status          core.ContactStatus `json:"status" enum:"pending,active,unsubscribed,bounced,complained,suppressed_manual" doc:"Global status; only active contacts receive marketing email" example:"active"`
	StatusChangedAt time.Time          `json:"status_changed_at"`
	// UnsubscribeRequestedAt: see core.Contact.
	UnsubscribeRequestedAt *time.Time `json:"unsubscribe_requested_at,omitempty" doc:"Set when the contact asked to unsubscribe while bounced, complained or manually suppressed (its status can't become unsubscribed then). Lifting the bounce or the suppression later makes it unsubscribed, not active"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	Version                int64      `json:"version" doc:"Version used for ETag / If-Match" example:"3"`
}

// fromCore converts a core contact to its API form.
func fromCore(c core.Contact) Contact {
	attrs := c.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}
	tags := c.Tags
	if tags == nil {
		tags = []string{}
	}
	return Contact{
		ID: c.ID, Email: c.Email, FirstName: c.FirstName, LastName: c.LastName,
		Attributes: attrs, Tags: tags, Status: c.Status,
		StatusChangedAt: c.StatusChangedAt, UnsubscribeRequestedAt: c.UnsubscribeRequestedAt,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Version: c.Version,
	}
}

// Consent describes the consent that came with a new contact.
type Consent struct {
	Source     string     `json:"source,omitempty" maxLength:"200" doc:"Where consent was given, e.g. \"webinar-2026\"; recorded as api:<token name>:<source>" example:"webinar-2026"`
	OccurredAt *time.Time `json:"occurred_at,omitempty" doc:"When consent was given (RFC 3339); default now"`
	Text       string     `json:"text,omitempty" maxLength:"5000" doc:"The consent text the person agreed to" example:"Yes, send me the newsletter. Clicks are tracked per person."`
}

// maxNoteLength bounds free-text notes (status changes, suppressions).
const maxNoteLength = 500
