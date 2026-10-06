package broadcasts

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/audit"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
	"mailroom/internal/segments"
	"mailroom/internal/settings"
)

// JobSnapshot is the job type of the audience snapshot (B-3).
const JobSnapshot = "broadcast.snapshot"

// SnapshotBatchSize is how many messages one snapshot transaction queues.
const SnapshotBatchSize = 500

// StarterInterval is how often the broadcast-starter worker runs.
const StarterInterval = 10 * time.Second

// Deps are the Service's dependencies.
type Deps struct {
	DB *db.DB
	// Content creates, changes, copies and deletes the broadcasts' emails
	// and queues test sends.
	Content *content.Service
	// Segments compiles segment audiences. When it also implements
	// SegmentCounter (segments.Service does), the send dry run and the
	// stats report the segment's missing tag names.
	Segments core.SegmentResolver
	// Jobs runs the snapshot job; Submit is called inside the operation's
	// transaction.
	Jobs *jobs.Runner
	// Alerter raises the circuit-breaker alert. May be nil (the trip is
	// then only logged).
	Alerter  core.Alerter
	Settings *settings.Service
	// BaseURL is the public base URL (cfg.BaseURL, no trailing slash). It
	// fills Broadcast.ArchiveURL; "" leaves it empty.
	BaseURL string
	Clock   core.Clock   // default core.SystemClock
	Logger  *slog.Logger // default slog.Default()
}

// SegmentCounter is implemented by segments.Service. It reports, among the
// counts, the tag names a segment refers to that no longer exist.
type SegmentCounter interface {
	CountSegment(ctx context.Context, id int64) (segments.SegmentCounts, error)
}

// Service implements broadcasts. API handlers and the admin UI call its
// methods; the starter worker and the snapshot job run on it.
type Service struct {
	db       *db.DB
	content  *content.Service
	segments core.SegmentResolver
	jobs     *jobs.Runner
	alerter  core.Alerter
	settings *settings.Service
	clock    core.Clock
	log      *slog.Logger
	baseURL  string
}

// New returns a broadcasts service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{
		db: d.DB, content: d.Content, segments: d.Segments, jobs: d.Jobs, alerter: d.Alerter,
		settings: d.Settings, clock: d.Clock, log: d.Logger, baseURL: strings.TrimRight(d.BaseURL, "/"),
	}
}

// RegisterJobs registers the "broadcast.snapshot" job type on Deps.Jobs
// with jobs.Resumable(). It panics if Deps.Jobs is nil (a wiring error).
func (s *Service) RegisterJobs() {
	if s.jobs == nil {
		panic("broadcasts: RegisterJobs needs Deps.Jobs")
	}
	s.jobs.Register(JobSnapshot, s.runSnapshot, jobs.Resumable())
}

// logAudit writes an audit record about broadcast id inside tx.
func (s *Service) logAudit(ctx context.Context, tx *sql.Tx, op string, id int64, changes any) error {
	return audit.Write(ctx, tx, audit.Entry{
		Operation: op, TargetType: "broadcast", TargetID: strconv.FormatInt(id, 10), Changes: changes, At: s.clock.Now(),
	})
}

// now returns the current time formatted for the database.
func (s *Service) now() string { return db.FormatTime(s.clock.Now()) }

// systemCtx returns ctx acting as the system actor (worker actions).
func systemCtx(ctx context.Context) context.Context {
	return core.WithActor(ctx, core.Actor{Type: core.ActorSystem, Name: "broadcast-starter"})
}
