// Package sequences implements drip sequences (PRD §6.4 S-1..S-10): linear
// lists of steps (an email plus a delay after the previous step), their
// triggers (tag added, form sign-up, API/admin enrolment), exit conditions,
// re-entry policy and send window, the scheduler that turns due
// enrollments into queued messages exactly once per step, per-step stats,
// test sends and the YAML import/export format (I-3, docs/sequence-format.md).
//
// The package implements core.Enroller, so forms, imports and contacts can
// start enrolments without importing it, and subscribes to the hooks bus:
// a new trigger tag enrols the contact, a new exit tag or a status change
// away from active ends its enrolments.
package sequences

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"mailroom/internal/audit"
	"mailroom/internal/contactstore"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/hooks"
	"mailroom/internal/jobs"
	"mailroom/internal/settings"
)

// DefaultSchedulerInterval is how often the scheduler worker looks for due
// enrollments (ARCHITECTURE.md §8).
const DefaultSchedulerInterval = 60 * time.Second

// Deps are the Service's dependencies.
type Deps struct {
	DB *db.DB
	// Store reads contacts and adds exit tags (publishing TagAdded).
	Store *contactstore.Store
	// Content creates and updates step emails in the caller's transaction
	// and queues test sends.
	Content *content.Service
	// Bus is used by RegisterHooks when it is called with a nil bus.
	Bus *hooks.Bus
	// Jobs runs large enrolments ("sequences.enroll").
	Jobs *jobs.Runner
	// Settings provides the time zone and the default send window.
	Settings *settings.Service
	Clock    core.Clock   // default core.SystemClock
	Logger   *slog.Logger // default slog.Default()
	// SchedulerInterval overrides DefaultSchedulerInterval (tests).
	SchedulerInterval time.Duration
}

// Service manages sequences, steps and enrollments. API handlers, the
// admin UI, hooks and the scheduler call its methods.
type Service struct {
	db       *db.DB
	store    *contactstore.Store
	content  *content.Service
	bus      *hooks.Bus
	jobs     *jobs.Runner
	settings *settings.Service
	clock    core.Clock
	log      *slog.Logger
	interval time.Duration
}

var _ core.Enroller = (*Service)(nil)

// New returns a sequences service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.SchedulerInterval <= 0 {
		d.SchedulerInterval = DefaultSchedulerInterval
	}
	return &Service{
		db: d.DB, store: d.Store, content: d.Content, bus: d.Bus, jobs: d.Jobs,
		settings: d.Settings, clock: d.Clock, log: d.Logger, interval: d.SchedulerInterval,
	}
}

// now returns the current time.
func (s *Service) now() time.Time { return s.clock.Now().UTC() }

// logAudit writes an audit record for the actor in ctx inside tx.
func (s *Service) logAudit(ctx context.Context, tx *sql.Tx, op, targetType string, targetID int64, changes any) error {
	id := ""
	if targetID != 0 {
		id = strconv.FormatInt(targetID, 10)
	}
	return audit.Write(ctx, tx, audit.Entry{
		Operation: op, TargetType: targetType, TargetID: id, Changes: changes, At: s.clock.Now(),
	})
}

// location returns the install's time zone (UTC when the settings can't
// be read).
func (s *Service) location(ctx context.Context) *time.Location {
	st, err := s.appSettings(ctx)
	if err != nil {
		return time.UTC
	}
	return st.Location()
}

// appSettings returns the business settings (defaults without a service).
func (s *Service) appSettings(ctx context.Context) (settings.Settings, error) {
	if s.settings == nil {
		return settings.Defaults(), nil
	}
	st, err := s.settings.Get(ctx)
	if err != nil {
		return settings.Settings{}, fmt.Errorf("sequences: read settings: %w", err)
	}
	return st, nil
}
