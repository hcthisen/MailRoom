// Package db opens Mailroom's SQLite database with a single-connection
// write pool and a multi-connection read-only pool, runs the embedded
// forward-only migrations and provides transaction, time and JSON helpers.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DefaultFileName is the database file name inside the data directory.
const DefaultFileName = "mailroom.db"

// readConns is the size of the read pool. WAL readers never block the writer.
const readConns = 4

// Querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx, so functions can
// run standalone or join a caller's transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
	_ Querier = (*sql.DB)(nil)
	_ Querier = (*sql.Tx)(nil)
	_ Querier = (*sql.Conn)(nil)
)

// DB holds the two connection pools of one SQLite database.
type DB struct {
	// Write has exactly one connection; every transaction on it starts with
	// BEGIN IMMEDIATE, so writers queue instead of failing with SQLITE_BUSY.
	Write *sql.DB
	// Read has several query-only connections for concurrent reads.
	Read *sql.DB
	// Path is the database file.
	Path string
}

// Open opens (creating if needed) the database at path. If path is an
// existing directory, the database is DefaultFileName inside it. Open does
// not run migrations; call Migrate.
func Open(path string) (*DB, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, DefaultFileName)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("db: create directory: %w", err)
	}

	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, fmt.Errorf("db: open write pool: %w", err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	w.SetConnMaxIdleTime(0)
	// Create the file and switch it to WAL before readers attach.
	if err := w.PingContext(context.Background()); err != nil {
		w.Close()
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	// The database holds password hashes, TOTP secrets and contact data:
	// owner-only, whatever the umask or an existing directory's mode.
	if err := restrictFiles(path, path+"-wal", path+"-shm"); err != nil {
		w.Close()
		return nil, err
	}

	r, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("db: open read pool: %w", err)
	}
	r.SetMaxOpenConns(readConns)
	r.SetMaxIdleConns(readConns)
	if err := r.PingContext(context.Background()); err != nil {
		w.Close()
		r.Close()
		return nil, fmt.Errorf("db: open read pool %s: %w", path, err)
	}
	return &DB{Write: w, Read: r, Path: path}, nil
}

// dsn builds a modernc.org/sqlite DSN. The path is passed without the
// "file:" prefix so Windows paths and spaces need no URI escaping.
func dsn(path string, write bool) string {
	params := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(10000)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
	}
	if write {
		params = append(params, "_txlock=immediate")
	} else {
		params = append(params, "_pragma=query_only(1)")
	}
	return path + "?" + strings.Join(params, "&")
}

// FileMode is the permission of the database file, its WAL and shared-memory
// files and backups: readable and writable by the owner only.
const FileMode os.FileMode = 0o600

// restrictFiles sets FileMode on each existing path. SQLite creates the WAL
// and -shm files with the database file's mode, so restricting the main
// file right after creating it covers them too.
func restrictFiles(paths ...string) error {
	for _, p := range paths {
		if err := os.Chmod(p, FileMode); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("db: restrict permissions of %s: %w", p, err)
		}
	}
	return nil
}

// Close closes both pools.
func (d *DB) Close() error {
	return errors.Join(d.Read.Close(), d.Write.Close())
}

// Ping checks that both pools can reach the database.
func (d *DB) Ping(ctx context.Context) error {
	if err := d.Write.PingContext(ctx); err != nil {
		return fmt.Errorf("db: write pool: %w", err)
	}
	if err := d.Read.PingContext(ctx); err != nil {
		return fmt.Errorf("db: read pool: %w", err)
	}
	return nil
}

// WithTx runs fn in a write transaction (BEGIN IMMEDIATE). It commits when
// fn returns nil and rolls back in every other case: fn returns an error,
// panics (the panic is re-raised after the rollback) or exits the goroutine
// with runtime.Goexit (t.Fatal inside fn), so the single write connection
// is always released.
//
// When ctx carries a CommitHook (WithCommitHook), its Before func runs
// inside the transaction just before COMMIT, and its After func runs after
// a successful COMMIT.
func (d *DB) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := d.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	hook := commitHookFrom(ctx)
	if hook != nil && hook.Before != nil {
		if err = hook.Before(ctx, tx); err != nil {
			return fmt.Errorf("db: before commit: %w", err)
		}
	}
	done = true
	if err = tx.Commit(); err != nil {
		_ = tx.Rollback() // no-op unless the commit left the tx open
		return fmt.Errorf("db: commit: %w", err)
	}
	if hook != nil && hook.After != nil {
		hook.After()
	}
	return nil
}
