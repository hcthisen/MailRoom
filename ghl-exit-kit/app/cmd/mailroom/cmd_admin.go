package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"mailroom/internal/app"
	"mailroom/internal/auth"
	"mailroom/internal/config"
	"mailroom/internal/db"
)

func init() {
	register(Command{Name: "admin", Summary: "bootstrap UI users (admin create --email X [--password-stdin])", Run: runAdmin})
}

// openBootstrap loads the configuration and opens the database, for the
// commands that run next to (or before) the server: doctor, admin create
// and token create. It creates the schema of a new database, so they work
// before the first start, but never upgrades an existing one: after
// "docker compose build" of a new version, "docker compose run --rm
// mailroom doctor" runs the new binary while the old server still uses the
// database. A database with pending migrations is refused (ErrSchemaBehind);
// upgrades are left to serve and "mailroom migrate", which back up first.
func openBootstrap(ctx context.Context, env *Env) (config.Config, *db.DB, error) {
	cfg, err := config.LoadFrom(env.Getenv)
	if err != nil {
		return config.Config{}, nil, err
	}
	d, err := db.Open(cfg.DBPath())
	if err != nil {
		return config.Config{}, nil, err
	}
	if err := migrateNewOnly(ctx, d, cfg.BackupsDir()); err != nil {
		d.Close()
		return config.Config{}, nil, err
	}
	return cfg, d, nil
}

// ErrSchemaBehind means the database needs migrations this binary has but
// the command doesn't apply.
var ErrSchemaBehind = errors.New("the database schema is older than this version of mailroom")

// migrateNewOnly applies the migrations to a database without a schema
// (version 0) and refuses one that has a schema with migrations pending.
func migrateNewOnly(ctx context.Context, d *db.DB, backupsDir string) error {
	all, err := migrations()
	if err != nil {
		return err
	}
	pending, err := d.PendingFrom(ctx, all)
	if err != nil || len(pending) == 0 {
		return err
	}
	current, err := d.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if current > 0 {
		return fmt.Errorf("%w (schema v%d, %d migration(s) pending): start the new version (docker compose up -d) "+
			"or run \"mailroom migrate\" first; both back up the database before migrating", ErrSchemaBehind, current, len(pending))
	}
	_, err = migrate(ctx, d, backupsDir, nil)
	return err
}

// cliCtx is ctx attributed to the CLI in the audit log.
func cliCtx(ctx context.Context) context.Context { return app.CLIContext(ctx) }

// adminCreateResult is the --json output of "admin create".
type adminCreateResult struct {
	User     auth.User `json:"user"`
	Password string    `json:"password,omitempty"`
}

func runAdmin(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return &ExitCodeError{Code: ExitError, Err: errors.New("usage: mailroom admin create --email ADDRESS [--password-stdin] [--json]")}
	}
	fs, jsonOut := newFlagSet(env, "admin create")
	email := fs.String("email", "", "login email of the new user (required)")
	fromStdin := fs.Bool("password-stdin", false, "read the password from the first line of stdin instead of generating one")
	if err := fs.Parse(args[1:]); err != nil {
		return &ExitCodeError{Code: ExitError}
	}
	if *email == "" {
		return &ExitCodeError{Code: ExitError, Err: errors.New("--email is required")}
	}
	var password string
	if *fromStdin {
		pw, err := readLine(env.Stdin)
		if err != nil {
			return err
		}
		if pw == "" {
			return errors.New("--password-stdin: no password on stdin")
		}
		password = pw
	}
	_, d, err := openBootstrap(ctx, env)
	if err != nil {
		return err
	}
	defer d.Close()

	u, generated, err := app.CreateAdmin(ctx, d, *email, password)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("created user %s (id %d)", u.Email, u.ID)
	if generated != "" {
		text += "\npassword: " + generated + "\n(shown only once; store it in a password manager)"
	}
	return printResult(env, *jsonOut, adminCreateResult{User: u, Password: generated}, text)
}

// readLine returns the first line of r without its line ending.
func readLine(r io.Reader) (string, error) {
	if r == nil {
		return "", nil
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
