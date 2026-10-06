package imports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"mailroom/internal/db"
	"mailroom/internal/jobs"
)

// jobParams are the parameters of an import.contacts job. The CSV itself
// stays in the file; params never hold its contents.
type jobParams struct {
	File       string `json:"file" doc:"Stored file name under <data>/imports"`
	OnExisting string `json:"on_existing"`
	Filename   string `json:"filename,omitempty" doc:"The uploaded file's name"`
	SizeBytes  int64  `json:"size_bytes"`
	// ParsedAt is the reference time of every parse of the file (it bounds
	// consent dates), fixed when the job is submitted: a resumed run must
	// read the file exactly like the first run, or the checkpoint's record
	// offset would point at a different record.
	ParsedAt time.Time `json:"parsed_at"`
}

// storedName matches the random names uploads are stored under.
var storedName = regexp.MustCompile(`^[0-9a-f]{32}\.csv$`)

// checkpoint is the progress of a running import, stored as the job's
// result in the same transaction as each batch, so a resumed job continues
// after the last committed batch instead of starting over.
type checkpoint struct {
	Offset int        `json:"offset" doc:"Records written so far"`
	Stats  applyStats `json:"stats"`
}

// jobResultProbe tells a checkpoint from a final report in a job result.
type jobResultProbe struct {
	Checkpoint *checkpoint `json:"checkpoint"`
	Rows       *int        `json:"rows"`
}

// orphanAge is how old an upload without a queued or running job must be
// before it is removed (e.g. after a crash between upload and commit).
const orphanAge = 24 * time.Hour

// RegisterJobs registers the import.contacts job handler. It is Resumable:
// creates, updates and tag adds are idempotent, and the job continues
// after its last committed batch.
func (s *Service) RegisterJobs(r *jobs.Runner) {
	r.Register(JobType, s.runImport, jobs.Resumable())
}

// runImport parses the stored file (as of the job's ParsedAt) and writes
// it in batches of BatchSize records, each in its own transaction with the job's checkpoint. The
// file is deleted when the job finishes; it is kept when a shutdown
// interrupts the job, because the job then runs again at the next start.
func (s *Service) runImport(ctx context.Context, jc jobs.JobContext) (result any, err error) {
	var p jobParams
	if err := jc.Params(&p); err != nil {
		return nil, err
	}
	if !storedName.MatchString(p.File) {
		return nil, fmt.Errorf("imports: invalid stored file name %q", p.File)
	}
	onExisting, err := validOnExisting(p.OnExisting)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, p.File)
	defer func() {
		if err != nil && ctx.Err() != nil {
			return // interrupted: the job is queued again and needs the file
		}
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			s.log.WarnContext(ctx, "imports: remove upload", "job", jc.ID(), "err", rmErr)
		}
	}()
	s.removeOrphans(ctx)

	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("the uploaded file is no longer there; upload it again")
	}
	if err != nil {
		return nil, fmt.Errorf("imports: open upload: %w", err)
	}
	now := p.ParsedAt
	if now.IsZero() {
		now = s.clock.Now()
	}
	pf, err := parseFile(f, now)
	f.Close()
	if err != nil {
		return nil, err
	}

	var st applyStats
	offset := 0
	if jc.Resumed() {
		cp, err := s.loadCheckpoint(ctx, jc.ID())
		if err != nil {
			return nil, err
		}
		if cp != nil && cp.Offset <= len(pf.records) {
			st, offset = cp.Stats, cp.Offset
		}
	}
	total := len(pf.records)
	for offset < total {
		if err := ctx.Err(); err != nil {
			return buildReport(pf, st, onExisting, false), err
		}
		end := min(offset+BatchSize, total)
		if err := s.applyBatch(ctx, jc.ID(), pf.records[offset:end], onExisting, &st, end); err != nil {
			return buildReport(pf, st, onExisting, false), err
		}
		offset = end
		jc.SetProgress(float64(offset) / float64(total))
	}
	return buildReport(pf, st, onExisting, false), nil
}

// saveCheckpoint stores cp as the running job's result inside tx.
func saveCheckpoint(ctx context.Context, tx *sql.Tx, jobID int64, cp checkpoint) error {
	b, err := db.MarshalJSON(jobResultProbe{Checkpoint: &cp})
	if err != nil {
		return fmt.Errorf("imports: encode checkpoint: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET result = ? WHERE id = ? AND status = 'running'`, b, jobID); err != nil {
		return fmt.Errorf("imports: save checkpoint: %w", err)
	}
	return nil
}

// loadCheckpoint returns the checkpoint a previous run of the job stored,
// or nil.
func (s *Service) loadCheckpoint(ctx context.Context, jobID int64) (*checkpoint, error) {
	var raw string
	if err := s.db.Read.QueryRowContext(ctx, `SELECT result FROM jobs WHERE id = ?`, jobID).Scan(&raw); err != nil {
		return nil, fmt.Errorf("imports: read checkpoint: %w", err)
	}
	var probe jobResultProbe
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, nil // not ours; start over
	}
	return probe.Checkpoint, nil
}

// removeOrphans deletes uploads older than orphanAge that no queued or
// running import refers to. Failures are only logged.
func (s *Service) removeOrphans(ctx context.Context) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	rows, err := s.db.Read.QueryContext(ctx, `SELECT coalesce(json_extract(params, '$.file'), '') FROM jobs
		WHERE type = ? AND status IN ('queued', 'running')`, JobType)
	if err != nil {
		s.log.WarnContext(ctx, "imports: list active imports", "err", err)
		return
	}
	inUse := map[string]bool{}
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			inUse[name] = true
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	cutoff := s.clock.Now().Add(-orphanAge)
	for _, e := range entries {
		if !storedName.MatchString(e.Name()) || inUse[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.WarnContext(ctx, "imports: remove orphaned upload", "file", e.Name(), "err", err)
		}
	}
}
