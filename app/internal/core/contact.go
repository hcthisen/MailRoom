package core

import "time"

// Contact is one person on the list (C-1). Email is always normalised
// (see NormalizeEmail). Tags are tag names sorted case-insensitively.
type Contact struct {
	ID              int64
	Email           string
	FirstName       string
	LastName        string
	Attributes      map[string]any
	Tags            []string
	Status          ContactStatus
	StatusChangedAt time.Time
	// UnsubscribeRequestedAt is set when the contact asked to unsubscribe
	// while its status could not become unsubscribed (bounced, complained,
	// suppressed_manual): a later reactivation lands on unsubscribed (U-3).
	UnsubscribeRequestedAt *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
	Version                int64
}
