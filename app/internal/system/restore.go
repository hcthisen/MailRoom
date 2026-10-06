package system

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// sqliteHeader starts every SQLite 3 database file.
var sqliteHeader = []byte("SQLite format 3\x00")

// ErrDatabaseInUse is returned by Restore while another connection, such as
// a running `mailroom serve`, has the live database open.
var ErrDatabaseInUse = errors.New("the database is in use: stop the server (e.g. `docker compose stop mailroom`) and run the restore again")

// ErrNotPaused is returned by Restore when the backup was restored but
// sending could not be paused in it: the server must not be started
// before it is (the result is filled in).
var ErrNotPaused = errors.New("the backup was restored, but sending could not be paused in it: don't start the server; pause sending first (mailroom restore again, or stop it right after start with POST /api/v1/sending/pause)")

// ErrInvalidBackup marks a file that can't be restored: not a SQLite
// database, damaged, not a Mailroom database, or from a newer Mailroom.
var ErrInvalidBackup = errors.New("invalid backup")

// ValidateBackup checks that path is an intact Mailroom database whose
// schema this binary knows, and returns its schema version. SQLite may
// create -wal/-shm files next to path while it is open; Restore therefore
// validates a private copy.
func ValidateBackup(ctx context.Context, path string) (int, error) {
	if err := checkHeader(path); err != nil {
		return 0, err
	}
	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, fmt.Errorf("%w: open: %v", ErrInvalidBackup, err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	var check string
	if err := conn.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return 0, fmt.Errorf("%w: %s is not a readable SQLite database: %v", ErrInvalidBackup, path, err)
	}
	if check != "ok" {
		return 0, fmt.Errorf("%w: %s is damaged: %s", ErrInvalidBackup, path, check)
	}
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return 0, fmt.Errorf("%w: %s is not a Mailroom database (no schema_migrations table)", ErrInvalidBackup, path)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return 0, fmt.Errorf("%w: read schema_migrations: %v", ErrInvalidBackup, err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("%w: read schema_migrations: %v", ErrInvalidBackup, err)
	}
	all, err := db.Migrations()
	if err != nil {
		return 0, err
	}
	known := map[int]bool{}
	latest := 0
	for _, m := range all {
		known[m.Version] = true
		latest = max(latest, m.Version)
	}
	version := 0
	for _, v := range versions {
		if !known[v] {
			return 0, fmt.Errorf("%w: the backup has schema version %d, but this binary only knows versions up to %d; restore it with a newer Mailroom binary",
				ErrInvalidBackup, v, latest)
		}
		version = max(version, v)
	}
	if version == 0 {
		return 0, fmt.Errorf("%w: %s has no applied migrations", ErrInvalidBackup, path)
	}
	return version, nil
}

func checkHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBackup, err)
	}
	defer f.Close()
	head := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, sqliteHeader) {
		return fmt.Errorf("%w: %s is not a SQLite database", ErrInvalidBackup, path)
	}
	return nil
}

// RestoreOptions says where Restore puts the restored database.
type RestoreOptions struct {
	DBPath     string // the live database file (config.Config.DBPath)
	BackupsDir string // where the pre-restore safety backup goes
	Clock      core.Clock
	Logger     *slog.Logger
}

// RestoreResult reports what Restore did.
type RestoreResult struct {
	RestoredFrom  string `json:"restored_from" doc:"The backup file that was restored"`
	DBPath        string `json:"db_path" doc:"The database file that was replaced"`
	SafetyBackup  string `json:"safety_backup,omitempty" doc:"Copy of the replaced database, taken first"`
	SchemaVersion int    `json:"schema_version" doc:"Schema version of the restored database"`
	// NeedsMigration is true when the backup is older than this binary's
	// schema; migrations run at the next start (or `mailroom migrate`).
	NeedsMigration bool `json:"needs_migration" doc:"The restored schema is older than this binary's; it is migrated at the next start"`
	// SendingPaused is true when the restored database has all sending
	// paused (always, unless writing the pause failed): an operator
	// resumes it on purpose after checking the restored data.
	SendingPaused bool `json:"sending_paused" doc:"All sending is paused in the restored database until resumed through the API or UI"`
	// SessionsEnded counts the admin UI sessions of the backup that were
	// ended: everyone logs in again (with the credentials carried over).
	SessionsEnded int `json:"sessions_ended" doc:"Admin UI sessions from the backup that were ended; everyone logs in again"`
	// CarriedOver counts what was carried over from the replaced database
	// (nil when there was none, or carrying over failed; see Warnings).
	CarriedOver *ReconcileResult `json:"carried_over,omitempty" doc:"Opt-outs, erasures and sends carried over from the replaced database"`
	// Warnings are things the operator must check before resuming.
	Warnings []string `json:"warnings,omitempty" doc:"Things to check before resuming sending"`
}

// Restore replaces the database at opts.DBPath with the backup file (L-6).
//
// The server must be stopped: Restore needs exclusive access to the
// database file, and a running server would keep writing to the replaced
// file. Restore checks this by taking an exclusive lock on the live
// database (which fails while any other connection, in this or another
// process, has it open) before it starts and again right before the
// replacement, and returns ErrDatabaseInUse otherwise. Restore copies file next to the database, validates the copy
// (ValidateBackup: intact SQLite, a Mailroom schema no newer than this
// binary), takes a safety backup of the current database into
// opts.BackupsDir (pre-restore-<stamp>.db; a raw copy when the current file
// is too damaged for VACUUM INTO), removes the old -wal and -shm files and
// moves the copy into place with mode 0600. The backup file itself is never
// modified. Nothing is replaced when any step before the move fails.
//
// A backup is up to a day old, and everything after it would be undone:
// unsubscribes, suppressions, GDPR erasures, and which queued messages
// were already sent. So after the move Restore pauses all sending in the
// restored database (a manual pause, lifted only by a resume through the
// API or UI; pauseRestored) and carries opt-outs, erasures and sends over
// from the safety backup of the replaced database (reconcile). When there
// was no database to replace (a new server) or carrying over fails, the
// result's Warnings say so; the restore itself stands.
func Restore(ctx context.Context, opts RestoreOptions, file string) (RestoreResult, error) {
	if opts.Clock == nil {
		opts.Clock = core.SystemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	res := RestoreResult{RestoredFrom: file, DBPath: opts.DBPath}
	fi, err := os.Stat(file)
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrInvalidBackup, err)
	}
	if !fi.Mode().IsRegular() {
		return res, fmt.Errorf("%w: %s is not a regular file", ErrInvalidBackup, file)
	}
	if same, _ := sameFile(file, opts.DBPath); same {
		return res, fmt.Errorf("%w: %s is the live database itself", ErrInvalidBackup, file)
	}
	if err := checkHeader(file); err != nil {
		return res, err
	}
	if err := checkNotInUse(ctx, opts.DBPath); err != nil {
		return res, err
	}

	tmp := opts.DBPath + ".restoring"
	removeDBFiles(tmp)
	if err := copyFile(file, tmp); err != nil {
		removeDBFiles(tmp)
		return res, fmt.Errorf("system: restore: copy backup: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			removeDBFiles(tmp)
		}
	}()
	version, err := ValidateBackup(ctx, tmp)
	if err != nil {
		return res, err
	}
	res.SchemaVersion = version
	removeSidecars(tmp) // closed: nothing may be left to replay

	if _, err := os.Stat(opts.DBPath); err == nil {
		safety, err := safetyBackup(ctx, opts)
		if err != nil {
			return res, fmt.Errorf("system: restore: safety backup of the current database failed, nothing was changed: %w", err)
		}
		res.SafetyBackup = safety
		opts.Logger.Info("system: safety backup written before restore", "path", safety)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("system: restore: %w", err)
	}

	// The safety backup took time: make sure no server started meanwhile.
	if err := checkNotInUse(ctx, opts.DBPath); err != nil {
		return res, err
	}
	// Stale WAL frames of the old database must never be replayed into the
	// restored one.
	for _, p := range []string{opts.DBPath + "-wal", opts.DBPath + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, fmt.Errorf("system: restore: remove %s: %w", p, err)
		}
	}
	if err := os.Rename(tmp, opts.DBPath); err != nil {
		return res, fmt.Errorf("system: restore: move the restored database into place: %w", err)
	}
	committed = true
	if err := os.Chmod(opts.DBPath, db.FileMode); err != nil {
		return res, fmt.Errorf("system: restore: %w", err)
	}

	all, err := db.Migrations()
	if err == nil && len(all) > 0 {
		res.NeedsMigration = version < all[len(all)-1].Version
	}
	opts.Logger.Info("system: database restored", "from", filepath.Base(file), "schema_version", version)
	if err := afterRestore(ctx, opts, filepath.Base(file), &res); err != nil {
		return res, err
	}
	return res, nil
}

// afterRestore pauses sending in the restored database and carries over
// what the replaced one knew (see Restore). It fails only when the pause
// can't be written: the restored server must then not be started.
func afterRestore(ctx context.Context, opts RestoreOptions, backup string, res *RestoreResult) error {
	now := opts.Clock.Now()
	d, err := db.Open(opts.DBPath)
	if err != nil {
		return fmt.Errorf("system: restore: %w: open: %v", ErrNotPaused, err)
	}
	defer d.Close()
	if err := pauseRestored(ctx, d, backup, now); err != nil {
		return fmt.Errorf("system: restore: %w: %v", ErrNotPaused, err)
	}
	res.SendingPaused = true
	res.Warnings = append(res.Warnings, "All sending is paused. Check the contacts, broadcasts and sequences, then resume "+
		"sending (POST /api/v1/sending/resume, or Resume sending on the Health page of the admin UI).")
	// Sessions of the backup would let a user whose access was taken away
	// after it back in: end them all.
	r, err := d.Write.ExecContext(ctx, `DELETE FROM sessions`)
	if err != nil {
		return fmt.Errorf("system: restore: end sessions: %w", err)
	}
	if n, err := r.RowsAffected(); err == nil {
		res.SessionsEnded = int(n)
	}
	if res.SafetyBackup == "" {
		res.Warnings = append(res.Warnings, "There was no database to replace, so nothing could be carried over: every "+
			"unsubscribe, bounce, suppression and GDPR erasure after the backup was taken is lost, and messages that were "+
			"queued in the backup may already have been sent. Re-apply the opt-outs you know of before resuming.")
		res.Warnings = append(res.Warnings, "API tokens revoked, users deleted and passwords or two-factor settings changed "+
			"after the backup are back as they were in the backup. Check the tokens and users (the Tokens and Users "+
			"pages of the admin UI, or GET /api/v1/tokens and /api/v1/users) and revoke or reset what should not work.")
		return nil
	}
	carried, err := reconcile(ctx, d, res.SafetyBackup, now)
	if err != nil {
		opts.Logger.Warn("system: restore: carrying over from the replaced database failed", "err", err)
		res.Warnings = append(res.Warnings, "Carrying opt-outs and sends over from the replaced database ("+res.SafetyBackup+") "+
			"failed, so unsubscribes, bounces, suppressions and erasures after the backup are lost, and queued messages may "+
			"be sent twice: "+err.Error())
		res.Warnings = append(res.Warnings, "API tokens revoked, users deleted and passwords changed after the backup are "+
			"back as they were in the backup. Check the tokens and users and revoke or reset what should not work.")
		return nil
	}
	res.CarriedOver = &carried
	res.Warnings = append(res.Warnings, "Credentials only get revoked by a restore: tokens revoked, users deleted and "+
		"passwords changed after the backup stay so, and every admin UI session was ended. Tokens and users created after "+
		"the backup are gone: create them again if they are still needed.")
	opts.Logger.Info("system: restore: carried over from the replaced database", "result", carried)
	return nil
}

// Restore restores file over the configured database; see the package
// function Restore. It refuses to run when the Service holds an open
// database: build the Service for the restore command without Deps.DB.
func (s *Service) Restore(ctx context.Context, file string) (RestoreResult, error) {
	if s.db != nil {
		return RestoreResult{}, errors.New("system: restore needs exclusive access to the database: stop the server and build the service without an open database")
	}
	return Restore(ctx, RestoreOptions{DBPath: s.cfg.DBPath(), BackupsDir: s.cfg.BackupsDir(), Clock: s.clock, Logger: s.log}, file)
}

// checkNotInUse returns ErrDatabaseInUse when another connection has the
// database at path open. In EXCLUSIVE locking mode SQLite takes an
// exclusive lock on the database file on first access, in WAL mode too, and
// every other connection to a WAL database holds a shared lock on it for as
// long as it is open, idle or not; with no busy timeout the attempt fails
// at once with SQLITE_BUSY. Other errors (a missing or damaged file) are
// left to the later steps, which handle them.
func checkNotInUse(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)")
	if err != nil {
		return nil
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	c, err := conn.Conn(ctx)
	if err != nil {
		return inUse(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		return inUse(err)
	}
	var n int
	err = c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master`).Scan(&n)
	_, _ = c.ExecContext(ctx, `ROLLBACK`)
	return inUse(err)
}

// inUse maps a SQLITE_BUSY or SQLITE_LOCKED error to ErrDatabaseInUse and
// every other outcome to nil.
func inUse(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return fmt.Errorf("system: restore: %w", ErrDatabaseInUse)
		}
	}
	return nil
}

// safetyBackup copies the current database into BackupsDir before it is
// replaced: with VACUUM INTO when the database can be opened, otherwise as
// a raw copy of the file and its WAL.
func safetyBackup(ctx context.Context, opts RestoreOptions) (string, error) {
	dest := filepath.Join(opts.BackupsDir, backupName(PreRestorePrefix, opts.Clock.Now()))
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("%s already exists", dest)
	}
	cur, err := db.Open(opts.DBPath)
	if err == nil {
		err = cur.Backup(ctx, dest)
		cur.Close()
		if err == nil {
			return dest, nil
		}
	}
	opts.Logger.Warn("system: VACUUM INTO of the current database failed; copying the raw file", "err", err)
	if err := os.MkdirAll(opts.BackupsDir, 0o700); err != nil {
		return "", err
	}
	_ = os.Remove(dest) // a partial VACUUM INTO target
	if err := copyFile(opts.DBPath, dest); err != nil {
		return "", err
	}
	if _, err := os.Stat(opts.DBPath + "-wal"); err == nil {
		if err := copyFile(opts.DBPath+"-wal", dest+"-wal"); err != nil {
			return "", err
		}
	}
	return dest, nil
}

// copyFile copies src to a new file dst (mode 0600) and syncs it.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, db.FileMode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// removeSidecars removes the -wal and -shm files of a database path.
func removeSidecars(path string) {
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

// removeDBFiles removes a database file and its sidecars.
func removeDBFiles(path string) {
	_ = os.Remove(path)
	removeSidecars(path)
}

// sameFile reports whether a and b are the same existing file.
func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}
