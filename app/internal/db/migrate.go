package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"mailroom/internal/core"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one embedded schema migration, named "NNNN_description.sql".
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded migrations in version order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("db: read migrations: %w", err)
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("db: migration %q must be named NNNN_description.sql", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("db: migrations %q and %q share version %d", prev, e.Name(), v)
		}
		seen[v] = e.Name()
		b, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("db: read migration %q: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: v, Name: e.Name(), SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at TEXT NOT NULL
)`

// appliedVersions returns the set of applied migration versions.
func (d *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	if _, err := d.Write.ExecContext(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("db: create schema_migrations: %w", err)
	}
	rows, err := d.Write.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("db: read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// Pending returns the embedded migrations not yet applied, in order. It
// fails if the database has a migration this binary doesn't know (the
// database is newer than the binary).
func (d *DB) Pending(ctx context.Context) ([]Migration, error) {
	all, err := Migrations()
	if err != nil {
		return nil, err
	}
	return d.PendingFrom(ctx, all)
}

// PendingFrom is Pending for an explicit, version-ordered migration list
// (tests use it to simulate a later schema version).
func (d *DB) PendingFrom(ctx context.Context, all []Migration) ([]Migration, error) {
	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}
	known := map[int]bool{}
	var pending []Migration
	for _, m := range all {
		known[m.Version] = true
		if !applied[m.Version] {
			pending = append(pending, m)
		}
	}
	for v := range applied {
		if !known[v] {
			return nil, fmt.Errorf("db: database has migration %d, which this binary doesn't know; use a newer binary", v)
		}
	}
	return pending, nil
}

// Migrate applies every pending embedded migration and returns the names of
// those applied. See MigrateFrom.
func (d *DB) Migrate(ctx context.Context) ([]string, error) {
	all, err := Migrations()
	if err != nil {
		return nil, err
	}
	return d.MigrateFrom(ctx, all)
}

// MigrateFrom applies every migration of all that is not applied yet, each
// in its own BEGIN IMMEDIATE transaction, and returns the names of those
// applied. Migrations are forward-only.
//
// Each migration runs with foreign key enforcement off, so the standard
// SQLite table rebuild (create new table, copy, drop old, rename) doesn't
// cascade-delete or null out child rows. Before COMMIT, PRAGMA
// foreign_key_check must come back empty, or the migration is rolled back.
// Enforcement is switched back on afterwards.
//
// A migration that another process applied in the meantime is skipped: the
// applied check is repeated inside the transaction.
func (d *DB) MigrateFrom(ctx context.Context, all []Migration) ([]string, error) {
	pending, err := d.PendingFrom(ctx, all)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range pending {
		applied, err := d.applyMigration(ctx, m)
		if err != nil {
			return names, fmt.Errorf("db: apply migration %s: %w", m.Name, err)
		}
		if applied {
			names = append(names, m.Name)
		}
	}
	return names, nil
}

// applyMigration runs one migration on a dedicated write connection. It
// returns false when the migration was already applied.
func (d *DB) applyMigration(ctx context.Context, m Migration) (applied bool, err error) {
	conn, err := d.Write.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	// PRAGMA foreign_keys is a no-op inside a transaction: set it before BEGIN.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return false, fmt.Errorf("disable foreign keys: %w", err)
	}
	defer func() {
		if _, ferr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`); ferr != nil {
			// A connection without enforcement must not go back to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("re-enable foreign keys: %w", ferr))
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
	}()
	var one int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, m.Version).Scan(&one); {
	case err == nil:
		return false, nil // applied by another process meanwhile
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return false, err
	}
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Name, FormatTime(core.SystemClock{}.Now())); err != nil {
		return false, err
	}
	done = true
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// foreignKeyCheck fails when PRAGMA foreign_key_check reports any row.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("foreign key check: %w", err)
		}
		problems = append(problems, fmt.Sprintf("%s row %d references a missing %s row", table, rowid.Int64, parent))
		if len(problems) == 5 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("migration leaves foreign key violations: %s", strings.Join(problems, "; "))
	}
	return nil
}

// SchemaVersion returns the highest applied migration version (0 if none).
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	if _, err := d.appliedVersions(ctx); err != nil {
		return 0, err
	}
	var v sql.NullInt64
	if err := d.Write.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("db: schema version: %w", err)
	}
	return int(v.Int64), nil
}
