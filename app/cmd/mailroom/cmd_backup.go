package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"mailroom/internal/config"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
	"mailroom/internal/system"
)

func init() {
	register(Command{Name: "backup", Summary: "write a backup of the database (VACUUM INTO; uploaded to BACKUP_BUCKET if set)", Run: runBackup})
	register(Command{Name: "restore", Summary: "replace the database with a backup file (restore FILE; the server must be stopped)", Run: runRestore})
}

// runBackup takes a backup like the nightly one, also while the server is
// stopped (L-6): a VACUUM INTO copy in <data>/backups, uploaded to
// BACKUP_BUCKET when that is configured, with old local backups pruned.
func runBackup(ctx context.Context, env *Env, args []string) error {
	fs, jsonOut := newFlagSet(env, "backup")
	if err := fs.Parse(args); err != nil {
		return &ExitCodeError{Code: ExitError}
	}
	if fs.NArg() > 0 {
		return &ExitCodeError{Code: ExitError, Err: fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	cfg, err := config.LoadFrom(env.Getenv)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.DBPath()); err != nil {
		return fmt.Errorf("no database at %s to back up: %w", cfg.DBPath(), err)
	}
	d, err := db.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer d.Close()
	in, err := newIntegrations(ctx, cfg)
	if err != nil {
		return err
	}
	sys := system.New(system.Deps{
		DB: d, Config: cfg, Settings: settings.New(settings.Deps{DB: d, Domain: cfg.Domain}),
		S3: in.AWS.S3, Version: version, Logger: newStderrLogger(env),
	})
	res, err := sys.Backup(ctx)
	if res.Backup.Name == "" {
		return err
	}
	text := fmt.Sprintf("backup written: %s (%d bytes)", res.Path, res.Backup.SizeBytes)
	if res.S3Key != "" {
		text += "\nuploaded to s3://" + cfg.BackupBucket + "/" + res.S3Key
	}
	if perr := printResult(env, *jsonOut, res, text); perr != nil {
		return perr
	}
	// The local backup exists; a failed upload still fails the command.
	return err
}

// runRestore replaces the live database with a backup (L-6), after a
// safety copy of the current one. It refuses while the database is open
// elsewhere (a running server).
func runRestore(ctx context.Context, env *Env, args []string) error {
	fs, jsonOut := newFlagSet(env, "restore")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return &ExitCodeError{Code: ExitError}
	}
	if len(pos) != 1 {
		return &ExitCodeError{Code: ExitError, Err: errors.New("usage: mailroom restore [--json] BACKUP_FILE")}
	}
	cfg, err := config.LoadFrom(env.Getenv)
	if err != nil {
		return err
	}
	res, err := system.Restore(ctx, system.RestoreOptions{
		DBPath: cfg.DBPath(), BackupsDir: cfg.BackupsDir(), Clock: core.SystemClock{}, Logger: newStderrLogger(env),
	}, pos[0])
	if err != nil && !errors.Is(err, system.ErrNotPaused) {
		return err
	}
	text := "restored " + res.DBPath + " from " + res.RestoredFrom
	if res.SafetyBackup != "" {
		text += "\nthe replaced database was saved as " + res.SafetyBackup
	}
	if res.NeedsMigration {
		text += "\nthe backup has an older schema; it is migrated at the next start (or run mailroom migrate)"
	}
	if c := res.CarriedOver; c != nil {
		text += fmt.Sprintf("\ncarried over from the replaced database: %d suppressions, %d erased contacts, %d contact statuses, "+
			"%d opted-out contacts added, %d messages already sent, %d enrollments moved on or opted out, "+
			"%d tokens revoked, %d users removed, %d passwords carried over",
			c.Suppressions, c.ErasedContacts, c.Statuses, c.ContactsAdded, c.Messages, c.Enrollments,
			c.TokensRevoked, c.UsersRemoved, c.UsersUpdated)
	}
	if res.SessionsEnded > 0 {
		text += fmt.Sprintf("\n%d admin UI sessions ended: everyone signs in again", res.SessionsEnded)
	}
	for _, w := range res.Warnings {
		text += "\nWARNING: " + w
	}
	if perr := printResult(env, *jsonOut, res, text); perr != nil || err != nil {
		return errors.Join(err, perr)
	}
	return nil
}
