package imports

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/jobs"
)

// ImportJob is an import and, once it has finished, its report.
type ImportJob struct {
	JobID      int64         `json:"job_id" doc:"The import's job ID (also readable with get-job)" example:"17"`
	Status     string        `json:"status" enum:"queued,running,succeeded,failed" example:"running"`
	Progress   float64       `json:"progress" minimum:"0" maximum:"1" doc:"Fraction of contacts written, 0 to 1" example:"0.4"`
	Error      string        `json:"error" doc:"Why the import failed; empty unless status is failed" example:""`
	OnExisting string        `json:"on_existing" enum:"update,skip"`
	Filename   string        `json:"filename" doc:"The uploaded file's name, when known" example:"contacts.csv"`
	SizeBytes  int64         `json:"size_bytes" example:"48213"`
	CreatedBy  string        `json:"created_by" example:"token:3"`
	CreatedAt  time.Time     `json:"created_at"`
	StartedAt  *time.Time    `json:"started_at,omitempty"`
	FinishedAt *time.Time    `json:"finished_at,omitempty"`
	Report     *ImportReport `json:"report,omitempty" doc:"The report once the import has finished (for a failed import, what was done before it stopped). Omitted in lists."`
}

// Get returns the import with job ID id. Jobs of other types wrap
// core.ErrNotFound.
func (s *Service) Get(ctx context.Context, id int64) (ImportJob, error) {
	j, err := s.jobs.Get(ctx, id)
	if err != nil {
		return ImportJob{}, err
	}
	if j.Type != JobType {
		return ImportJob{}, fmt.Errorf("import %d: %w", id, core.ErrNotFound)
	}
	return toImportJob(j, true), nil
}

// List returns imports newest first, without reports. beforeID > 0 pages.
func (s *Service) List(ctx context.Context, beforeID int64, limit int) ([]ImportJob, error) {
	js, err := s.jobs.List(ctx, jobs.ListFilter{Type: JobType, BeforeID: beforeID, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]ImportJob, len(js))
	for i, j := range js {
		out[i] = toImportJob(j, false)
	}
	return out, nil
}

// toImportJob converts a job. withReport adds the report of a finished job.
func toImportJob(j jobs.Job, withReport bool) ImportJob {
	var p jobParams
	_ = json.Unmarshal(j.Params, &p)
	out := ImportJob{
		JobID: j.ID, Status: j.Status, Progress: j.Progress, Error: j.Error,
		OnExisting: p.OnExisting, Filename: p.Filename, SizeBytes: p.SizeBytes,
		CreatedBy: j.CreatedBy, CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
	if withReport && j.Done() {
		var probe jobResultProbe
		if json.Unmarshal(j.Result, &probe) == nil && probe.Rows != nil {
			var rep ImportReport
			if json.Unmarshal(j.Result, &rep) == nil {
				out.Report = &rep
			}
		}
	}
	return out
}
