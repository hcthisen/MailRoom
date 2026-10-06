package broadcasts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"mailroom/internal/core"
	"mailroom/internal/jobs"
)

// MaxSnapshotAttempts is how many snapshot jobs of one start may fail in a
// row before the starter stops re-submitting: it then pauses the broadcast
// (reason snapshot_failed) and alerts the owner. Resuming it tries again.
const MaxSnapshotAttempts = 5

// snapshotParams are the parameters of a broadcast.snapshot job.
type snapshotParams struct {
	BroadcastID int64 `json:"broadcast_id"`
	// MaxContactID bounds the snapshot to contacts that existed when the
	// broadcast started. Re-submitted jobs carry the original bound.
	MaxContactID int64 `json:"max_contact_id"`
	// Attempt numbers the jobs submitted in a row for one start: 1 for the
	// first job and after a resume, +1 for every re-submission by the
	// starter.
	Attempt int `json:"attempt"`
}

// start sets the broadcast sending and submits its snapshot job in tx. It
// fixes the snapshot's contact bound for the whole run.
func (s *Service) start(ctx context.Context, tx *sql.Tx, id int64) (jobs.Job, error) {
	var maxID int64
	if err := tx.QueryRowContext(ctx, `SELECT IFNULL(max(id), 0) FROM contacts`).Scan(&maxID); err != nil {
		return jobs.Job{}, fmt.Errorf("broadcasts: snapshot bound: %w", err)
	}
	job, err := s.submitSnapshot(ctx, tx, snapshotParams{BroadcastID: id, MaxContactID: maxID, Attempt: 1})
	if err != nil {
		return jobs.Job{}, err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE broadcasts SET status = 'sending', started_at = ?, paused_reason = '', snapshot_job_id = ?,
		       snapshot_done_at = NULL, recipients_total = 0, version = version + 1, updated_at = ?
		WHERE id = ?`, now, job.ID, now, id); err != nil {
		return jobs.Job{}, fmt.Errorf("broadcasts: start %d: %w", id, err)
	}
	return job, nil
}

// submitSnapshot submits a snapshot job with p in tx and records it as the
// broadcast's snapshot_job_id.
func (s *Service) submitSnapshot(ctx context.Context, tx *sql.Tx, p snapshotParams) (jobs.Job, error) {
	job, err := s.jobs.Submit(ctx, tx, JobSnapshot, p)
	if err != nil {
		return jobs.Job{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE broadcasts SET snapshot_job_id = ? WHERE id = ?`, job.ID, p.BroadcastID); err != nil {
		return jobs.Job{}, fmt.Errorf("broadcasts: record snapshot job: %w", err)
	}
	return job, nil
}

// nextSnapshotParams returns the parameters of a job re-submitted after
// prev (nil: the job is gone): the same contact bound and the next attempt
// number. Without the previous job's parameters the bound is the highest
// contact created up to the broadcast's start.
func (s *Service) nextSnapshotParams(ctx context.Context, tx *sql.Tx, id int64, prev *jobs.Job) (snapshotParams, error) {
	if prev != nil {
		var p snapshotParams
		if err := json.Unmarshal(prev.Params, &p); err == nil && p.BroadcastID == id {
			p.Attempt = max(p.Attempt, 1) + 1
			return p, nil
		}
	}
	p := snapshotParams{BroadcastID: id, Attempt: 1}
	if err := tx.QueryRowContext(ctx, `
		SELECT IFNULL(max(c.id), 0) FROM contacts c, broadcasts b
		WHERE b.id = ? AND c.created_at <= IFNULL(b.started_at, '')`, id).Scan(&p.MaxContactID); err != nil {
		return snapshotParams{}, fmt.Errorf("broadcasts: snapshot bound: %w", err)
	}
	return p, nil
}

// snapshotJob returns the snapshot job jobID, nil when there is none (not
// recorded, or deleted).
func (s *Service) snapshotJob(ctx context.Context, jobID *int64) (*jobs.Job, error) {
	if jobID == nil {
		return nil, nil
	}
	j, err := s.jobs.Get(ctx, *jobID)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// snapshotStalled reports whether a broadcast whose snapshot isn't done
// needs a new snapshot job: its job is missing or finished (failed, or
// stopped early).
func snapshotStalled(j *jobs.Job) bool {
	return j == nil || j.Done()
}
