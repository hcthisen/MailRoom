// Package settings owns Mailroom's business settings (PRD §6.12
// "Settings"): sender identity, footer, owner and alert addresses, time
// zone, tracking, send rate, circuit-breaker thresholds and the default
// send window. They are stored as JSON under the key "app" in the settings
// table, seeded from the environment on first start and afterwards changed
// only through the API. The sending domain comes from the configuration
// and is read-only (D-7).
//
// Other packages keep their own JSON documents in the same table through
// GetRaw and PutRaw (e.g. content stores the base layout under "layout").
package settings

import (
	"time"
)

// Key is the settings-table key of the business settings document.
const Key = "app"

// Settings is the typed business settings document.
type Settings struct {
	// Domain is the one sending domain, from DOMAIN. Read-only.
	Domain string `json:"domain" readOnly:"true" doc:"The sending domain (read-only; changing it means re-provisioning)" example:"example.com"`
	// Version increases with every change; it is the ETag.
	Version int64 `json:"version" readOnly:"true" doc:"Increases with every change; sent as the ETag" example:"3"`

	Sender         Sender         `json:"sender"`
	CompanyFooter  string         `json:"company_footer" maxLength:"2000" doc:"Footer text of every marketing email: company name, address, registration no." example:"Example Ltd, 1 High Street, London"`
	OwnerEmail     string         `json:"owner_email" doc:"The owner's address: test sends and, unless alert_email is set, alerts" example:"owner@example.org"`
	AlertEmail     string         `json:"alert_email" doc:"Where alert emails go; empty means owner_email" example:""`
	Timezone       string         `json:"timezone" doc:"IANA time zone of the install, used for send windows and schedules" example:"Europe/London"`
	Tracking       Tracking       `json:"tracking"`
	Sending        Sending        `json:"sending"`
	CircuitBreaker CircuitBreaker `json:"circuit_breaker"`
	// DefaultSendWindow is the send window of sequences that have none of
	// their own, applied by the sequences scheduler at send time; nil means
	// no restriction.
	DefaultSendWindow *SendWindow `json:"default_send_window" doc:"Send window used by sequences that have none of their own; null means any time"`
}

// Sender is the default From and Reply-To identity.
type Sender struct {
	FromName  string `json:"from_name" maxLength:"200" doc:"Display name in From" example:"Example Newsletter"`
	FromEmail string `json:"from_email" doc:"From address; must be on the sending domain" example:"news@example.com"`
	ReplyTo   string `json:"reply_to" doc:"Reply-To address; must be on the sending domain; empty means none" example:"hello@example.com"`
}

// Tracking switches click and open tracking (K-1, K-2).
type Tracking struct {
	Clicks bool `json:"clicks" doc:"Rewrite links for per-contact click tracking" example:"true"`
	Opens  bool `json:"opens" doc:"Add an open-tracking pixel (off by default: unreliable with Apple Mail privacy)" example:"false"`
}

// Sending tunes the global send rate (B-4).
type Sending struct {
	RateOverridePerSec float64 `json:"rate_override_per_sec" minimum:"0" maximum:"1000" doc:"Fixed send rate in messages per second; 0 means automatic (rate_fraction of the SES MaxSendRate)" example:"0"`
	RateFraction       float64 `json:"rate_fraction" exclusiveMinimum:"0" maximum:"1" doc:"Share of the SES MaxSendRate to use when rate_override_per_sec is 0" example:"0.8"`
}

// CircuitBreaker holds the broadcast auto-pause thresholds (B-6).
type CircuitBreaker struct {
	MinMessages    int     `json:"min_messages" minimum:"1" maximum:"1000000" doc:"Rates are checked only after this many messages of a broadcast" example:"200"`
	HardBounceRate float64 `json:"hard_bounce_rate" exclusiveMinimum:"0" maximum:"1" doc:"Pause when the hard-bounce rate exceeds this fraction" example:"0.05"`
	ComplaintRate  float64 `json:"complaint_rate" exclusiveMinimum:"0" maximum:"1" doc:"Pause when the complaint rate exceeds this fraction" example:"0.001"`
}

// Defaults returns the settings of a fresh install before seeding.
func Defaults() Settings {
	return Settings{
		Timezone:       "UTC",
		Tracking:       Tracking{Clicks: true, Opens: false},
		Sending:        Sending{RateOverridePerSec: 0, RateFraction: 0.8},
		CircuitBreaker: CircuitBreaker{MinMessages: 200, HardBounceRate: 0.05, ComplaintRate: 0.001},
	}
}

// AlertAddress returns where alerts go: AlertEmail, else OwnerEmail.
func (s Settings) AlertAddress() string {
	if s.AlertEmail != "" {
		return s.AlertEmail
	}
	return s.OwnerEmail
}

// Location returns the settings' time zone (UTC if it can't be loaded).
func (s Settings) Location() *time.Location {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// clone returns a deep copy, so cached values can't be mutated by callers.
func (s Settings) clone() Settings {
	if s.DefaultSendWindow != nil {
		w := s.DefaultSendWindow.clone()
		s.DefaultSendWindow = &w
	}
	return s
}
