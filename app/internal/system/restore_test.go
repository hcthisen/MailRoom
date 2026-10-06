package system

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

// openRaw opens a SQLite file directly (closed at test end).
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	c, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// makeBackup writes a migrated database holding one contact with email to
// a backup file and returns its path.
func makeBackup(t *testing.T, email string) string {
	t.Helper()
	src := testutil.NewDB(t)
	testutil.InsertContact(t, src, email, core.StatusActive)
	path := filepath.Join(t.TempDir(), "mailroom-20260101T023000Z.db")
	if err := src.Backup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	return path
}

// liveDB creates the live database at cfg.DBPath() with one contact and
// closes it (the server is stopped during a restore).
func liveDB(t *testing.T, path, email string) {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.InsertContact(t, d, email, core.StatusActive)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func contactEmails(t *testing.T, path string) []string {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Read.Query(`SELECT email FROM contacts ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		rows.Scan(&e)
		out = append(out, e)
	}
	return out
}

func restoreOpts(f *fixture) RestoreOptions {
	return RestoreOptions{DBPath: f.cfg.DBPath(), BackupsDir: f.cfg.BackupsDir(), Clock: f.clock, Logger: quietLogger()}
}

func TestRestore(t *testing.T) {
	f := newFixture(t)
	backup := makeBackup(t, "from-backup@example.org")
	before, _ := os.ReadFile(backup)
	liveDB(t, f.cfg.DBPath(), "live@example.org")

	res, err := Restore(f.ctx, restoreOpts(f), backup)
	if err != nil {
		t.Fatal(err)
	}
	if res.SchemaVersion != latestSchema() || res.NeedsMigration || res.DBPath != f.cfg.DBPath() || res.RestoredFrom != backup {
		t.Fatalf("result = %+v", res)
	}
	if want := filepath.Join(f.cfg.BackupsDir(), "pre-restore-20260105T090000Z.db"); res.SafetyBackup != want {
		t.Fatalf("safety backup = %q, want %q", res.SafetyBackup, want)
	}
	if got := contactEmails(t, f.cfg.DBPath()); len(got) != 1 || got[0] != "from-backup@example.org" {
		t.Fatalf("restored contacts = %v", got)
	}
	if got := contactEmails(t, res.SafetyBackup); len(got) != 1 || got[0] != "live@example.org" {
		t.Fatalf("safety backup contacts = %v", got)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(f.cfg.DBPath()); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", fi.Mode().Perm())
		}
	}
	if after, _ := os.ReadFile(backup); string(after) != string(before) {
		t.Error("the backup file was modified")
	}
	if _, err := os.Stat(f.cfg.DBPath() + ".restoring"); !os.IsNotExist(err) {
		t.Error("temporary copy left behind")
	}
	// The safety backup is listed like any other backup.
	list, _ := f.service().ListBackups()
	if len(list) != 1 || list[0].Name != "pre-restore-20260105T090000Z.db" {
		t.Fatalf("list = %+v", list)
	}
}

func TestRestoreRemovesStaleWAL(t *testing.T) {
	f := newFixture(t)
	backup := makeBackup(t, "from-backup@example.org")
	liveDB(t, f.cfg.DBPath(), "live@example.org")
	// Left-over sidecars of the old database must not survive the swap.
	os.WriteFile(f.cfg.DBPath()+"-wal", []byte("stale wal"), 0o600)
	os.WriteFile(f.cfg.DBPath()+"-shm", []byte("stale shm"), 0o600)
	if _, err := Restore(f.ctx, restoreOpts(f), backup); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.cfg.DBPath() + "-wal", f.cfg.DBPath() + "-shm"} {
		if b, err := os.ReadFile(p); err == nil && strings.HasPrefix(string(b), "stale") {
			t.Errorf("%s survived the restore", p)
		}
	}
	if got := contactEmails(t, f.cfg.DBPath()); len(got) != 1 || got[0] != "from-backup@example.org" {
		t.Fatalf("restored contacts = %v", got)
	}
}

func TestRestoreIntoEmptyDataDir(t *testing.T) {
	f := newFixture(t)
	backup := makeBackup(t, "a@example.org")
	res, err := Restore(f.ctx, restoreOpts(f), backup)
	if err != nil || res.SafetyBackup != "" {
		t.Fatalf("res = %+v, err %v", res, err)
	}
	// Nothing to carry over from: sending is paused, and the warnings say
	// that opt-outs after the backup are lost.
	if !res.SendingPaused || res.CarriedOver != nil || len(res.Warnings) != 3 || !strings.Contains(res.Warnings[1], "nothing could be carried over") || !strings.Contains(res.Warnings[2], "API tokens revoked") {
		t.Fatalf("res = %+v", res)
	}
	if got := contactEmails(t, f.cfg.DBPath()); len(got) != 1 {
		t.Fatalf("contacts = %v", got)
	}
}

func TestRestoreRejects(t *testing.T) {
	cases := []struct {
		name string
		file func(t *testing.T, f *fixture) string
		msg  string
	}{
		{"missing file", func(t *testing.T, f *fixture) string { return filepath.Join(t.TempDir(), "nope.db") }, ""},
		{"directory", func(t *testing.T, f *fixture) string { return t.TempDir() }, "not a regular file"},
		{"text file", func(t *testing.T, f *fixture) string {
			p := filepath.Join(t.TempDir(), "x.db")
			os.WriteFile(p, []byte("definitely not a database, just some text"), 0o600)
			return p
		}, "not a SQLite database"},
		{"damaged database", func(t *testing.T, f *fixture) string {
			p := makeBackup(t, "a@example.org")
			b, _ := os.ReadFile(p)
			for i := 100; i < len(b); i++ {
				b[i] = 0xAB
			}
			os.WriteFile(p, b, 0o600)
			return p
		}, ""},
		{"not a mailroom database", func(t *testing.T, f *fixture) string {
			p := filepath.Join(t.TempDir(), "other.db")
			c := openRaw(t, p)
			if _, err := c.Exec(`CREATE TABLE things (id INTEGER PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			c.Close()
			return p
		}, "not a Mailroom database"},
		{"newer schema", func(t *testing.T, f *fixture) string {
			p := makeBackup(t, "a@example.org")
			c := openRaw(t, p)
			if _, err := c.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (9999, '9999_future.sql', '2027-01-01T00:00:00.000000Z')`); err != nil {
				t.Fatal(err)
			}
			c.Close()
			return p
		}, "newer Mailroom binary"},
		{"no migrations", func(t *testing.T, f *fixture) string {
			p := makeBackup(t, "a@example.org")
			c := openRaw(t, p)
			c.Exec(`DELETE FROM schema_migrations`)
			c.Close()
			return p
		}, "no applied migrations"},
		{"the live database", func(t *testing.T, f *fixture) string { return f.cfg.DBPath() }, "live database"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			liveDB(t, f.cfg.DBPath(), "live@example.org")
			_, err := Restore(f.ctx, restoreOpts(f), tc.file(t, f))
			if !errors.Is(err, ErrInvalidBackup) {
				t.Fatalf("err = %v, want ErrInvalidBackup", err)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("err %q doesn't mention %q", err, tc.msg)
			}
			// Nothing changed: the live database, no safety backup, no temp file.
			if got := contactEmails(t, f.cfg.DBPath()); len(got) != 1 || got[0] != "live@example.org" {
				t.Fatalf("live contacts = %v", got)
			}
			if list, _ := f.service().ListBackups(); len(list) != 0 {
				t.Errorf("backups = %+v", list)
			}
			if _, err := os.Stat(f.cfg.DBPath() + ".restoring"); !os.IsNotExist(err) {
				t.Error("temporary copy left behind")
			}
		})
	}
}

// TestRestoreRefusesOpenDatabase: while another connection (a running
// server) has the live database open, Restore changes nothing.
func TestRestoreRefusesOpenDatabase(t *testing.T) {
	f := newFixture(t)
	liveDB(t, f.cfg.DBPath(), "live@example.org")
	backup := makeBackup(t, "restored@example.org")
	server, err := db.Open(f.cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Ping(f.ctx); err != nil {
		server.Close()
		t.Fatal(err)
	}
	_, err = Restore(f.ctx, restoreOpts(f), backup)
	if !errors.Is(err, ErrDatabaseInUse) || !strings.Contains(err.Error(), "stop the server") {
		server.Close()
		t.Fatalf("err = %v", err)
	}
	// Idle after a read, but still open: still in use.
	var n int
	if err := server.Read.QueryRow(`SELECT count(*) FROM contacts`).Scan(&n); err != nil || n != 1 {
		server.Close()
		t.Fatalf("server read = %d, %v", n, err)
	}
	if _, err := Restore(f.ctx, restoreOpts(f), backup); !errors.Is(err, ErrDatabaseInUse) {
		server.Close()
		t.Fatalf("idle err = %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.cfg.DBPath() + ".restoring", f.cfg.BackupsDir()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after a refused restore: %v", p, err)
		}
	}
	if got := contactEmails(t, f.cfg.DBPath()); len(got) != 1 || got[0] != "live@example.org" {
		t.Fatalf("live contacts = %v", got)
	}

	// Stopped: the restore goes through.
	if _, err := Restore(f.ctx, restoreOpts(f), backup); err != nil {
		t.Fatal(err)
	}
	if got := contactEmails(t, f.cfg.DBPath()); len(got) != 1 || got[0] != "restored@example.org" {
		t.Fatalf("restored contacts = %v", got)
	}
}

func TestServiceRestoreNeedsClosedDB(t *testing.T) {
	f := newFixture(t)
	backup := makeBackup(t, "a@example.org")
	if _, err := f.service().Restore(f.ctx, backup); err == nil || !strings.Contains(err.Error(), "exclusive access") {
		t.Fatalf("err = %v", err)
	}
	s := f.service()
	s.db = nil
	res, err := s.Restore(f.ctx, backup)
	if err != nil || res.DBPath != f.cfg.DBPath() {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
