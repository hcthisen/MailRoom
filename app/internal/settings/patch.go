package settings

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// Patch is a partial update of Settings (PATCH /settings). Omitted fields
// are left unchanged. default_send_window may be null to remove it; null
// for any other field counts as omitted. domain and version are read-only.
type Patch struct {
	Sender            *SenderPatch         `json:"sender,omitempty"`
	CompanyFooter     *string              `json:"company_footer,omitempty" nullable:"false" maxLength:"2000"`
	OwnerEmail        *string              `json:"owner_email,omitempty" nullable:"false"`
	AlertEmail        *string              `json:"alert_email,omitempty" nullable:"false" doc:"Empty string clears it (alerts then go to owner_email)"`
	Timezone          *string              `json:"timezone,omitempty" nullable:"false" example:"Europe/Berlin"`
	Tracking          *TrackingPatch       `json:"tracking,omitempty"`
	Sending           *SendingPatch        `json:"sending,omitempty"`
	CircuitBreaker    *CircuitBreakerPatch `json:"circuit_breaker,omitempty"`
	DefaultSendWindow OptionalSendWindow   `json:"default_send_window,omitempty"`
}

// SenderPatch is a partial Sender.
type SenderPatch struct {
	FromName  *string `json:"from_name,omitempty" nullable:"false" maxLength:"200"`
	FromEmail *string `json:"from_email,omitempty" nullable:"false" example:"news@example.com"`
	ReplyTo   *string `json:"reply_to,omitempty" nullable:"false" doc:"Empty string clears it"`
}

// TrackingPatch is a partial Tracking.
type TrackingPatch struct {
	Clicks *bool `json:"clicks,omitempty" nullable:"false"`
	Opens  *bool `json:"opens,omitempty" nullable:"false"`
}

// SendingPatch is a partial Sending.
type SendingPatch struct {
	RateOverridePerSec *float64 `json:"rate_override_per_sec,omitempty" nullable:"false" minimum:"0" maximum:"1000"`
	RateFraction       *float64 `json:"rate_fraction,omitempty" nullable:"false" exclusiveMinimum:"0" maximum:"1"`
}

// CircuitBreakerPatch is a partial CircuitBreaker.
type CircuitBreakerPatch struct {
	MinMessages    *int     `json:"min_messages,omitempty" nullable:"false" minimum:"1" maximum:"1000000"`
	HardBounceRate *float64 `json:"hard_bounce_rate,omitempty" nullable:"false" exclusiveMinimum:"0" maximum:"1"`
	ComplaintRate  *float64 `json:"complaint_rate,omitempty" nullable:"false" exclusiveMinimum:"0" maximum:"1"`
}

// OptionalSendWindow tells "absent" from "null" from a value in a PATCH
// body: Set is true when the field was present; Value is nil for null.
type OptionalSendWindow struct {
	Set   bool
	Value *SendWindow
}

// UnmarshalJSON records that the field was present.
func (o *OptionalSendWindow) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.Value = nil
		return nil
	}
	var w SendWindow
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	o.Value = &w
	return nil
}

// MarshalJSON encodes the value (null when unset or null).
func (o OptionalSendWindow) MarshalJSON() ([]byte, error) {
	if o.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}

// Schema documents the field as a nullable SendWindow.
func (OptionalSendWindow) Schema(r huma.Registry) *huma.Schema {
	return r.Schema(reflect.TypeFor[SendWindow](), true, "SendWindow")
}

// apply returns s with p applied.
func (p Patch) apply(s Settings) Settings {
	s = s.clone()
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	if p.Sender != nil {
		set(&s.Sender.FromName, p.Sender.FromName)
		set(&s.Sender.FromEmail, p.Sender.FromEmail)
		set(&s.Sender.ReplyTo, p.Sender.ReplyTo)
	}
	set(&s.CompanyFooter, p.CompanyFooter)
	set(&s.OwnerEmail, p.OwnerEmail)
	set(&s.AlertEmail, p.AlertEmail)
	set(&s.Timezone, p.Timezone)
	if t := p.Tracking; t != nil {
		if t.Clicks != nil {
			s.Tracking.Clicks = *t.Clicks
		}
		if t.Opens != nil {
			s.Tracking.Opens = *t.Opens
		}
	}
	if x := p.Sending; x != nil {
		if x.RateOverridePerSec != nil {
			s.Sending.RateOverridePerSec = *x.RateOverridePerSec
		}
		if x.RateFraction != nil {
			s.Sending.RateFraction = *x.RateFraction
		}
	}
	if c := p.CircuitBreaker; c != nil {
		if c.MinMessages != nil {
			s.CircuitBreaker.MinMessages = *c.MinMessages
		}
		if c.HardBounceRate != nil {
			s.CircuitBreaker.HardBounceRate = *c.HardBounceRate
		}
		if c.ComplaintRate != nil {
			s.CircuitBreaker.ComplaintRate = *c.ComplaintRate
		}
	}
	if p.DefaultSendWindow.Set {
		if p.DefaultSendWindow.Value == nil {
			s.DefaultSendWindow = nil
		} else {
			w := p.DefaultSendWindow.Value.clone()
			s.DefaultSendWindow = &w
		}
	}
	return s
}
