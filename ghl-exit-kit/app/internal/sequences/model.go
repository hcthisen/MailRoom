package sequences

import (
	"time"

	"mailroom/internal/settings"
)

// Sequence statuses. Paused sequences keep and accept enrollments, but the
// scheduler sends nothing until they are active again.
const (
	StatusActive = "active"
	StatusPaused = "paused"
)

// Re-entry policies (S-4).
const (
	// ReentryNever enrols a contact at most once, ever (the default).
	ReentryNever = "never"
	// ReentryAfterCompletion allows a new enrolment once the most recent
	// one completed; an exited contact can't come back.
	ReentryAfterCompletion = "after_completion"
	// ReentryAlways allows a new enrolment whenever none is active.
	ReentryAlways = "always"
)

// Enrollment statuses.
const (
	EnrollmentActive    = "active"
	EnrollmentCompleted = "completed"
	EnrollmentExited    = "exited"
)

// Exit reasons (enrollments.exit_reason).
const (
	ExitNotActive       = "not_active"
	ExitTag             = "exit_tag"
	ExitManual          = "manual"
	ExitSequenceDeleted = "sequence_deleted"
	ExitStepDeleted     = "step_deleted"
	// ExitUnsubscribed is written by the events package when the contact
	// chooses "Only stop these emails" on the unsubscribe page
	// (events.ExitUnsubscribedSequence). Such a contact is never enrolled in
	// that sequence again, whatever its re-entry policy (SkipUnsubscribed).
	ExitUnsubscribed = "unsubscribed_sequence"
)

// Reasons an enrolment is skipped, as reported by the enrollments API.
const (
	SkipNotFound      = "not_found"      // no such contact
	SkipNotActive     = "not_active"     // the contact's status is not active
	SkipAlreadyActive = "already_active" // an enrolment in the sequence is active
	SkipReentry       = "reentry"        // the re-entry policy forbids another enrolment
	SkipExitTag       = "exit_tag"       // the contact has one of the exit tags
	SkipNoSteps       = "no_steps"       // the sequence has no steps yet
	SkipUnsubscribed  = "unsubscribed"   // the contact stopped this sequence from its unsubscribe page
)

// Step deletion modes for enrollments waiting for the deleted step (S-7).
const (
	OnActiveAdvance = "advance"
	OnActiveExit    = "exit"
)

// Reorder modes for active enrollments (S-7).
const (
	// OnActiveKeep (the default) keeps each enrollment's next step by ID.
	OnActiveKeep = "keep"
	// OnActiveEnd moves active enrollments to the end: they complete and
	// get no further steps (messages already queued are still sent).
	OnActiveEnd = "end"
)

// Limits.
const (
	MaxNameLength  = 100
	MaxExitTags    = 20
	MaxSteps       = 100
	MaxDelay       = 365 * 24 * 60 * 60 // one year, in seconds
	MaxEnrollBatch = 50_000             // contacts per enrolment request
	// EnrollJobThreshold: requests for more contacts run as a job (202).
	EnrollJobThreshold = 500
	// SchedulerBatch is the number of due enrollments read per query.
	SchedulerBatch = 200
)

// JobEnroll is the job type of large enrolments.
const JobEnroll = "sequences.enroll"

// Sequence is a drip sequence with its steps, as returned by the API.
type Sequence struct {
	ID         int64                `json:"id" example:"4"`
	Name       string               `json:"name" example:"Community welcome"`
	Status     string               `json:"status" enum:"active,paused" doc:"paused: nothing is sent until the sequence is active again" example:"active"`
	TriggerTag *string              `json:"trigger_tag" doc:"Adding this tag to an active contact enrols it; null means no tag trigger" example:"joined-community"`
	ExitTags   []string             `json:"exit_tags" nullable:"false" doc:"A contact with any of these tags leaves the sequence before its next step" example:"[\"customer\"]"`
	ExitAddTag *string              `json:"exit_add_tag" doc:"Tag added when a contact leaves because of an exit tag or because it is no longer active; null for none" example:"community-drip-exited"`
	Reentry    string               `json:"reentry" enum:"never,after_completion,always" doc:"Whether a contact can be enrolled again" example:"never"`
	SendWindow *settings.SendWindow `json:"send_window" doc:"Steps due outside this window wait for its next opening, in the install's time zone; null uses the default send window from the settings (if any)"`
	Steps      []SequenceStep       `json:"steps" nullable:"false" doc:"The steps in order"`
	Version    int64                `json:"version" doc:"Increases with every change to the sequence or its steps; sent as the ETag" example:"3"`
	CreatedAt  time.Time            `json:"created_at"`
	UpdatedAt  time.Time            `json:"updated_at"`
}

// SequenceStep is one step of a sequence: an email sent a delay after the
// previous step (or after the enrolment, for the first step).
type SequenceStep struct {
	ID           int64     `json:"id" example:"11"`
	Position     int       `json:"position" doc:"1-based position in the sequence" example:"1"`
	DelaySeconds int64     `json:"delay_seconds" doc:"Delay after the previous step was queued (the first step: after enrolment)" example:"86400"`
	Delay        string    `json:"delay" doc:"delay_seconds in the YAML notation (0, 90m, 36h, 2d)" example:"1d"`
	EmailID      int64     `json:"email_id" doc:"The email sent by this step (see the emails API for its body)" example:"21"`
	Subject      string    `json:"subject" example:"Welcome to the community"`
	Format       string    `json:"format" enum:"markdown,html" example:"markdown"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SequenceEnrollment is one contact's passage through a sequence.
type SequenceEnrollment struct {
	ID               int64      `json:"id" example:"301"`
	SequenceID       int64      `json:"sequence_id" example:"4"`
	ContactID        int64      `json:"contact_id" example:"42"`
	Email            string     `json:"email" example:"alice@example.com"`
	Status           string     `json:"status" enum:"active,completed,exited" example:"active"`
	NextStepID       *int64     `json:"next_step_id" doc:"The step sent next; null when not active" example:"12"`
	NextStepPosition *int       `json:"next_step_position" example:"2"`
	NextRunAt        *time.Time `json:"next_run_at" doc:"When the next step is due; null when not active"`
	ExitReason       string     `json:"exit_reason" doc:"Why it exited: not_active, exit_tag, manual, sequence_deleted, step_deleted or unsubscribed_sequence (the contact chose 'only stop these emails' on the unsubscribe page)" example:""`
	Source           string     `json:"source" doc:"What enrolled the contact: form:<id>, tag:<name>, api, admin, migration" example:"tag:joined-community"`
	StartedAt        time.Time  `json:"started_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	FinishedAt       *time.Time `json:"finished_at"`
}

// SequenceStats are the numbers of a sequence (S-9).
type SequenceStats struct {
	SequenceID int64               `json:"sequence_id" example:"4"`
	Active     int                 `json:"active" doc:"Enrollments in progress" example:"120"`
	Completed  int                 `json:"completed" doc:"Enrollments that received every step" example:"800"`
	Exited     int                 `json:"exited" doc:"Enrollments that left early" example:"45"`
	Steps      []SequenceStepStats `json:"steps" nullable:"false"`
}

// SequenceStepStats are the numbers of one step. Test sends are not
// counted.
type SequenceStepStats struct {
	StepID       int64 `json:"step_id" example:"11"`
	Position     int   `json:"position" example:"1"`
	Waiting      int   `json:"waiting" doc:"Active enrollments whose next step is this one" example:"30"`
	Queued       int   `json:"queued" doc:"Messages queued or being sent" example:"0"`
	Sent         int   `json:"sent" doc:"Messages handed to the email provider" example:"950"`
	Delivered    int   `json:"delivered" example:"940"`
	Bounced      int   `json:"bounced" example:"6"`
	Complained   int   `json:"complained" example:"1"`
	Clicked      int   `json:"clicked" doc:"Unique contacts who clicked a link in this step" example:"310"`
	Unsubscribed int   `json:"unsubscribed" doc:"Unsubscribes from this step's messages" example:"4"`
}
