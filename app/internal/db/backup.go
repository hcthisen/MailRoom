package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Backup writes a consistent, compacted copy of the database to destPath
// with VACUUM INTO. It runs on its own short-lived connection, so it
// neither blocks writers nor needs the query-only read pool. destPath must
// not exist yet; its directory is created if needed. The backup gets
// FileMode (owner-only), like the database.
func (d *DB) Backup(ctx context.Context, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return fmt.Errorf("db: backup directory: %w", err)
	}
	// Create the (empty) target owner-only first, so the copy is never
	// readable by others, not even while it is being written. VACUUM INTO
	// accepts an existing empty file.
	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("db: backup target %s already exists", destPath)
	}
	if err != nil {
		return fmt.Errorf("db: backup target: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(destPath)
		return fmt.Errorf("db: backup target: %w", err)
	}
	src, err := sql.Open("sqlite", d.Path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		_ = os.Remove(destPath)
		return fmt.Errorf("db: backup open: %w", err)
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	if _, err := src.ExecContext(ctx, `VACUUM INTO ?`, destPath); err != nil {
		_ = os.Remove(destPath)
		return fmt.Errorf("db: vacuum into %s: %w", destPath, err)
	}
	if err := restrictFiles(destPath); err != nil {
		_ = os.Remove(destPath)
		return err
	}
	return nil
}
