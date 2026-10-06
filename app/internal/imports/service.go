// Package imports is Mailroom's contacts CSV import (PRD §6.15 I-1, I-2):
// POST /api/v1/imports takes a CSV upload, stores it under
// <data>/imports and hands it to a resumable "import.contacts" job
// (G-7), or, with ?dry_run=true, evaluates the whole file at once and
// returns the report without writing anything (G-6).
//
// Every import follows the never-reactivate rule: a status only ever moves
// away from active, an erased or manually suppressed address is rejected,
// and a contact that is unsubscribed, bounced or complained stays that way
// whatever the CSV says. The file format is documented in
// docs/import-format.md.
package imports

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"

	"mailroom/internal/audit"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
)

// JobType is the job type of a contacts import.
const JobType = "import.contacts"

// What an import does with contacts that already exist (on_existing).
const (
	// OnExistingUpdate fills in names and attributes, adds tags and applies
	// statuses that block sending (the default).
	OnExistingUpdate = "update"
	// OnExistingSkip leaves existing contacts as they are, except that a
	// status which blocks sending is still applied.
	OnExistingSkip = "skip"
)

// Limits.
const (
	// MaxFileBytes bounds one CSV file (50 MiB).
	MaxFileBytes = 50 << 20
	// MaxColumns bounds the columns of the header row.
	MaxColumns = 500
	// MaxRejectedListed is the number of rejected rows listed in a report;
	// rejected_count has the total.
	MaxRejectedListed = 1000
	// MaxConsentSourceLength bounds consent_source.
	MaxConsentSourceLength = 200
	// BatchSize is the number of contacts written per transaction.
	BatchSize = 500
)

// Deps are the Service's dependencies.
type Deps struct {
	DB    *db.DB
	Store *contactstore.Store
	// Jobs runs imports. Submit is called inside the operation's tx.
	Jobs    *jobs.Runner
	Clock   core.Clock   // default core.SystemClock
	Logger  *slog.Logger // default slog.Default()
	DataDir string       // cfg.DataDir; uploads are kept in DataDir/imports
}

// Service implements the imports operations. API handlers and the admin UI
// call its methods.
type Service struct {
	db    *db.DB
	store *contactstore.Store
	jobs  *jobs.Runner
	clock core.Clock
	log   *slog.Logger
	dir   string
}

// New returns an imports service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{
		db: d.DB, store: d.Store, jobs: d.Jobs, clock: d.Clock, log: d.Logger,
		dir: filepath.Join(d.DataDir, "imports"),
	}
}

// Dir returns the directory holding uploaded files while their import runs.
func (s *Service) Dir() string { return s.dir }

// validOnExisting normalises on_existing ("" = update).
func validOnExisting(v string) (string, error) {
	switch v {
	case "", OnExistingUpdate:
		return OnExistingUpdate, nil
	case OnExistingSkip:
		return OnExistingSkip, nil
	}
	return "", core.Invalid("on_existing", "must be update or skip")
}

// logAudit writes an audit record for the actor in ctx inside tx.
func (s *Service) logAudit(ctx context.Context, tx *sql.Tx, op, targetType, targetID string, changes any) error {
	return audit.Write(ctx, tx, audit.Entry{
		Operation: op, TargetType: targetType, TargetID: targetID, Changes: changes, At: s.clock.Now(),
	})
}
