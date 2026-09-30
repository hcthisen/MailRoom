package app

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// MigrateResult reports what Migrate did.
type MigrateResult struct {
	Applied []string `json:"applied"`
	Backup  string   `json:"backup,omitempty"`
	Version int      `json:"schema_version"`
}

// Migrate applies the pending migrations among all (normally
// db.Migrations()). When the database already has a schema, it first takes
// a VACUUM INTO backup in backupsDir named pre-migrate-v<N>-<time>.db
// (PRD O-6); if that backup fails, nothing is migrated. log may be nil.
func Migrate(ctx context.Context, d *db.DB, all []db.Migration, backupsDir string, clock core.Clock, log *slog.Logger) (MigrateResult, error) {
	if clock == nil {
		clock = core.SystemClock{}
	}
	res := MigrateResult{Applied: []string{}}
	pending, err := d.PendingFrom(ctx, all)
	if err != nil {
		return res, err
	}
	current, err := d.SchemaVersion(ctx)
	if err != nil {
		return res, err
	}
	if len(pending) > 0 && current > 0 {
		name := fmt.Sprintf("pre-migrate-v%d-%s.db", current, clock.Now().UTC().Format("20060102T150405Z"))
		res.Backup = filepath.Join(backupsDir, name)
		if log != nil {
			log.Info("backing up before migration", "path", res.Backup)
		}
		if err := d.Backup(ctx, res.Backup); err != nil {
			return res, fmt.Errorf("backup before migration: %w", err)
		}
	}
	applied, err := d.MigrateFrom(ctx, all)
	res.Applied = append(res.Applied, applied...)
	if err != nil {
		return res, err
	}
	if log != nil && len(applied) > 0 {
		log.Info("migrations applied", "migrations", applied)
	}
	res.Version, err = d.SchemaVersion(ctx)
	return res, err
}

// OpenDB opens the database file at path and applies the embedded
// migrations (with the backup-first rule of Migrate, backups in
// backupsDir).
func OpenDB(ctx context.Context, path, backupsDir string, clock core.Clock, log *slog.Logger) (*db.DB, MigrateResult, error) {
	d, err := db.Open(path)
	if err != nil {
		return nil, MigrateResult{}, err
	}
	all, err := db.Migrations()
	if err != nil {
		d.Close()
		return nil, MigrateResult{}, err
	}
	res, err := Migrate(ctx, d, all, backupsDir, clock, log)
	if err != nil {
		d.Close()
		return nil, res, err
	}
	return d, res, nil
}
