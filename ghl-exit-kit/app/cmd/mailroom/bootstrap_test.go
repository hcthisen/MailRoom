package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailroom/internal/auth"
	"mailroom/internal/db"
)

func bootstrapVars(t *testing.T) map[string]string {
	return map[string]string{
		"MAILROOM_DATA_DIR": t.TempDir(),
		"DOMAIN":            "example.com",
		"FROM_EMAIL":        "news@example.com",
		"OWNER_EMAIL":       "owner@gmail.com",
		"COMPANY_FOOTER":    "Example Ltd, 1 Street, Town",
		"MAILROOM_SECRET":   "0123456789abcdef0123456789abcdef",
	}
}

func TestAdminCreate(t *testing.T) {
	vars := bootstrapVars(t)
	env, out, errOut := testEnv(vars)
	if code := run(context.Background(), []string{"admin", "create", "--email", "Owner@Example.com", "--json"}, env); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, errOut)
	}
	var res adminCreateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.User.Email != "owner@example.com" || len(res.Password) != 24 {
		t.Fatalf("result = %+v", res)
	}

	env, out, errOut = testEnv(vars)
	env.Stdin = strings.NewReader("a chosen long password\n")
	if code := run(context.Background(), []string{"admin", "create", "--email", "editor@example.com", "--password-stdin"}, env); code != ExitOK {
		t.Fatalf("stdin exit = %d: %s", code, errOut)
	}
	if strings.Contains(out.String(), "password") {
		t.Fatalf("chosen password echoed: %q", out.String())
	}

	// The chosen password works.
	d := openTestDB(t, vars)
	svc := auth.New(auth.Deps{DB: d})
	if _, err := svc.Login(context.Background(), auth.LoginInput{Email: "editor@example.com", Password: "a chosen long password"}); err != nil {
		t.Fatalf("login = %v", err)
	}

	for _, args := range [][]string{
		{"admin", "create", "--email", "owner@example.com"}, // duplicate
		{"admin", "create"}, // no email
		{"admin"},           // no subcommand
		{"admin", "create", "--email", "x@example.com", "--password-stdin"}, // empty stdin
	} {
		env, _, _ := testEnv(vars)
		if code := run(context.Background(), args, env); code != ExitError {
			t.Errorf("%v: exit = %d", args, code)
		}
	}
}

func TestTokenCreate(t *testing.T) {
	vars := bootstrapVars(t)
	env, out, errOut := testEnv(vars)
	if code := run(context.Background(), []string{"token", "create", "--name", "setup-agent", "--scope", "admin", "--json"}, env); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, errOut)
	}
	var res tokenCreateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Secret, "mr_") || res.Token.Name != "setup-agent" || res.Token.CreatedBy != "cli" {
		t.Fatalf("result = %+v", res)
	}

	env, out, _ = testEnv(vars)
	if code := run(context.Background(), []string{"token", "create", "--name", "reports", "--scope", "read", "--expires-in", "30d"}, env); code != ExitOK {
		t.Fatalf("text exit = %d", code)
	}
	secret := strings.TrimSpace(out.String())

	d := openTestDB(t, vars)
	svc := auth.New(auth.Deps{DB: d})
	for s, scope := range map[string]string{res.Secret: "admin", secret: "read"} {
		p, err := svc.AuthenticateToken(context.Background(), s)
		if err != nil || string(p.Scope) != scope {
			t.Fatalf("authenticate %s = %+v %v", scope, p, err)
		}
	}
	tok, _ := svc.ListTokens(context.Background(), 0, 10, false)
	if len(tok) != 2 || tok[1].ExpiresAt == nil {
		t.Fatalf("tokens = %+v", tok)
	}

	for _, args := range [][]string{
		{"token", "create", "--name", "x"},                                            // no scope
		{"token", "create", "--scope", "admin"},                                       // no name
		{"token", "create", "--name", "x", "--scope", "root"},                         // bad scope
		{"token", "create", "--name", "x", "--scope", "read", "--expires-in", "soon"}, // bad lifetime
		{"token", "create", "--name", "setup-agent", "--scope", "read"},               // duplicate
	} {
		env, _, _ := testEnv(vars)
		if code := run(context.Background(), args, env); code != ExitError {
			t.Errorf("%v: exit = %d", args, code)
		}
	}
}

// openTestDB opens (and migrates) the database of a bootstrap test.
func openTestDB(t *testing.T, vars map[string]string) *db.DB {
	t.Helper()
	env, _, _ := testEnv(vars)
	_, d, err := openBootstrap(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestBootstrapCommandsNeverUpgrade: doctor, admin create and token create
// create the schema of a new database but refuse one whose schema is
// behind the binary (a new image next to the old server), and leave it
// untouched; migrate then upgrades it and they work again.
func TestBootstrapCommandsNeverUpgrade(t *testing.T) {
	vars := bootstrapVars(t)
	env, _, errOut := testEnv(vars)
	if code := run(context.Background(), []string{"admin", "create", "--email", "owner@example.com"}, env); code != ExitOK {
		t.Fatalf("admin create on a new database: exit %d: %s", code, errOut)
	}
	dbPath := filepath.Join(vars["MAILROOM_DATA_DIR"], "mailroom.db")
	withUpgrade(t)
	for _, args := range [][]string{
		{"doctor"},
		{"admin", "create", "--email", "editor@example.com"},
		{"token", "create", "--name", "x", "--scope", "read"},
	} {
		env, _, errOut := testEnv(vars)
		if code := run(context.Background(), args, env); code != ExitError {
			t.Fatalf("%v: exit = %d, want %d", args, code, ExitError)
		}
		if !strings.Contains(errOut.String(), "mailroom migrate") {
			t.Fatalf("%v: stderr = %q", args, errOut)
		}
		if v, probe := schemaVersion(t, dbPath); v != embeddedLatest(t) || probe {
			t.Fatalf("%v migrated the database: v%d probe %v", args, v, probe)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(vars["MAILROOM_DATA_DIR"], "backups")); len(entries) != 0 {
		t.Fatalf("backups written: %v", entries)
	}
	if _, code, stderr := migrateJSON(t, vars); code != ExitOK {
		t.Fatalf("migrate: %s", stderr)
	}
	env, _, errOut = testEnv(vars)
	if code := run(context.Background(), []string{"token", "create", "--name", "x", "--scope", "read"}, env); code != ExitOK {
		t.Fatalf("token create after migrate: exit %d: %s", code, errOut)
	}
}
