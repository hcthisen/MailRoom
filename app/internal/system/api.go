package system

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/jobs"
)

var tags = []string{"System"}

type emptyInput struct{}

type healthOutput struct{ Body Health }

type versionOutput struct{ Body VersionInfo }

type doctorOutput struct{ Body Report }

type sesInput struct {
	Refresh bool `query:"refresh" doc:"Read the status from SES now instead of the sender's last poll"`
}

type sesOutput struct{ Body SESStatus }

// BackupSchedule describes the nightly backup (O-3).
type BackupSchedule struct {
	NextRunAt     time.Time `json:"next_run_at" doc:"When the next nightly backup runs (UTC)"`
	LocalTime     string    `json:"local_time" doc:"Time of day of the nightly backup, in timezone" example:"02:30"`
	Timezone      string    `json:"timezone" doc:"The settings time zone" example:"Europe/London"`
	RetentionDays int       `json:"retention_days" doc:"Local backups older than this are deleted" example:"14"`
	S3Bucket      string    `json:"s3_bucket" doc:"Bucket each backup is uploaded to (BACKUP_BUCKET); empty when uploads are off" example:"mailroom-backup-example"`
}

// BackupList is the body of GET /system/backups.
type BackupList struct {
	Items      []BackupInfo   `json:"items" doc:"Local backups, newest first"`
	NextCursor string         `json:"next_cursor" doc:"Always empty: every local backup is listed on one page"`
	Schedule   BackupSchedule `json:"schedule"`
}

type backupListOutput struct{ Body BackupList }

type createBackupInput struct {
	api.DryRunParam
}

type createBackupOutput struct{ Body jobs.AcceptedBody }

type downloadInput struct {
	Name string `path:"name" maxLength:"128" pattern:"^[a-z][a-z0-9-]{0,62}-[0-9]{8}T[0-9]{6}Z\\.db$" doc:"Backup file name from GET /system/backups" example:"mailroom-20260105T023000Z.db"`
}

// Schedule returns the nightly backup schedule as of now.
func (s *Service) Schedule(ctx context.Context) BackupSchedule {
	loc := s.location(ctx)
	return BackupSchedule{
		NextRunAt:     NextBackupAt(s.clock.Now(), loc),
		LocalTime:     fmt.Sprintf("%02d:%02d", BackupHour, BackupMinute),
		Timezone:      loc.String(),
		RetentionDays: int(BackupRetention / (24 * time.Hour)),
		S3Bucket:      s.cfg.BackupBucket,
	}
}

// RequestBackup queues an on-demand backup job with its audit record, in
// one transaction. With dryRun the transaction is rolled back: the
// returned job shows what would be queued, does not exist and has ID 0
// (the rolled-back ID would be reused by the next real job).
func (s *Service) RequestBackup(ctx context.Context, dryRun bool) (jobs.Job, error) {
	if s.db == nil {
		return jobs.Job{}, errNoDB
	}
	if s.jobs == nil {
		return jobs.Job{}, fmt.Errorf("system: no job runner configured")
	}
	var job jobs.Job
	err := api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		j, err := s.jobs.Submit(ctx, tx, JobTypeBackup, nil)
		if err != nil {
			return err
		}
		job = j
		return audit.Write(ctx, tx, audit.Entry{
			Operation: "system.backup", TargetType: "job", TargetID: strconv.FormatInt(j.ID, 10),
			Changes: map[string]any{"job_type": JobTypeBackup}, At: s.clock.Now(),
		})
	})
	if dryRun && err == nil {
		job.ID = 0
	}
	return job, err
}

// RegisterAPI registers the System operations.
func (s *Service) RegisterAPI(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "get-system-health",
		Method:      http.MethodGet,
		Path:        "/system/health",
		Summary:     "Get system health",
		Description: "Returns whether the database is reachable, the applied and latest schema versions, the send queue depth (queued messages and how many are due), queued and running jobs, and the sender's state document (e.g. whether sending is paused). `status` is `fail` when the database is unreachable and `degraded` when migrations are pending. The unauthenticated liveness probe is `/healthz`.",
		Tags:        tags,
	}, func(ctx context.Context, _ *emptyInput) (*healthOutput, error) {
		return &healthOutput{Body: s.Health(ctx)}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-system-version",
		Method:      http.MethodGet,
		Path:        "/system/version",
		Summary:     "Get version",
		Description: "Returns the build version compiled into the binary, the Go version and platform, the newest schema version the binary knows, and the process start time and uptime.",
		Tags:        tags,
	}, func(ctx context.Context, _ *emptyInput) (*versionOutput, error) {
		return &versionOutput{Body: s.Version()}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "run-doctor",
		Method:      http.MethodGet,
		Path:        "/system/doctor",
		Summary:     "Run the doctor checks",
		Description: "Runs the install checks of `mailroom doctor` (L-4) and returns each one with a status (ok, warn, fail, skipped), a message and a fix hint: DNS (app A/AAAA record, DKIM CNAMEs, MAIL FROM MX and SPF, DMARC), SES identity verification, production access, sending enabled and quota, SNS subscription of /hooks/ses, HTTPS certificate, last backup under 26 h old, clock skew against AWS, free disk space and the sender configuration. `status` is the worst check. The checks run concurrently and take at most 15 seconds.",
		Tags:        tags,
	}, func(ctx context.Context, _ *emptyInput) (*doctorOutput, error) {
		return &doctorOutput{Body: s.Doctor(ctx)}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-ses-status",
		Method:      http.MethodGet,
		Path:        "/system/ses",
		Summary:     "Get SES account status and quota",
		Description: "Returns the SES account status: production access (sandbox or not), whether sending is enabled, the enforcement status, and the sending quota (daily limit, sent in the last 24 hours, maximum rate). It serves the sender's last poll (every 5 minutes) and reads SES live when that is missing or older than 15 minutes, or with `refresh=true`. Answers 409 when AWS isn't configured.",
		Tags:        tags,
		Errors:      []int{http.StatusConflict},
	}, func(ctx context.Context, in *sesInput) (*sesOutput, error) {
		st, err := s.SESStatus(ctx, in.Refresh)
		if err != nil {
			return nil, err
		}
		return &sesOutput{Body: st}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-backups",
		Method:      http.MethodGet,
		Path:        "/system/backups",
		Summary:     "List backups",
		Description: "Lists the local database backups (newest first) with their size and time, and the nightly schedule: the next run (02:30 in the settings time zone), how long local backups are kept (14 days) and the S3 bucket each backup is uploaded to, if any. Backups are consistent SQLite copies made with VACUUM INTO.",
		Tags:        tags,
	}, func(ctx context.Context, _ *emptyInput) (*backupListOutput, error) {
		list, err := s.ListBackups()
		if err != nil {
			return nil, err
		}
		return &backupListOutput{Body: BackupList{Items: list, Schedule: s.Schedule(ctx)}}, nil
	})

	api.Register(a, huma.Operation{
		OperationID:   "create-backup",
		Method:        http.MethodPost,
		Path:          "/system/backups",
		Summary:       "Take a backup",
		Description:   "Starts a backup job and answers 202 with the job; poll GET /api/v1/jobs/{id} until it has succeeded. The job writes a VACUUM INTO copy to the backups directory, deletes local backups older than 14 days and, when BACKUP_BUCKET is set, uploads the copy to S3 (server-side encrypted). A failed backup also alerts the owner. With `dry_run=true` nothing is queued: the returned job has `id` 0, does not exist and can't be polled.",
		Tags:          tags,
		DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *createBackupInput) (*createBackupOutput, error) {
		job, err := s.RequestBackup(ctx, in.DryRun)
		if err != nil {
			return nil, err
		}
		return &createBackupOutput{Body: jobs.AcceptedBody{Job: job}}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "download-backup",
		Method:      http.MethodGet,
		Path:        "/system/backups/{name}",
		Summary:     "Download a backup",
		Description: "Streams a backup file (a SQLite database) as an attachment. It contains every contact and the user password hashes, so it needs an admin token. Use a name from GET /system/backups; any other name is refused.",
		Tags:        tags,
		Metadata:    map[string]any{api.MetaScope: api.ScopeAdmin},
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "The backup file",
				Content: map[string]*huma.MediaType{
					backupContentType: {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}},
				},
			},
		},
	}, func(ctx context.Context, in *downloadInput) (*huma.StreamResponse, error) {
		f, info, err := s.OpenBackup(in.Name)
		if err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hc huma.Context) {
			defer f.Close()
			hc.SetHeader("Content-Type", backupContentType)
			hc.SetHeader("Content-Length", strconv.FormatInt(info.SizeBytes, 10))
			hc.SetHeader("Content-Disposition", `attachment; filename="`+info.Name+`"`)
			hc.SetHeader("Cache-Control", "no-store")
			hc.SetStatus(http.StatusOK)
			if _, err := io.Copy(hc.BodyWriter(), f); err != nil {
				s.log.Warn("system: backup download interrupted", "name", info.Name, "err", err)
			}
		}}, nil
	})
}
