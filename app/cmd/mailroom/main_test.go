package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mailroom/internal/app"
	"mailroom/internal/db"
)

func testEnv(vars map[string]string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return &Env{Stdout: &out, Stderr: &errOut, Getenv: func(k string) string { return vars[k] }}, &out, &errOut
}

func TestVersion(t *testing.T) {
	env, out, _ := testEnv(nil)
	if code := run(context.Background(), []string{"version"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(out.String()) != "mailroom dev" {
		t.Fatalf("output = %q", out.String())
	}
	env, out, _ = testEnv(nil)
	if code := run(context.Background(), []string{"version", "--json"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var v map[string]string
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || v["version"] != "dev" {
		t.Fatalf("json = %s (%v)", out.String(), err)
	}
}

func TestUnknownCommandAndRegistry(t *testing.T) {
	env, _, errOut := testEnv(nil)
	if code := run(context.Background(), []string{"frobnicate"}, env); code != ExitError {
		t.Fatalf("unknown exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	for _, name := range []string{"serve", "migrate", "version", "admin", "token", "doctor", "provision", "dns", "backup", "restore"} {
		if _, ok := lookup(name); !ok {
			t.Errorf("command %q not registered", name)
		}
	}
	env, out, _ := testEnv(nil)
	if code := run(context.Background(), []string{"help"}, env); code != ExitOK || !strings.Contains(out.String(), "migrate") {
		t.Fatalf("help = %d %q", code, out.String())
	}
}

func TestMigrateCommand(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{"MAILROOM_DATA_DIR": dir, "DOMAIN": "example.com"}

	env, out, errOut := testEnv(vars)
	if code := run(context.Background(), []string{"migrate", "--json"}, env); code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	var res app.MigrateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if all, _ := db.Migrations(); len(res.Applied) != len(all) || res.Version != embeddedLatest(t) || res.Backup != "" {
		t.Fatalf("first migrate = %+v (a fresh DB needs no backup)", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "mailroom.db")); err != nil {
		t.Fatal(err)
	}

	env, out, _ = testEnv(vars)
	if code := run(context.Background(), []string{"migrate"}, env); code != ExitOK {
		t.Fatalf("second exit = %d", code)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("second migrate = %q", out.String())
	}
}

func TestMigrateConfigError(t *testing.T) {
	env, _, errOut := testEnv(map[string]string{"MAILROOM_DATA_DIR": t.TempDir()})
	if code := run(context.Background(), []string{"migrate"}, env); code != ExitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "DOMAIN") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

// withUpgrade makes migrate see one more migration than is embedded.
func withUpgrade(t *testing.T) {
	t.Helper()
	orig := migrations
	t.Cleanup(func() { migrations = orig })
	migrations = func() ([]db.Migration, error) {
		all, err := orig()
		return append(all, db.Migration{Version: probeVersion, Name: "0900_probe.sql", SQL: `CREATE TABLE upgrade_probe (id INTEGER PRIMARY KEY);`}), err
	}
}

// probeVersion is the version of withUpgrade's extra migration.
const probeVersion = 900

// embeddedLatest is the highest embedded migration version: the schema of
// a database the binary migrated.
func embeddedLatest(t *testing.T) int {
	t.Helper()
	all, err := db.Migrations()
	if err != nil || len(all) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	return all[len(all)-1].Version
}

func migrateJSON(t *testing.T, vars map[string]string) (app.MigrateResult, int, string) {
	t.Helper()
	env, out, errOut := testEnv(vars)
	code := run(context.Background(), []string{"migrate", "--json"}, env)
	var res app.MigrateResult
	if code == ExitOK {
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("output %q: %v", out.String(), err)
		}
	}
	return res, code, errOut.String()
}

func schemaVersion(t *testing.T, path string) (int, bool) {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	v, err := d.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Read.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'upgrade_probe'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return v, n == 1
}

// TestMigrateBacksUpBeforeUpgrade covers PRD O-6: an existing database is
// backed up before pending migrations run, and the backup has the old schema.
func TestMigrateBacksUpBeforeUpgrade(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{"MAILROOM_DATA_DIR": dir, "DOMAIN": "example.com"}
	if _, code, stderr := migrateJSON(t, vars); code != ExitOK {
		t.Fatalf("initial migrate: %s", stderr)
	}
	withUpgrade(t)
	res, code, stderr := migrateJSON(t, vars)
	if code != ExitOK {
		t.Fatalf("upgrade: %s", stderr)
	}
	if len(res.Applied) != 1 || res.Applied[0] != "0900_probe.sql" || res.Version != probeVersion || res.Backup == "" {
		t.Fatalf("upgrade = %+v", res)
	}
	if filepath.Dir(res.Backup) != filepath.Join(dir, "backups") || !strings.HasPrefix(filepath.Base(res.Backup), fmt.Sprintf("pre-migrate-v%d-", embeddedLatest(t))) {
		t.Fatalf("backup path = %s", res.Backup)
	}
	if v, probe := schemaVersion(t, res.Backup); v != embeddedLatest(t) || probe {
		t.Fatalf("backup schema = v%d (probe table %v), want the old one", v, probe)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(res.Backup); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("backup mode = %v, %v", fi.Mode().Perm(), err)
		}
	}
	if v, probe := schemaVersion(t, filepath.Join(dir, "mailroom.db")); v != probeVersion || !probe {
		t.Fatalf("live schema = v%d (probe table %v)", v, probe)
	}
}

func TestMigrateAbortsWhenBackupFails(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{"MAILROOM_DATA_DIR": dir, "DOMAIN": "example.com"}
	if _, code, stderr := migrateJSON(t, vars); code != ExitOK {
		t.Fatalf("initial migrate: %s", stderr)
	}
	// A file where the backups directory should be makes the backup fail.
	if err := os.WriteFile(filepath.Join(dir, "backups"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withUpgrade(t)
	if _, code, stderr := migrateJSON(t, vars); code == ExitOK || !strings.Contains(stderr, "backup before migration") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if v, probe := schemaVersion(t, filepath.Join(dir, "mailroom.db")); v != embeddedLatest(t) || probe {
		t.Fatalf("schema after failed backup = v%d (probe %v), want it untouched", v, probe)
	}
}
