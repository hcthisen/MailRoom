package core

import (
	"context"
	"database/sql"
	"time"
)

// System template names used in messages.system_template and RenderInput.
const (
	TemplateOptinConfirm       = "optin_confirm"
	TemplateResubscribeConfirm = "resubscribe_confirm"
	TemplateAlert              = "alert"
	// TemplateReply renders a reply from the inbox (kind reply): data
	// subject, body (plain text) and optionally quote_header and quote.
	TemplateReply = "reply"
)

// Data keys of reply messages (kind reply), written by inbound and read by
// content (rendering) and sending (threading headers).
const (
	DataReplySubject    = "subject"
	DataReplyBody       = "body"
	DataReplyQuote      = "quote"        // the quoted original, plain text
	DataReplyQuoteHead  = "quote_header" // "On <date>, <sender> wrote:"
	DataReplyInReplyTo  = "in_reply_to"  // Message-ID of the received mail, without <>
	DataReplyReferences = "references"   // References of the received mail plus its Message-ID
)

// InboundReceiver records SES "Received" notifications (N-1). Implemented
// by inbound, used by events (the SNS webhook): Received runs inside the
// webhook's transaction and does no network I/O; the message itself is
// fetched later by inbound's worker. A malformed notification returns an
// error wrapping ErrValidation.
type InboundReceiver interface {
	Received(ctx context.Context, tx *sql.Tx, snsMessageID, message string) error
}

// Renderer turns a message into its final subject, HTML and text. It is
// implemented by content and used by sending (at send time) and previews.
type Renderer interface {
	Render(ctx context.Context, in RenderInput) (Rendered, error)
}

// RenderInput describes what to render.
type RenderInput struct {
	MessageID      int64          // 0 for previews: tracking/unsubscribe links become "#preview"
	EmailID        int64          // 0 when SystemTemplate is set
	SystemTemplate string         // TemplateOptinConfirm | TemplateResubscribeConfirm | TemplateAlert
	Contact        *Contact       // nil for alert/test-to-address; merge fields then use fallbacks
	ToEmail        string         // recipient address
	Data           map[string]any // messages.data
	Marketing      bool           // true: wrap in layout with unsubscribe footer, rewrite links for click tracking
}

// Rendered is the output of a Renderer.
type Rendered struct {
	Subject        string
	HTML           string
	Text           string
	UnsubscribeURL string // "" when !Marketing
}

// SegmentResolver compiles a saved segment to SQL. Implemented by segments,
// used by broadcasts and contacts (bulk tag).
type SegmentResolver interface {
	// WhereSQL returns a SQL boolean expression over alias "c" (contacts)
	// plus its args. It never includes the status filter; callers add
	// "c.status = 'active'" when sending.
	WhereSQL(ctx context.Context, segmentID int64) (string, []any, error)
}

// TagReferrer is implemented by saved records that refer to tags by name
// (segments). contacts calls it inside the transaction of a tag rename,
// merge or delete, so those references keep selecting the same contacts.
type TagReferrer interface {
	// ReplaceTagName rewrites every reference to the tag name from
	// (ignoring ASCII case) to name to, inside tx, and returns how many
	// records changed.
	ReplaceTagName(ctx context.Context, tx *sql.Tx, from, to string) (int, error)
	// SegmentsUsingTag returns the IDs of the records that refer to the tag
	// name (ignoring ASCII case), in ID order.
	SegmentsUsingTag(ctx context.Context, tx *sql.Tx, name string) ([]int64, error)
}

// Enroller enrols contacts in sequences. Implemented by sequences, used by
// forms, contacts (tag trigger via hooks) and imports.
type Enroller interface {
	// Enroll respects the sequence's reentry policy and returns (0, nil)
	// when the enrolment is skipped by it.
	Enroll(ctx context.Context, tx *sql.Tx, sequenceID, contactID int64, opts EnrollOptions) (enrollmentID int64, err error)
}

// EnrollOptions tunes an enrolment.
type EnrollOptions struct {
	Source  string     // "form:<public_id>" | "tag:<name>" | "api" | "admin" | "migration"
	AtStep  int        // 0/1 = first step; N = start at position N
	StartAt *time.Time // nil = now; next_run_at = StartAt + step.delay
}

// EmailProvider sends one fully built message. SES in production, a fake in
// tests. Errors should be (or wrap) *ProviderError.
type EmailProvider interface {
	Send(ctx context.Context, m OutgoingEmail) (providerMessageID string, err error)
}

// OutgoingEmail is a message ready for the provider.
type OutgoingEmail struct {
	From    string
	To      string
	ReplyTo string
	Raw     []byte            // full RFC 5322 message
	Tags    map[string]string // provider message tags (e.g. SES message tags)
}

// Alerter raises an operational alert email to the owner (PRD O-5).
// Implemented by sending; used by sending itself, system (backups, certificates)
// and broadcasts (circuit breaker). Alerts are never suppressed. Implementations
// de-duplicate by key: the same key is sent at most once per 6 hours.
// Alert only enqueues: pass the caller's transaction as tx to join it, or nil
// to let the implementation use its own. It never blocks on the network.
type Alerter interface {
	Alert(ctx context.Context, tx *sql.Tx, key, title, body string) error
}
