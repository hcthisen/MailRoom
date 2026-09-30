package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestWithTxGoexit: a callback that leaves through runtime.Goexit (what
// t.Fatal does) must still release the single write connection.
func TestWithTxGoexit(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.WithTx(ctx, func(tx *sql.Tx) error {
			if err := insertSetting(ctx, tx, "goexit"); err != nil {
				return err
			}
			runtime.Goexit()
			return nil
		})
	}()
	<-done
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.WithTx(tctx, func(tx *sql.Tx) error { return insertSetting(tctx, tx, "after") }); err != nil {
		t.Fatalf("write pool stuck after Goexit: %v", err)
	}
	if n := countSettings(t, d); n != 1 {
		t.Fatalf("rows = %d, want 1 (the Goexit tx must roll back)", n)
	}
}

func TestCommitHook(t *testing.T) {
	ctx := context.Background()
	errHook := errors.New("hook")
	tests := []struct {
		name       string
		fnErr      error
		beforeErr  error
		wantBefore bool
		wantAfter  bool
		wantRows   int
	}{
		{name: "commit", wantBefore: true, wantAfter: true, wantRows: 2},
		{name: "fn fails", fnErr: errHook, wantRows: 0},
		{name: "before fails", beforeErr: errHook, wantBefore: true, wantRows: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openTestDB(t)
			var before, after bool
			hctx := WithCommitHook(ctx, &CommitHook{
				Before: func(ctx context.Context, tx *sql.Tx) error {
					before = true
					if err := insertSetting(ctx, tx, "hook"); err != nil {
						return err
					}
					return tt.beforeErr
				},
				After: func() { after = true },
			})
			err := d.WithTx(hctx, func(tx *sql.Tx) error {
				if err := insertSetting(ctx, tx, "fn"); err != nil {
					return err
				}
				return tt.fnErr
			})
			if (tt.fnErr != nil || tt.beforeErr != nil) != (err != nil) || (err != nil && !errors.Is(err, errHook)) {
				t.Fatalf("err = %v", err)
			}
			if before != tt.wantBefore || after != tt.wantAfter {
				t.Fatalf("before=%v after=%v", before, after)
			}
			if n := countSettings(t, d); n != tt.wantRows {
				t.Fatalf("rows = %d, want %d", n, tt.wantRows)
			}
		})
	}
}

// laterMigrations returns the embedded migrations plus extra ones.
func laterMigrations(t *testing.T, extra ...Migration) []Migration {
	t.Helper()
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	return append(all, extra...)
}

// TestMigrationTableRebuildKeepsChildren: the standard SQLite 12-step
// rebuild of a parent table must not cascade-delete its children.
func TestMigrationTableRebuildKeepsChildren(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO contacts (id, email, status, status_changed_at, created_at, updated_at) VALUES (1, 'a@example.com', 'active', 'x', 'x', 'x')`,
		`INSERT INTO tags (id, name, created_at) VALUES (1, 'vip', 'x')`,
		`INSERT INTO contact_tags (contact_id, tag_id, added_at) VALUES (1, 1, 'x')`,
	} {
		if _, err := d.Write.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	rebuild := Migration{Version: 900, Name: "0900_rebuild.sql", SQL: `
		CREATE TABLE tags_new (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE COLLATE NOCASE, created_at TEXT NOT NULL);
		INSERT INTO tags_new SELECT id, name, created_at FROM tags;
		DROP TABLE tags;
		ALTER TABLE tags_new RENAME TO tags;`}
	applied, err := d.MigrateFrom(ctx, laterMigrations(t, rebuild))
	if err != nil || len(applied) != 1 {
		t.Fatalf("MigrateFrom = %v, %v", applied, err)
	}
	var n int
	if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM contact_tags`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("contact_tags rows = %d, %v; the rebuild cascaded", n, err)
	}
	var fk int
	if err := d.Write.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys after migration = %d, %v", fk, err)
	}
}

func TestMigrationForeignKeyViolationRollsBack(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	bad := Migration{Version: 900, Name: "0900_bad.sql", SQL: `
		CREATE TABLE extra (id INTEGER PRIMARY KEY);
		INSERT INTO contact_tags (contact_id, tag_id, added_at) VALUES (42, 42, 'x');`}
	_, err := d.MigrateFrom(ctx, laterMigrations(t, bad))
	if err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("err = %v, want a foreign key violation", err)
	}
	var n int
	if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'extra'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed migration left table extra (n=%d, %v)", n, err)
	}
	if v, _ := d.SchemaVersion(ctx); v != latestVersion(t) {
		t.Fatalf("schema version = %d", v)
	}
	var fk int
	if err := d.Write.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys after failed migration = %d, %v", fk, err)
	}
}

// TestMigrationAppliedMeanwhileIsSkipped simulates a second process that
// applied a migration between Pending and the migration's transaction.
func TestMigrationAppliedMeanwhileIsSkipped(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	m := Migration{Version: 900, Name: "0900_x.sql", SQL: `CREATE TABLE x (id INTEGER PRIMARY KEY);`}
	if _, err := d.Write.ExecContext(ctx, m.SQL); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (900, '0900_x.sql', 'x')`); err != nil {
		t.Fatal(err)
	}
	applied, err := d.applyMigration(ctx, m)
	if err != nil || applied {
		t.Fatalf("applyMigration = %v, %v; want skipped", applied, err)
	}
}

func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o755); err != nil { // an existing, open directory
		t.Fatal(err)
	}
	d, err := Open(filepath.Join(dir, DefaultFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backups", "b.db")
	if err := d.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{d.Path, d.Path + "-wal", d.Path + "-shm", backup} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != FileMode {
			t.Errorf("%s mode = %v, want %v", filepath.Base(p), fi.Mode().Perm(), FileMode)
		}
	}
}
