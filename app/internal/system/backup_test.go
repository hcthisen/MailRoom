package system

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/jobs"
	"mailroom/internal/testutil"
)

func TestBackupNames(t *testing.T) {
	at := time.Date(2026, 1, 5, 2, 30, 0, 0, time.UTC)
	if got := backupName(BackupPrefix, at); got != "mailroom-20260105T023000Z.db" {
		t.Fatalf("name = %s", got)
	}
	cph, _ := time.LoadLocation("Europe/Copenhagen")
	if got := backupName(BackupPrefix, at.In(cph)); got != "mailroom-20260105T023000Z.db" {
		t.Fatalf("name from a local time = %s, want the UTC stamp", got)
	}
	for name, valid := range map[string]bool{
		"mailroom-20260105T023000Z.db":                   true,
		"pre-migrate-v1-20260105T023000Z.db":             true,
		"pre-restore-20260105T023000Z.db":                true,
		"mailroom-20260105T023000Z.db-wal":               false,
		"Mailroom-20260105T023000Z.db":                   false,
		"mailroom-20260105T0230Z.db":                     false,
		"../mailroom-20260105T023000Z.db":                false,
		"mailroom-20260105T023000Z.db/../x":              false,
		"a/mailroom-20260105T023000Z.db":                 false,
		`a\mailroom-20260105T023000Z.db`:                 false,
		"mailroom.db":                                    false,
		"":                                               false,
		"mailroom-20260105T023000Z.db\x00":               false,
		"-20260105T023000Z.db":                           false,
		strings.Repeat("a", 70) + "-20260105T023000Z.db": false,
	} {
		if ValidBackupName(name) != valid {
			t.Errorf("ValidBackupName(%q) = %v", name, !valid)
		}
	}
	if got, ok := backupTime("pre-migrate-v1-20260105T023000Z.db"); !ok || !got.Equal(at) {
		t.Fatalf("backupTime = %v %v", got, ok)
	}
}

func TestBackupWritesValidCopy(t *testing.T) {
	f := newFixture(t)
	c := testutil.InsertContact(t, f.d, "ada@example.org", core.StatusActive)
	s := f.service()

	res, err := s.Backup(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantName := "mailroom-20260105T090000Z.db"
	if res.Backup.Name != wantName || res.Path != filepath.Join(f.cfg.BackupsDir(), wantName) || res.S3Key != "" {
		t.Fatalf("result = %+v", res)
	}
	fi, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Backup.SizeBytes != fi.Size() || fi.Size() == 0 || !res.Backup.CreatedAt.Equal(testutil.Epoch) {
		t.Errorf("info = %+v, file size %d", res.Backup, fi.Size())
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if v, err := ValidateBackup(f.ctx, res.Path); err != nil || v != latestSchema() {
		t.Fatalf("ValidateBackup = %d, %v", v, err)
	}
	// The copy holds the data.
	copyDB := openRaw(t, res.Path)
	var email string
	if err := copyDB.QueryRow(`SELECT email FROM contacts WHERE id = ?`, c.ID).Scan(&email); err != nil || email != "ada@example.org" {
		t.Fatalf("contact in backup = %q, %v", email, err)
	}

	// A second backup in the same second returns the first one.
	again, err := s.Backup(f.ctx)
	if err != nil || again.Backup != res.Backup || again.Path != res.Path || len(again.Pruned) != 0 {
		t.Fatalf("same-second backup = %+v, %v", again, err)
	}
	// A same-named file this process didn't write is never overwritten.
	other := f.service()
	if _, err := other.Backup(f.ctx); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("foreign same-second backup err = %v", err)
	}
	f.clock.Advance(time.Second)
	if _, err := s.Backup(f.ctx); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListBackups()
	if err != nil || len(list) != 2 || list[0].Name != "mailroom-20260105T090001Z.db" || list[1].Name != wantName {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestBackupPrunesOldBackups(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	old := f.writeBackupFile(BackupPrefix, now.Add(-15*day))
	oldMigrate := f.writeBackupFile("pre-migrate-v1-", now.Add(-20*day))
	keep := f.writeBackupFile(BackupPrefix, now.Add(-13*day))
	unrelated := filepath.Join(f.cfg.BackupsDir(), "notes.txt")
	if err := os.WriteFile(unrelated, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := f.service().Backup(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(res.Pruned)
	if want := []string{old, oldMigrate}; !slices.Equal(res.Pruned, want) {
		t.Fatalf("pruned = %v, want %v", res.Pruned, want)
	}
	for _, name := range []string{old, oldMigrate} {
		if _, err := os.Stat(filepath.Join(f.cfg.BackupsDir(), name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists", name)
		}
	}
	for _, p := range []string{filepath.Join(f.cfg.BackupsDir(), keep), unrelated, res.Path} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	list, _ := f.service().ListBackups()
	if len(list) != 2 || list[0].Name != res.Backup.Name || list[1].Name != keep {
		t.Fatalf("list = %+v", list)
	}
}

func TestBackupUploadsToS3(t *testing.T) {
	f := newFixture(t)
	f.cfg.BackupBucket = "mailroom-backup-example"
	res, err := f.service().Backup(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.s3.puts) != 1 {
		t.Fatalf("puts = %d", len(f.s3.puts))
	}
	p := f.s3.puts[0]
	if p.bucket != "mailroom-backup-example" || p.key != "backups/"+res.Backup.Name || p.sse != "AES256" ||
		p.contentType != "application/vnd.sqlite3" || p.length != res.Backup.SizeBytes {
		t.Fatalf("put = %+v", p)
	}
	local, _ := os.ReadFile(res.Path)
	if !bytes.Equal(p.body, local) {
		t.Fatal("uploaded body differs from the local backup")
	}
	if res.S3Key != "backups/"+res.Backup.Name {
		t.Fatalf("S3Key = %q", res.S3Key)
	}
}

func TestBackupSameSecondRetriesUpload(t *testing.T) {
	f := newFixture(t)
	f.cfg.BackupBucket = "b"
	f.s3.err = errBoom
	s := f.service()
	first, err := s.Backup(f.ctx)
	if err == nil || first.Backup.Name == "" || first.S3Key != "" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	f.s3.err = nil
	second, err := s.Backup(f.ctx)
	if err != nil || second.Backup != first.Backup || second.S3Key != "backups/"+first.Backup.Name || len(f.s3.puts) != 1 {
		t.Fatalf("second = %+v, %v, puts %d", second, err, len(f.s3.puts))
	}
	// Uploaded now: a third request doesn't upload again.
	if third, err := s.Backup(f.ctx); err != nil || third.S3Key != second.S3Key || len(f.s3.puts) != 1 {
		t.Fatalf("third = %+v, %v, puts %d", third, err, len(f.s3.puts))
	}
}

// TestBackupJobsSameSecond: two on-demand backups in the same second both
// succeed with the same backup and raise no alert.
func TestBackupJobsSameSecond(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	s.RegisterJobs()
	startRunner(t, f.runner)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	var names []string
	for range 2 {
		job, err := s.RequestBackup(f.ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		done, err := f.runner.Wait(ctx, job.ID)
		if err != nil || done.Status != jobs.StatusSucceeded {
			t.Fatalf("job = %+v, %v", done, err)
		}
		var res BackupResult
		if err := json.Unmarshal(done.Result, &res); err != nil {
			t.Fatal(err)
		}
		names = append(names, res.Backup.Name)
	}
	if names[0] != "mailroom-20260105T090000Z.db" || names[1] != names[0] {
		t.Fatalf("backups = %v", names)
	}
	if keys := f.alerter.keys(); len(keys) != 0 {
		t.Fatalf("alerts = %v", keys)
	}
}

func TestBackupConflictDoesNotAlert(t *testing.T) {
	f := newFixture(t)
	f.writeBackupFile(BackupPrefix, f.clock.Now()) // another process, this second
	_, err := f.service().runBackup(f.ctx)
	if !errors.Is(err, core.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	if keys := f.alerter.keys(); len(keys) != 0 {
		t.Fatalf("alerts = %v", keys)
	}
}

func TestBackupPrunesFutureDatedByModTime(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	dir := f.cfg.BackupsDir()
	stale := f.writeBackupFile(BackupPrefix, now.Add(30*day)) // written 20 days ago with a clock 50 days ahead
	fresh := f.writeBackupFile(BackupPrefix, now.Add(10*day)) // written just now with a clock 10 days ahead
	if err := os.Chtimes(filepath.Join(dir, stale), now.Add(-20*day), now.Add(-20*day)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, fresh), now, now); err != nil {
		t.Fatal(err)
	}
	res, err := f.service().Backup(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Pruned, []string{stale}) {
		t.Fatalf("pruned = %v, want [%s]", res.Pruned, stale)
	}
	if _, err := os.Stat(filepath.Join(dir, fresh)); err != nil {
		t.Fatalf("%s was removed: %v", fresh, err)
	}
}

func TestBackupFailuresAlert(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(f *fixture)
		wantLocal bool // the local backup exists despite the error
		title     string
	}{
		{"upload fails", func(f *fixture) {
			f.cfg.BackupBucket = "b"
			f.s3.err = errBoom
		}, true, "Backup upload to S3 failed"},
		{"bucket without AWS", func(f *fixture) {
			f.cfg.BackupBucket = "b"
			f.s3 = nil
		}, true, "Backup upload to S3 failed"},
		{"backup dir unusable", func(f *fixture) {
			// A file where the backups directory should be.
			if err := os.WriteFile(f.cfg.BackupsDir(), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false, "Backup failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			res, err := f.service().runBackup(f.ctx)
			if err == nil {
				t.Fatal("no error")
			}
			if (res.Backup.Name != "") != tc.wantLocal {
				t.Fatalf("result = %+v", res)
			}
			if tc.wantLocal {
				if _, err := os.Stat(res.Path); err != nil {
					t.Fatalf("local backup missing: %v", err)
				}
			}
			if len(f.alerter.alerts) != 1 {
				t.Fatalf("alerts = %+v", f.alerter.alerts)
			}
			a := f.alerter.alerts[0]
			if a.key != AlertBackupFailed || a.title != tc.title || !strings.Contains(a.body, "mailroom backup") {
				t.Fatalf("alert = %+v", a)
			}
		})
	}
}

func TestBackupWithoutAlerterOrDB(t *testing.T) {
	f := newFixture(t)
	f.cfg.BackupBucket = "b"
	f.s3.err = errBoom
	s := f.service()
	s.alerter = nil
	if _, err := s.runBackup(f.ctx); err == nil {
		t.Fatal("no error")
	}
	s.db = nil
	if _, err := s.Backup(f.ctx); !errors.Is(err, errNoDB) {
		t.Fatalf("no-DB backup err = %v", err)
	}
}

func TestListBackups(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	if list, err := s.ListBackups(); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("missing dir: %v %v", list, err)
	}
	now := f.clock.Now()
	b1 := f.writeBackupFile(BackupPrefix, now.Add(-2*time.Hour))
	b2 := f.writeBackupFile("pre-migrate-v1-", now.Add(-time.Hour))
	b3 := f.writeBackupFile(BackupPrefix, now.Add(-3*time.Hour))
	for _, junk := range []string{"README", "mailroom.db", "mailroom-20260105T090000Z.db-wal"} {
		os.WriteFile(filepath.Join(f.cfg.BackupsDir(), junk), []byte("x"), 0o600)
	}
	os.Mkdir(filepath.Join(f.cfg.BackupsDir(), "mailroom-20260105T080000Z.db"), 0o700) // a directory
	list, err := s.ListBackups()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range list {
		names = append(names, b.Name)
	}
	if want := []string{b2, b1, b3}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if list[0].SizeBytes != int64(len("backup "+b2)) || !list[0].CreatedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("info = %+v", list[0])
	}
}

func TestOpenBackup(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	name := f.writeBackupFile(BackupPrefix, f.clock.Now())

	file, info, err := s.OpenBackup(name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(file)
	file.Close()
	if string(b) != "backup "+name || info.Name != name || info.SizeBytes != int64(len(b)) {
		t.Fatalf("read %q, info %+v", b, info)
	}

	// Secrets next to the backups directory must be unreachable.
	os.WriteFile(filepath.Join(f.cfg.DataDir, "secret.key"), []byte("topsecret"), 0o600)
	for _, bad := range []string{
		"../secret.key", "..%2Fsecret.key", "../mailroom.db", "/etc/passwd", `..\secret.key`,
		name + "/../../secret.key", "./" + name, "", strings.ToUpper(name),
	} {
		if _, _, err := s.OpenBackup(bad); !errors.Is(err, core.ErrValidation) {
			t.Errorf("OpenBackup(%q) err = %v, want validation", bad, err)
		}
	}
	if _, _, err := s.OpenBackup("mailroom-20250101T000000Z.db"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("unknown backup err = %v", err)
	}

	// A symlink with a valid name that leaves the directory is refused.
	if runtime.GOOS != "windows" {
		link := backupName(BackupPrefix, f.clock.Now().Add(-time.Minute))
		if err := os.Symlink(filepath.Join(f.cfg.DataDir, "secret.key"), filepath.Join(f.cfg.BackupsDir(), link)); err != nil {
			t.Fatal(err)
		}
		if file, _, err := s.OpenBackup(link); !errors.Is(err, core.ErrNotFound) {
			if file != nil {
				file.Close()
			}
			t.Errorf("escaping symlink err = %v", err)
		}
	}

	// No backups directory at all.
	f2 := newFixture(t)
	if _, _, err := f2.service().OpenBackup(name); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("missing dir err = %v", err)
	}
}

// TestBackupUploadsTheSecretKey: with the generated key file in use, every
// upload also puts secret.key at S3SecretKey (outside the expiring
// backups/ prefix), so a restore on a new server keeps the links in sent
// emails working. A key file that isn't the one in use (MAILROOM_SECRET
// is set) is not uploaded, and a failed key upload is retried.
func TestBackupUploadsTheSecretKey(t *testing.T) {
	f := newFixture(t)
	f.cfg.BackupBucket = "b"
	if err := os.MkdirAll(f.cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cfg.SecretKeyPath(), []byte("some-other-key-0123456789abcdef-xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := f.service().Backup(f.ctx)
	if err != nil || res.SecretKeyS3Key != "" || len(f.s3.puts) != 1 {
		t.Fatalf("unused key file: %+v, %v, puts %d", res, err, len(f.s3.puts))
	}

	f.clock.Advance(time.Second)
	key := string(f.cfg.Secret) + "\n"
	if err := os.WriteFile(f.cfg.SecretKeyPath(), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	s := f.service()
	res, err = s.Backup(f.ctx)
	if err != nil || res.SecretKeyS3Key != S3SecretKey || len(f.s3.puts) != 3 {
		t.Fatalf("key file in use: %+v, %v, puts %d", res, err, len(f.s3.puts))
	}
	p := f.s3.puts[2]
	if p.bucket != "b" || p.key != "keys/secret.key" || p.sse != "AES256" || string(p.body) != key {
		t.Fatalf("key put = %+v", p)
	}
	if strings.HasPrefix(S3SecretKey, S3KeyPrefix) {
		t.Fatal("the key would expire with the backups")
	}

	// The key upload fails once: the backup upload is not repeated, the
	// key upload is.
	f.clock.Advance(time.Second)
	f.s3.failKey = S3SecretKey
	if res, err = s.Backup(f.ctx); err == nil || res.S3Key == "" || res.SecretKeyS3Key != "" {
		t.Fatalf("failed key upload: %+v, %v", res, err)
	}
	f.s3.failKey = ""
	before := len(f.s3.puts)
	if res, err = s.Backup(f.ctx); err != nil || res.SecretKeyS3Key != S3SecretKey || len(f.s3.puts) != before+1 {
		t.Fatalf("retry: %+v, %v, puts %d", res, err, len(f.s3.puts)-before)
	}
}
