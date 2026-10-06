package forms

import (
	"time"

	"mailroom/internal/contactstore"
)

// Submitted field names with a fixed meaning.
const (
	FieldEmail     = "email"
	FieldFirstName = "first_name"
	FieldLastName  = "last_name"
	// AttrPrefix starts the key of a custom attribute field: "attr.company"
	// is stored as the contact attribute "company".
	AttrPrefix = "attr."
	// HoneypotField is the hidden field bots fill in (F-5). A submission
	// with a value in it is pretended to succeed and recorded as spam.
	HoneypotField = "website"
)

// Limits.
const (
	MaxNameLength        = 200
	MaxFields            = 20
	MaxLabelLength       = 100
	MaxTags              = 20
	MaxConsentTextLength = 2000
	MaxRedirectURLLength = 2048
	MaxOrigins           = 20
	MaxOriginLength      = 255
	// MaxValueLength bounds each submitted attribute value.
	MaxValueLength = 500
	// MaxUserAgentLength bounds the user agent stored with a sign-up, in
	// bytes: the bound consent events use.
	MaxUserAgentLength = contactstore.MaxUserAgentLength
	// MaxBodyBytes bounds a submission's request body.
	MaxBodyBytes = 64 << 10
	// PublicIDLength is the length of a form's public slug.
	PublicIDLength = 10
)

// Opt-in timing (F-6).
const (
	// OptinTTL is how long a confirmation link stays valid; unconfirmed
	// sign-ups are deleted after it.
	OptinTTL = 7 * 24 * time.Hour
	// ResendInterval is the minimum time between two confirmation emails
	// to the same contact, so a form can't be used to flood an inbox.
	ResendInterval = 10 * time.Minute
	// MaxConfirmationsPerDay bounds the confirmation emails sent to one
	// address in any 24 hours, so a form can't be used to keep emailing
	// an unsubscribed or never-confirming address.
	MaxConfirmationsPerDay = 3
	// MaxConfirmationsPerMonth bounds the confirmation emails sent to one
	// address in any ConfirmationWindow. The count is kept by address, so
	// it survives the deletion of an expired pending contact.
	MaxConfirmationsPerMonth = 5
	// ConfirmationWindow is the long window of MaxConfirmationsPerMonth.
	ConfirmationWindow = 30 * 24 * time.Hour
	// ResubscribeCooldown: after a resubscribe confirmation email, no
	// other one goes to that contact for this long (unless its status
	// changes in between), so an unsubscribed address gets at most one
	// "confirm that you want our emails again" email a month, however
	// often someone submits it.
	ResubscribeCooldown = 30 * 24 * time.Hour
	// MaxConfirmationsPerHour bounds the opt-in and resubscribe
	// confirmation emails of all forms together in any hour, and
	// MaxFormConfirmationsPerHour those of one form. When a cap is
	// reached, submissions are still accepted and recorded but no
	// confirmation email goes out, and the owner is alerted
	// (AlertOptinCap), so a public form can't be used to spray email at
	// many addresses from the verified domain.
	MaxConfirmationsPerHour     = 200
	MaxFormConfirmationsPerHour = 100
	// ExpiryInterval is how often the optin-expiry worker runs.
	ExpiryInterval = time.Hour
)

// DefaultConsentText is the consent text of a form that sets none. It says
// that signing up means receiving email and that clicks in the emails are
// recorded per person (K-4).
const DefaultConsentText = "By signing up you agree to receive our emails. You can unsubscribe at any time with the link at the bottom of every email. " +
	"We record which links in our emails each subscriber clicks, so we can send you content that interests you."

// AlertOptinCap is the alert key raised when a confirmation email cap is
// reached (per form: AlertOptinCap + ":form:<id>").
const AlertOptinCap = "optin_confirmation_cap"

// Submission outcomes (form_submissions.outcome).
const (
	OutcomeCreated            = "created"
	OutcomeUpdated            = "updated"
	OutcomeOptinPending       = "optin_pending"
	OutcomeResubscribePending = "resubscribe_pending"
	OutcomeRejected           = "rejected"
	OutcomeSpam               = "spam"
	OutcomeRateLimited        = "rate_limited"
)

// Pending opt-in kinds (pending_optins.kind).
const (
	KindOptin       = "optin"
	KindResubscribe = "resubscribe"
)

// FormField is one input of a form besides the always-present email.
type FormField struct {
	Key      string `json:"key" maxLength:"69" doc:"first_name, last_name or attr.<key> (a contact attribute; key of 1-64 letters, digits or underscores)" example:"first_name"`
	Label    string `json:"label" required:"false" maxLength:"100" doc:"Label shown next to the input; defaults to one derived from the key" example:"First name"`
	Required bool   `json:"required" required:"false" doc:"Whether the visitor must fill it in" example:"false"`
}

// Form is a sign-up form (F-1).
type Form struct {
	ID             int64       `json:"id" example:"1"`
	PublicID       string      `json:"public_id" doc:"Random slug used in the public URLs /f/{public_id}" example:"k3m9x2p7qa"`
	Name           string      `json:"name" example:"Newsletter sign-up"`
	Fields         []FormField `json:"fields" doc:"Inputs besides email, which is always present and required"`
	Tags           []string    `json:"tags" doc:"Tags added to the contact on sign-up (after confirmation with double opt-in)" example:"[\"newsletter\"]"`
	SequenceID     *int64      `json:"sequence_id" doc:"Sequence the contact is enrolled in on sign-up; null = none" example:"2"`
	DoubleOptin    bool        `json:"double_optin" doc:"Send a confirmation email and apply tags and the sequence only after the click (F-6), also for existing active contacts" example:"false"`
	ConsentText    string      `json:"consent_text" doc:"Consent text shown on the form and stored in the consent log with every sign-up"`
	RedirectURL    string      `json:"redirect_url" doc:"Absolute https URL visitors are sent to after submitting; empty = the hosted thank-you page" example:"https://example.com/thanks"`
	AllowedOrigins []string    `json:"allowed_origins" doc:"Web origins allowed to submit with JavaScript (CORS), e.g. https://example.com" example:"[\"https://example.com\"]"`
	HostedURL      string      `json:"hosted_url" doc:"Public hosted form page" example:"https://newsletter.example.com/f/k3m9x2p7qa"`
	Endpoint       string      `json:"endpoint" doc:"URL the form posts to (form-encoded or JSON)" example:"https://newsletter.example.com/f/k3m9x2p7qa"`
	Version        int64       `json:"version" example:"1"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// FormInput is a new form. ConsentText "" means DefaultConsentText.
type FormInput struct {
	Name           string
	Fields         []FormField
	Tags           []string
	SequenceID     *int64 // nil or 0 = none
	DoubleOptin    bool
	ConsentText    string
	RedirectURL    string
	AllowedOrigins []string
}

// FormPatch is a partial update of a form; nil members stay unchanged.
// SequenceID 0 removes the sequence and ConsentText "" restores the
// default text.
type FormPatch struct {
	Name           *string
	Fields         *[]FormField
	Tags           *[]string
	SequenceID     *int64
	DoubleOptin    *bool
	ConsentText    *string
	RedirectURL    *string
	AllowedOrigins *[]string
}

// Snippet is the embeddable HTML of a form (F-4).
type Snippet struct {
	HTML      string `json:"html" doc:"Copy-paste HTML form that posts to the endpoint, with the honeypot field and the consent text"`
	HostedURL string `json:"hosted_url" doc:"Public hosted form page, for linking to directly" example:"https://newsletter.example.com/f/k3m9x2p7qa"`
	Endpoint  string `json:"endpoint" doc:"URL the form posts to" example:"https://newsletter.example.com/f/k3m9x2p7qa"`
}

// Submission is one recorded submission of a form.
type Submission struct {
	ID        int64     `json:"id" example:"17"`
	Outcome   string    `json:"outcome" enum:"created,updated,optin_pending,resubscribe_pending,rejected,spam,rate_limited" doc:"What happened: created/updated a contact, sent a confirmation (optin_pending, resubscribe_pending), silently rejected a suppressed/bounced/complained address, honeypot spam, or refused by the rate limit" example:"created"`
	ContactID *int64    `json:"contact_id" doc:"The contact, when there is one (null for spam, rate-limited, erased or since-deleted contacts)" example:"42"`
	Email     string    `json:"email" doc:"The contact's current address; empty without a contact" example:"alice@example.com"`
	CreatedAt time.Time `json:"created_at"`
}
