// Package system is Mailroom's operations package (PRD §6.12 "System",
// §6.13 L-4 and L-6, §6.14 O-3 to O-5 and X-5):
//
//   - health, version and SES account status for the API;
//   - doctor: the install checks shared by `mailroom doctor` and
//     GET /api/v1/system/doctor (DNS, SES, SNS, HTTPS, backups, clock, disk,
//     configuration), each with a status and a fix hint;
//   - backups: VACUUM INTO copies under <data>/backups, kept for 14 days and
//     optionally uploaded to S3 (server-side encrypted), taken nightly, on
//     demand through a job, or offline from the CLI; download and restore;
//   - retention: raw event payloads are emptied after 90 days, finished
//     jobs deleted after 30 days, expired sessions deleted;
//   - health-watch: a daily run of the certificate, backup and disk checks
//     that alerts the owner on failure.
package system

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"mailroom/internal/config"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
	"mailroom/internal/settings"
)

// JobTypeBackup is the job type of an on-demand backup (POST /system/backups).
const JobTypeBackup = "backup"

// DoctorTimeout bounds a whole doctor run; checks still running then fail.
const DoctorTimeout = 15 * time.Second

// Deps are the Service's dependencies. Only DB, Clock and Config are needed
// for everything to work offline; each missing integration turns the checks
// that need it into a clear fail or skip instead of a crash.
type Deps struct {
	// DB is the open database. It may be nil only for Restore.
	DB     *db.DB
	Clock  core.Clock // default core.SystemClock
	Config config.Config
	// Settings supplies the time zone (backup schedule) and the sender and
	// owner addresses (config check), and holds the raw "ses_account" and
	// "sending_state" documents written by the sending package.
	Settings *settings.Service
	// Jobs runs on-demand backups; RegisterJobs registers the handler on it.
	Jobs *jobs.Runner
	// Alerter raises backup-failed and health-watch alerts. Nil: only logged.
	Alerter core.Alerter

	// AWS clients (nil when AWS isn't configured). NewAWSClients builds them.
	SES SESClient
	SNS SNSClient
	S3  S3Client

	// Resolver answers the DNS checks (NewDefaultResolver). Nil: skipped.
	Resolver Resolver
	// CertChecker inspects the served HTTPS certificate (NewTLSChecker).
	// Nil: the https_cert check is skipped.
	CertChecker CertChecker
	// HTTP makes the clock-skew request to the SES endpoint
	// (NewDefaultHTTPClient). Nil: the clock_skew check is skipped.
	HTTP HTTPDoer
	// DiskFree returns the free bytes of the file system holding a path.
	// Default: the platform implementation.
	DiskFree func(path string) (uint64, error)

	Version   string    // build version (default "dev")
	StartedAt time.Time // process start (default: Clock.Now() at New)
	Logger    *slog.Logger
}

// Service implements the system API, doctor, backups, restore, retention
// and the system workers.
type Service struct {
	db       *db.DB
	clock    core.Clock
	cfg      config.Config
	settings *settings.Service
	jobs     *jobs.Runner
	alerter  core.Alerter
	ses      SESClient
	sns      SNSClient
	s3       S3Client
	resolver Resolver
	certs    CertChecker
	http     HTTPDoer
	diskFree func(string) (uint64, error)
	version  string
	started  time.Time
	log      *slog.Logger

	// backupMu serialises backups taken by this process (worker, job, CLI)
	// and guards lastBackup, the last backup this process wrote.
	backupMu   sync.Mutex
	lastBackup BackupResult

	// Tunables, overridden by tests.
	doctorTimeout time.Duration
	sleep         func(ctx context.Context, d time.Duration) error
}

// New returns a system service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Version == "" {
		d.Version = "dev"
	}
	if d.StartedAt.IsZero() {
		d.StartedAt = d.Clock.Now()
	}
	if d.DiskFree == nil {
		d.DiskFree = diskFree
	}
	return &Service{
		db: d.DB, clock: d.Clock, cfg: d.Config, settings: d.Settings, jobs: d.Jobs, alerter: d.Alerter,
		ses: d.SES, sns: d.SNS, s3: d.S3,
		resolver: d.Resolver, certs: d.CertChecker, http: d.HTTP, diskFree: d.DiskFree,
		version: d.Version, started: d.StartedAt.UTC(), log: d.Logger,
		doctorTimeout: DoctorTimeout,
		sleep:         sleepCtx,
	}
}

// errNoDB is returned by operations that need the database when the
// Service was built without one.
var errNoDB = errors.New("system: no database configured")
