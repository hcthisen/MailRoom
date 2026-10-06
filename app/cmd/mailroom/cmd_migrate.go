package main

import (
	"context"
	"log/slog"
	"strings"

	"mailroom/internal/app"
	"mailroom/internal/config"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

func init() {
	register(Command{Name: "migrate", Summary: "apply pending database migrations (backup first)", Run: runMigrate})
}

func runMigrate(ctx context.Context, env *Env, args []string) error {
	fs, jsonOut := newFlagSet(env, "migrate")
	if err := fs.Parse(args); err != nil {
		return &ExitCodeError{Code: ExitError}
	}
	cfg, err := config.LoadFrom(env.Getenv)
	if err != nil {
		return err
	}
	d, err := db.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer d.Close()
	res, err := migrate(ctx, d, cfg.BackupsDir(), nil)
	if err != nil {
		return err
	}
	text := "database is up to date"
	if len(res.Applied) > 0 {
		text = "applied " + strings.Join(res.Applied, ", ")
		if res.Backup != "" {
			text += " (backup: " + res.Backup + ")"
		}
	}
	return printResult(env, *jsonOut, res, text)
}

// migrations returns the migrations the CLI applies (the embedded ones).
// Tests replace it to simulate an upgrade.
var migrations = db.Migrations

// migrate applies the pending migrations with app.Migrate: an existing
// database is backed up to backupsDir first (PRD O-6).
func migrate(ctx context.Context, d *db.DB, backupsDir string, log *slog.Logger) (app.MigrateResult, error) {
	all, err := migrations()
	if err != nil {
		return app.MigrateResult{}, err
	}
	return app.Migrate(ctx, d, all, backupsDir, core.SystemClock{}, log)
}
