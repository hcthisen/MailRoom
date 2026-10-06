package system

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"mailroom/internal/jobs"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestNextBackupAt(t *testing.T) {
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		tz, now, want string
	}{
		{"UTC", "2026-01-05T01:00:00Z", "2026-01-05T02:30:00Z"},
		{"UTC", "2026-01-05T02:30:00Z", "2026-01-06T02:30:00Z"}, // exactly at: the next day
		{"UTC", "2026-01-05T09:00:00Z", "2026-01-06T02:30:00Z"},
		{"Europe/Copenhagen", "2026-01-05T09:00:00Z", "2026-01-06T01:30:00Z"}, // CET, UTC+1
		{"Europe/Copenhagen", "2026-07-01T09:00:00Z", "2026-07-02T00:30:00Z"}, // CEST, UTC+2
		{"Europe/Copenhagen", "2026-01-05T01:00:00Z", "2026-01-05T01:30:00Z"}, // 02:00 local: today
		{"America/New_York", "2026-01-05T09:00:00Z", "2026-01-06T07:30:00Z"},
		{"America/New_York", "2026-01-06T03:00:00Z", "2026-01-06T07:30:00Z"}, // still Jan 5 locally
		{"Pacific/Auckland", "2026-01-05T09:00:00Z", "2026-01-05T13:30:00Z"}, // NZDT, UTC+13
		// 2026-03-29 02:30 doesn't exist in Copenhagen (02:00 -> 03:00):
		// normalised into the next hour, still after now.
		{"Europe/Copenhagen", "2026-03-28T23:00:00Z", "2026-03-29T01:30:00Z"},
		// Fall back: 02:30 exists twice on 2026-10-25 (00:30 and 01:30
		// UTC); time.Date picks one of them.
		{"Europe/Copenhagen", "2026-10-24T23:00:00Z", "2026-10-25T00:30:00Z|2026-10-25T01:30:00Z"},
		{"Europe/Copenhagen", "2026-10-25T01:30:00Z", "2026-10-26T01:30:00Z"}, // one backup that night
	}
	for _, tc := range cases {
		got := NextBackupAt(utc(tc.now), mustLoc(t, tc.tz))
		match := false
		for _, want := range strings.Split(tc.want, "|") {
			match = match || got.Equal(utc(want))
		}
		if !match || got.Location() != time.UTC {
			t.Errorf("NextBackupAt(%s, %s) = %s, want %s", tc.now, tc.tz, got, tc.want)
		}
		if !got.After(utc(tc.now)) {
			t.Errorf("NextBackupAt(%s, %s) = %s is not after now", tc.now, tc.tz, got)
		}
	}
}

// fakeSleep makes s.sleep advance the fake clock instead of waiting, and
// cancels the run once stop reports true. It records every wait.
func fakeSleep(f *fixture, s *Service, cancel context.CancelFunc, stop func() bool) *[]time.Duration {
	var waits []time.Duration
	s.sleep = func(ctx context.Context, d time.Duration) error {
		if stop() {
			cancel()
			return context.Canceled
		}
		waits = append(waits, d)
		f.clock.Advance(d)
		return ctx.Err()
	}
	return &waits
}

// nightly returns the names of the "mailroom-" backups, oldest first.
func nightly(t *testing.T, s *Service) []string {
	t.Helper()
	list, err := s.ListBackups()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range list {
		if strings.HasPrefix(b.Name, BackupPrefix) {
			out = append(out, b.Name)
		}
	}
	slices.Sort(out)
	return out
}

func setTimezone(t *testing.T, f *fixture, tz string) {
	t.Helper()
	if _, err := f.settings.Update(f.ctx, settings.Patch{Timezone: &tz}, 0); err != nil {
		t.Fatal(err)
	}
}

func TestBackupScheduleUsesSettingsTimezone(t *testing.T) {
	f := newFixture(t)
	setTimezone(t, f, "Europe/Copenhagen")
	existing := f.writeBackupFile(BackupPrefix, f.clock.Now().Add(-time.Hour)) // fresh: no catch-up
	s := f.service()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	switched := false
	waits := fakeSleep(f, s, cancel, func() bool {
		names := nightly(t, s)
		if len(names) == 2 && !switched {
			// The time zone changes after the first nightly backup.
			setTimezone(t, f, "America/New_York")
			switched = true
		}
		return len(names) >= 3
	})
	if err := s.runBackupSchedule(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mailroom-20260105T080000Z.db", // existing
		"mailroom-20260106T013000Z.db", // 02:30 Copenhagen
		"mailroom-20260106T073000Z.db", // then 02:30 New York
	}
	if got := nightly(t, s); !slices.Equal(got, want) || got[0] != existing {
		t.Fatalf("backups = %v, want %v", got, want)
	}
	for _, w := range *waits {
		if w <= 0 || w > maxSleep {
			t.Fatalf("wait %s outside (0, %s]", w, maxSleep)
		}
	}
	if len(f.alerter.alerts) != 0 {
		t.Fatalf("alerts = %+v", f.alerter.alerts)
	}
}

func TestBackupScheduleCatchesUp(t *testing.T) {
	cases := []struct {
		name     string
		existing time.Duration // age of the newest backup; 0 = none
		want     string        // first backup taken
	}{
		{"no backup yet", 0, "mailroom-20260105T090000Z.db"},
		{"backup over a day old", 25 * time.Hour, "mailroom-20260105T090000Z.db"},
		{"fresh backup", 23 * time.Hour, "mailroom-20260106T023000Z.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var existing []string
			if tc.existing > 0 {
				existing = append(existing, f.writeBackupFile(BackupPrefix, f.clock.Now().Add(-tc.existing)))
			}
			s := f.service()
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			fakeSleep(f, s, cancel, func() bool { return len(nightly(t, s)) > len(existing) })
			if err := s.runBackupSchedule(ctx); err != nil {
				t.Fatal(err)
			}
			got := nightly(t, s)
			if len(got) != len(existing)+1 || !slices.Contains(got, tc.want) {
				t.Fatalf("backups = %v, want %s added", got, tc.want)
			}
		})
	}
}

func TestBackupScheduleAlertsAndContinues(t *testing.T) {
	f := newFixture(t)
	f.cfg.BackupBucket = "b"
	f.s3.err = errBoom
	s := f.service()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	fakeSleep(f, s, cancel, func() bool { return len(nightly(t, s)) >= 2 })
	if err := s.runBackupSchedule(ctx); err != nil {
		t.Fatal(err)
	}
	if keys := f.alerter.keys(); len(keys) != 2 || keys[0] != AlertBackupFailed {
		t.Fatalf("alerts = %v", keys)
	}
}

// blockBackups puts a file where the backups directory should be, so every
// backup fails before writing anything; it returns a func that undoes it.
func blockBackups(t *testing.T, f *fixture) func() {
	t.Helper()
	if err := os.WriteFile(f.cfg.BackupsDir(), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(f.cfg.BackupsDir()); err != nil {
			t.Fatal(err)
		}
	}
}

func rfc3339(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBackupScheduleRetriesFailedBackup(t *testing.T) {
	f := newFixture(t)
	unblock := blockBackups(t, f)
	s := f.service()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	fixed := false
	waits := fakeSleep(f, s, cancel, func() bool {
		if len(f.alerter.keys()) == 1 && !fixed {
			unblock() // the disk problem goes away after the first attempt
			fixed = true
		}
		return len(nightly(t, s)) >= 1
	})
	if err := s.runBackupSchedule(ctx); err != nil {
		t.Fatal(err)
	}
	// Catch-up at 09:00 fails; the retry 15 minutes later succeeds.
	if got := nightly(t, s); !slices.Equal(got, []string{"mailroom-20260105T091500Z.db"}) {
		t.Fatalf("backups = %v, waits = %v", got, *waits)
	}
	if keys := f.alerter.keys(); !slices.Equal(keys, []string{AlertBackupFailed}) {
		t.Fatalf("alerts = %v", keys)
	}
}

func TestBackupScheduleRetriesThenWaitsForNightly(t *testing.T) {
	f := newFixture(t)
	blockBackups(t, f)
	s := f.service()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	var attempts []time.Time
	fakeSleep(f, s, cancel, func() bool {
		if n := len(f.alerter.keys()); n > len(attempts) {
			attempts = append(attempts, f.clock.Now())
		}
		return len(attempts) >= 5
	})
	if err := s.runBackupSchedule(ctx); err != nil {
		t.Fatal(err)
	}
	// Catch-up, three retries (15m, 1h, 3h), then the nightly run.
	want := []time.Time{rfc3339(t, "2026-01-05T09:00:00Z"), rfc3339(t, "2026-01-05T09:15:00Z"), rfc3339(t, "2026-01-05T10:15:00Z"),
		rfc3339(t, "2026-01-05T13:15:00Z"), rfc3339(t, "2026-01-06T02:30:00Z")}
	if !slices.EqualFunc(attempts, want, time.Time.Equal) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
}

func TestBackupScheduleRetryNeverDelaysNightly(t *testing.T) {
	f := newFixture(t)
	f.clock.Set(rfc3339(t, "2026-01-05T23:00:00Z"))
	unblock := blockBackups(t, f)
	s := f.service()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	var attempts []time.Time
	unblocked := false
	fakeSleep(f, s, cancel, func() bool {
		if n := len(f.alerter.keys()); n > len(attempts) {
			attempts = append(attempts, f.clock.Now())
			if n == 3 {
				unblock()
				unblocked = true
			}
		}
		return unblocked && len(nightly(t, s)) >= 1
	})
	if err := s.runBackupSchedule(ctx); err != nil {
		t.Fatal(err)
	}
	// 23:00, 23:15 and 00:15 fail; the 3h retry (03:15) would come after
	// 02:30, so the nightly run at 02:30 is next.
	if got := nightly(t, s); !slices.Equal(got, []string{"mailroom-20260106T023000Z.db"}) {
		t.Fatalf("backups = %v, attempts = %v", got, attempts)
	}
	want := []time.Time{rfc3339(t, "2026-01-05T23:00:00Z"), rfc3339(t, "2026-01-05T23:15:00Z"), rfc3339(t, "2026-01-06T00:15:00Z")}
	if !slices.EqualFunc(attempts, want, time.Time.Equal) {
		t.Fatalf("failed attempts = %v, want %v", attempts, want)
	}
}

func TestBackupScheduleUploadFailureIsNotRetried(t *testing.T) {
	f := newFixture(t)
	f.cfg.BackupBucket = "b"
	f.s3.err = errBoom
	s := f.service()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	fakeSleep(f, s, cancel, func() bool { return len(nightly(t, s)) >= 2 })
	if err := s.runBackupSchedule(ctx); err != nil {
		t.Fatal(err)
	}
	// The local backup exists, so the next attempt is the nightly run.
	if got := nightly(t, s); !slices.Equal(got, []string{"mailroom-20260105T090000Z.db", "mailroom-20260106T023000Z.db"}) {
		t.Fatalf("backups = %v", got)
	}
}

func TestBackupOverdueIgnoresSafetyAndFutureCopies(t *testing.T) {
	cases := []struct {
		name   string
		files  func(f *fixture)
		expect bool
	}{
		{"none", func(*fixture) {}, true},
		{"fresh nightly", func(f *fixture) { f.writeBackupFile(BackupPrefix, f.clock.Now().Add(-time.Hour)) }, false},
		{"old nightly", func(f *fixture) { f.writeBackupFile(BackupPrefix, f.clock.Now().Add(-25*time.Hour)) }, true},
		{"just restored", func(f *fixture) {
			f.writeBackupFile(BackupPrefix, f.clock.Now().Add(-25*time.Hour))
			f.writeBackupFile(PreRestorePrefix, f.clock.Now())
		}, true},
		{"future-dated nightly", func(f *fixture) { f.writeBackupFile(BackupPrefix, f.clock.Now().Add(3*day)) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.files(f)
			if got := f.service().backupOverdue(f.clock.Now()); got != tc.expect {
				t.Fatalf("overdue = %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestWorkers(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	var names []string
	for _, w := range s.Workers() {
		names = append(names, w.Name())
	}
	if !slices.Equal(names, []string{"backup", "retention", "health-watch"}) {
		t.Fatalf("workers = %v", names)
	}

	t.Run("retention runs after 5 minutes, then daily", func(t *testing.T) {
		f := newFixture(t)
		id := insertEvent(t, f, f.clock.Now().Add(-100*day), `{"raw":1}`)
		s := f.service()
		ctx, cancel := context.WithCancel(f.ctx)
		defer cancel()
		calls := 0
		waits := fakeSleep(f, s, cancel, func() bool { calls++; return calls > 2 })
		if err := s.Workers()[1].Run(ctx); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(*waits, []time.Duration{5 * time.Minute, 24 * time.Hour}) {
			t.Fatalf("waits = %v", *waits)
		}
		if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM events WHERE id = ? AND detail = '{}'`, id); n != 1 {
			t.Fatal("retention didn't run")
		}
	})

	t.Run("health-watch runs after 15 minutes, then daily", func(t *testing.T) {
		f := newFixture(t)
		f.writeBackupFile(BackupPrefix, f.clock.Now())
		f.free = 1 << 20
		s := f.service()
		ctx, cancel := context.WithCancel(f.ctx)
		defer cancel()
		calls := 0
		waits := fakeSleep(f, s, cancel, func() bool { calls++; return calls > 2 })
		if err := s.Workers()[2].Run(ctx); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(*waits, []time.Duration{15 * time.Minute, 24 * time.Hour}) {
			t.Fatalf("waits = %v", *waits)
		}
		if keys := f.alerter.keys(); !slices.Equal(keys, []string{AlertDiskSpaceLow, AlertDiskSpaceLow}) {
			t.Fatalf("alerts = %v", keys)
		}
	})

	t.Run("cancelled workers return nil", func(t *testing.T) {
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		for _, w := range s.Workers() {
			if err := w.Run(ctx); err != nil {
				t.Errorf("%s: %v", w.Name(), err)
			}
		}
	})
}

func TestHealthWatch(t *testing.T) {
	f := newFixture(t)
	f.certExp = f.clock.Now().Add(2 * day)
	f.free = 100 << 20
	f.writeBackupFile(BackupPrefix, f.clock.Now().Add(-time.Hour))
	checks := f.service().HealthWatch(f.ctx)
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{CheckHTTPSCert, CheckBackupRecent, CheckDiskSpace}) {
		t.Fatalf("checks = %v", names)
	}
	keys := f.alerter.keys()
	if !slices.Equal(keys, []string{AlertCertRenewalFailed, AlertDiskSpaceLow}) {
		t.Fatalf("alerts = %v", keys)
	}
	a := f.alerter.alerts[0]
	if a.title != "Health check failed: https cert" || !strings.Contains(a.body, "How to fix:") {
		t.Fatalf("alert = %+v", a)
	}

	// Stale backup alerts too; warnings don't.
	f2 := newFixture(t)
	f2.writeBackupFile(BackupPrefix, f2.clock.Now().Add(-30*time.Hour))
	f2.certExp = f2.clock.Now().Add(10 * day) // warn only
	f2.service().HealthWatch(f2.ctx)
	if keys := f2.alerter.keys(); !slices.Equal(keys, []string{AlertBackupStale}) {
		t.Fatalf("alerts = %v", keys)
	}
}

// startRunner runs the job runner until the test ends.
func startRunner(t *testing.T, r *jobs.Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestBackupJob(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	s.RegisterJobs()
	startRunner(t, f.runner)

	// Dry run: nothing is queued.
	dry, err := s.RequestBackup(f.ctx, true)
	if err != nil || dry.ID != 0 || dry.Type != JobTypeBackup || dry.Status != jobs.StatusQueued {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("dry run left %d jobs", n)
	}

	job, err := s.RequestBackup(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'system.backup' AND target_type = 'job' AND target_id = ?`, job.ID); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	done, err := f.runner.Wait(ctx, job.ID)
	if err != nil || done.Status != jobs.StatusSucceeded {
		t.Fatalf("job = %+v, %v", done, err)
	}
	var res BackupResult
	if err := json.Unmarshal(done.Result, &res); err != nil || res.Backup.Name != "mailroom-20260105T090000Z.db" {
		t.Fatalf("result = %s, %v", done.Result, err)
	}

	// A failing job reports the partial result and alerts.
	f.cfg.BackupBucket = "b"
	f.s3.err = errBoom
	s.cfg = f.cfg
	f.clock.Advance(time.Second)
	job, err = s.RequestBackup(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	done, err = f.runner.Wait(ctx, job.ID)
	if err != nil || done.Status != jobs.StatusFailed || !strings.Contains(done.Error, "upload") {
		t.Fatalf("job = %+v, %v", done, err)
	}
	if err := json.Unmarshal(done.Result, &res); err != nil || res.Backup.Name != "mailroom-20260105T090001Z.db" {
		t.Fatalf("partial result = %s", done.Result)
	}
	if keys := f.alerter.keys(); !slices.Equal(keys, []string{AlertBackupFailed}) {
		t.Fatalf("alerts = %v", keys)
	}
}

func TestRegisterJobsNeedsRunner(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	s.jobs = nil
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	s.RegisterJobs()
}

func TestRequestBackupNeedsDeps(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	s.jobs = nil
	if _, err := s.RequestBackup(f.ctx, false); err == nil {
		t.Fatal("no error without a runner")
	}
	s.db = nil
	if _, err := s.RequestBackup(f.ctx, false); !errors.Is(err, errNoDB) {
		t.Fatalf("err = %v", err)
	}
}
