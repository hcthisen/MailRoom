package broadcasts

import (
	"time"
)

// Broadcast statuses (B-2).
const (
	StatusDraft     = "draft"
	StatusScheduled = "scheduled"
	StatusSending   = "sending"
	StatusPaused    = "paused"
	StatusSent      = "sent"
	StatusCancelled = "cancelled"
)

// Pause reasons stored in broadcasts.paused_reason.
const (
	PauseManual         = "manual"
	PauseCircuitBreaker = "circuit_breaker"
	// PauseSnapshotFailed marks a broadcast whose audience snapshot failed
	// permanently (e.g. its segment filter no longer compiles). Resuming it
	// retries the snapshot.
	PauseSnapshotFailed = "snapshot_failed"
)

// Audience types.
const (
	AudienceAll     = "all"
	AudienceSegment = "segment"
)

// SkipCancelled is the error recorded on queued messages skipped because
// their broadcast was cancelled.
const SkipCancelled = "cancelled"

// Field limits.
const (
	MaxNameLength     = 200
	MaxFromNameLength = 200
)

// BroadcastAudience says who receives a broadcast: every active contact, or the
// active members of a segment.
type BroadcastAudience struct {
	Type        string `json:"type" enum:"all,segment" doc:"all: every active contact; segment: the active members of segment_id" example:"segment"`
	SegmentID   int64  `json:"segment_id,omitempty" minimum:"0" doc:"The segment (type segment only)" example:"3"`
	SegmentName string `json:"segment_name,omitempty" readOnly:"true" doc:"The segment's name when the audience was set" example:"Customers"`
}

// BroadcastEmail is the content of a broadcast (its own email).
type BroadcastEmail struct {
	ID        int64  `json:"id" readOnly:"true" doc:"The email's ID (owned by this broadcast)" example:"12"`
	Subject   string `json:"subject" doc:"Subject line; may use merge fields" example:"Our autumn news"`
	Preheader string `json:"preheader" doc:"Preview text shown after the subject" example:"Three things worth knowing"`
	Format    string `json:"format" enum:"markdown,html" doc:"markdown or html" example:"markdown"`
	Body      string `json:"body,omitempty" doc:"The content (omitted in lists)" example:"Hi {{first_name | \"there\"}},\n\nHere is our news."`
}

// Broadcast is a broadcast as returned by the API.
type Broadcast struct {
	ID       int64             `json:"id" example:"7"`
	Name     string            `json:"name" doc:"Internal name" example:"October newsletter"`
	Status   string            `json:"status" enum:"draft,scheduled,sending,paused,sent,cancelled" example:"draft"`
	Audience BroadcastAudience `json:"audience"`
	FromName *string           `json:"from_name" doc:"From display name; null means the settings default" example:"Example News"`
	ReplyTo  *string           `json:"reply_to" doc:"Reply-To address on the sending domain; null means the settings default, \"\" none" example:"hello@example.com"`
	Email    BroadcastEmail    `json:"email"`
	// PausedReason is set while paused: manual, circuit_breaker or
	// snapshot_failed.
	PausedReason    string     `json:"paused_reason" enum:",manual,circuit_breaker,snapshot_failed,ses_paused" doc:"Why the broadcast is paused; empty unless paused" example:""`
	ScheduledAt     *time.Time `json:"scheduled_at" doc:"When a scheduled broadcast starts"`
	StartedAt       *time.Time `json:"started_at" doc:"When sending started"`
	FinishedAt      *time.Time `json:"finished_at" doc:"When the broadcast was sent or cancelled"`
	RecipientsTotal int64      `json:"recipients_total" doc:"Messages queued by the snapshot (final once snapshot_done_at is set)" example:"1180"`
	SnapshotJobID   *int64     `json:"snapshot_job_id" doc:"The broadcast.snapshot job; poll GET /jobs/{id}" example:"41"`
	SnapshotDoneAt  *time.Time `json:"snapshot_done_at" doc:"When every recipient had been queued"`
	ArchiveURL      string     `json:"archive_url,omitempty" readOnly:"true" doc:"The public web archive page (B-10): set once a broadcast to everyone has gone out (sending, paused or sent). Rendered without contact data, merge fields show their fallbacks, no tracking." example:"https://newsletter.example.com/archive/7"`
	Version         int64      `json:"version" doc:"Increases with every change of the broadcast or its email; sent as the ETag" example:"3"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`

	rowVersion   int64 // broadcasts.version
	emailVersion int64 // emails.version
}

// combinedVersion is the ETag version of a broadcast: it increases when
// either the broadcast row or its email changes (both only ever grow).
func combinedVersion(rowVersion, emailVersion int64) int64 { return rowVersion + emailVersion - 1 }

// editable reports whether the broadcast's settings and content may change.
func editable(status string) bool { return status == StatusDraft || status == StatusScheduled }
