package system

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/jobs"
	"mailroom/internal/server"
)

// Schedules of the system workers (ARCHITECTURE.md §8).
const (
	// BackupHour and BackupMinute: the nightly backup runs at 02:30 in the
	// settings time zone.
	BackupHour   = 2
	BackupMinute = 30
	// RetentionInterval and HealthWatchInterval: both run daily, the first
	// time a few minutes after start.
	RetentionInterval   = 24 * time.Hour
	HealthWatchInterval = 24 * time.Hour

	retentionFirstRun   = 5 * time.Minute
	healthWatchFirstRun = 15 * time.Minute
	// maxSleep caps each wait of the backup worker, so a time-zone change
	// in the settings takes effect within the hour.
	maxSleep = time.Hour
)

// backupRetryDelays are the waits before retrying a scheduled backup that
// wrote no local file, one per consecutive failure. A retry never delays
// the next 02:30 run; after the last one the worker waits for it.
var backupRetryDelays = []time.Duration{15 * time.Minute, time.Hour, 3 * time.Hour}

// Alert keys (core.Alerter de-duplicates per key).
const (
	AlertBackupFailed      = "backup_failed"
	AlertCertRenewalFailed = "cert_renewal_failed"
	AlertBackupStale       = "backup_stale"
	AlertDiskSpaceLow      = "disk_space_low"
)

// healthWatchChecks are the checks the daily health-watch runs, with the
// alert key raised when one fails.
var healthWatchChecks = []struct {
	check checkDef
	key   string
}{
	{checkDef{CheckHTTPSCert, (*doctorRun).checkHTTPSCert}, AlertCertRenewalFailed},
	{checkDef{CheckBackupRecent, (*doctorRun).checkBackupRecent}, AlertBackupStale},
	{checkDef{CheckDiskSpace, (*doctorRun).checkDiskSpace}, AlertDiskSpaceLow},
}

// NextBackupAt returns the first nightly backup time (02:30 in loc) after
// t, in UTC. On a day when 02:30 doesn't exist (a DST gap) Go's time.Date
// normalisation moves it into the following hour.
func NextBackupAt(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), BackupHour, BackupMinute, 0, 0, loc)
	for !next.After(t) {
		local = local.AddDate(0, 0, 1)
		next = time.Date(local.Year(), local.Month(), local.Day(), BackupHour, BackupMinute, 0, 0, loc)
	}
	return next.UTC()
}

// location is the settings time zone, or the configured one when the
// settings can't be read.
func (s *Service) location(ctx context.Context) *time.Location {
	if s.settings != nil {
		if st, err := s.settings.Get(ctx); err == nil {
			return st.Location()
		}
	}
	return s.cfg.Location()
}

// backupOverdue reports whether the newest scheduled backup is older than
// a day (or there is none), so the worker should catch up right away.
// Safety copies and backups dated in the future don't count (see
// latestScheduled): after a restore, the restored database gets its own
// backup at once.
func (s *Service) backupOverdue(now time.Time) bool {
	list, err := s.ListBackups()
	if err != nil {
		return true
	}
	last, found, _ := latestScheduled(list, now)
	return !found || now.Sub(last.CreatedAt) > 24*time.Hour
}

// runBackup takes a backup and alerts the owner when it fails (O-5). It is
// shared by the nightly worker and the "backup" job.
func (s *Service) runBackup(ctx context.Context) (BackupResult, error) {
	res, err := s.Backup(ctx)
	if errors.Is(err, core.ErrConflict) {
		// Another process wrote a backup this very second: there is a
		// fresh backup, so nothing to alert about.
		s.log.Warn("system: backup skipped", "err", err)
		return res, err
	}
	if err != nil {
		s.log.Error("system: backup failed", "err", err)
		title := "Backup failed"
		if res.Backup.Name != "" {
			title = "Backup upload to S3 failed"
		}
		s.alert(ctx, AlertBackupFailed, title,
			fmt.Sprintf("Mailroom could not complete a backup: %v\n\nCheck the disk space and the logs, then run `mailroom backup` or POST /api/v1/system/backups.", err))
	}
	return res, err
}

// alert raises an owner alert, or only logs it without an Alerter.
func (s *Service) alert(ctx context.Context, key, title, body string) {
	if s.alerter == nil {
		s.log.Warn("system: alert (no alerter configured)", "key", key, "title", title)
		return
	}
	if err := s.alerter.Alert(ctx, nil, key, title, body); err != nil {
		s.log.Error("system: raising alert failed", "key", key, "err", err)
	}
}

// HealthWatch runs the certificate, backup and disk checks once and alerts
// the owner about each one that fails. It returns the checks it ran.
func (s *Service) HealthWatch(ctx context.Context) []Check {
	defs := make([]checkDef, len(healthWatchChecks))
	for i, h := range healthWatchChecks {
		defs[i] = h.check
	}
	rep := s.runChecks(ctx, defs)
	for i, c := range rep.Checks {
		if c.Status != StatusFail {
			continue
		}
		body := c.Message
		if c.FixHint != "" {
			body += "\n\nHow to fix: " + c.FixHint
		}
		s.alert(ctx, healthWatchChecks[i].key, "Health check failed: "+strings.ReplaceAll(c.Name, "_", " "), body)
	}
	return rep.Checks
}

// RegisterJobs registers the "backup" job handler on Deps.Jobs.
func (s *Service) RegisterJobs() {
	if s.jobs == nil {
		panic("system: RegisterJobs needs Deps.Jobs")
	}
	s.jobs.Register(JobTypeBackup, func(ctx context.Context, _ jobs.JobContext) (any, error) {
		res, err := s.runBackup(ctx)
		if res.Backup.Name == "" {
			return nil, err
		}
		return res, err
	})
}

// Workers returns the system's background workers: "backup" (nightly at
// 02:30 in the settings time zone, catching up at start when the newest
// backup is over a day old), "retention" (daily) and "health-watch"
// (daily).
func (s *Service) Workers() []server.Worker {
	return []server.Worker{
		&worker{name: "backup", run: s.runBackupSchedule},
		&worker{name: "retention", run: func(ctx context.Context) error {
			return s.every(ctx, retentionFirstRun, RetentionInterval, func(ctx context.Context) {
				res, err := s.Retention(ctx)
				if err != nil {
					s.log.Error("system: retention failed", "err", err)
					return
				}
				s.log.Info("system: retention done", "event_details_cleared", res.EventDetailsCleared,
					"jobs_deleted", res.JobsDeleted, "sessions_deleted", res.SessionsDeleted,
					"confirmations_purged", res.ConfirmationsPurged)
			})
		}},
		&worker{name: "health-watch", run: func(ctx context.Context) error {
			return s.every(ctx, healthWatchFirstRun, HealthWatchInterval, func(ctx context.Context) { s.HealthWatch(ctx) })
		}},
	}
}

// worker adapts a run function to server.Worker.
type worker struct {
	name string
	run  func(ctx context.Context) error
}

func (w *worker) Name() string                  { return w.name }
func (w *worker) Run(ctx context.Context) error { return w.run(ctx) }

// runBackupSchedule is the backup worker's loop: nightly at 02:30, a
// catch-up at start when the newest backup is over a day old, and retries
// (backupRetryDelays) when a run wrote no local backup. A run whose only
// failure is the S3 upload isn't retried: the local backup exists. It
// returns nil when ctx is cancelled.
func (s *Service) runBackupSchedule(ctx context.Context) error {
	anchor := s.clock.Now()
	catchUp := s.backupOverdue(anchor)
	var retryAt time.Time // zero: no retry pending
	failures := 0         // consecutive runs that wrote no local backup
	for {
		now := s.clock.Now()
		next := NextBackupAt(anchor, s.location(ctx))
		due, retry := next, false
		if !retryAt.IsZero() && retryAt.Before(next) {
			due, retry = retryAt, true
		}
		if catchUp || !now.Before(due) {
			if !retry {
				failures = 0 // a scheduled run starts a new series of retries
			}
			res, err := s.runBackup(ctx)
			anchor, catchUp, retryAt = s.clock.Now(), false, time.Time{}
			if err != nil && res.Backup.Name == "" && !errors.Is(err, core.ErrConflict) && ctx.Err() == nil && failures < len(backupRetryDelays) {
				retryAt = anchor.Add(backupRetryDelays[failures])
				failures++
				s.log.Info("system: backup will be retried", "at", retryAt)
			}
			continue
		}
		if err := s.sleep(ctx, min(due.Sub(now), maxSleep)); err != nil {
			return nil
		}
	}
}

// every runs fn after first and then every interval until ctx is cancelled.
func (s *Service) every(ctx context.Context, first, interval time.Duration, fn func(ctx context.Context)) error {
	wait := first
	for {
		if err := s.sleep(ctx, wait); err != nil {
			return nil
		}
		fn(ctx)
		wait = interval
	}
}

// sleepCtx waits for d or until ctx is done (then it returns ctx.Err()).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
