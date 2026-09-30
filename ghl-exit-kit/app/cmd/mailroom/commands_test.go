package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"mailroom/internal/app"
	"mailroom/internal/config"
	"mailroom/internal/provision"
	"mailroom/internal/system"
	"mailroom/internal/testutil"
)

// offline makes doctor and backup use no network clients at all.
func offline(t *testing.T) {
	t.Helper()
	orig := newIntegrations
	t.Cleanup(func() { newIntegrations = orig })
	newIntegrations = func(context.Context, config.Config) (integrations, error) { return integrations{}, nil }
}

// fakeSES answers GetEmailIdentity with three DKIM tokens.
type fakeSES struct{ provision.SESAPI }

func (fakeSES) GetEmailIdentity(context.Context, *sesv2.GetEmailIdentityInput, ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error) {
	return &sesv2.GetEmailIdentityOutput{DkimAttributes: &types.DkimAttributes{Tokens: []string{"t1", "t2", "t3"}}}, nil
}

func withProvisionClients(t *testing.T, fn func(context.Context, func(string) string) (provision.Clients, error)) {
	t.Helper()
	orig := provisionCLI
	t.Cleanup(func() { provisionCLI = orig })
	provisionCLI = &provision.CLI{NewClients: fn}
}

// TestProvisionCommandsNeedNoDatabase: provision and dns read only the
// environment; they never create the data directory's database, and with
// --json stdout holds exactly one JSON document, also on failure.
func TestProvisionCommandsNeedNoDatabase(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{"MAILROOM_DATA_DIR": dir, "DOMAIN": "example.com", "AWS_REGION": "eu-central-1"}
	withProvisionClients(t, func(context.Context, func(string) string) (provision.Clients, error) {
		return provision.Clients{SES: fakeSES{}, Region: "eu-central-1"}, nil
	})

	env, out, errOut := testEnv(vars)
	if code := run(context.Background(), []string{"dns", "records", "--ipv4", "192.0.2.10", "--json"}, env); code != ExitOK {
		t.Fatalf("dns records exit = %d: %s", code, errOut)
	}
	var recs struct {
		Records []provision.Record `json:"records"`
	}
	if err := json.Unmarshal(out.Bytes(), &recs); err != nil || len(recs.Records) < 5 {
		t.Fatalf("records = %s (%v)", out, err)
	}

	withProvisionClients(t, func(context.Context, func(string) string) (provision.Clients, error) {
		return provision.Clients{}, errors.New("no AWS credentials in this test")
	})
	env, out, _ = testEnv(vars)
	if code := run(context.Background(), []string{"provision", "ses", "--json"}, env); code != ExitError {
		t.Fatalf("provision ses exit = %d", code)
	}
	var fail map[string]any
	if err := json.Unmarshal(out.Bytes(), &fail); err != nil || fail["error"] == nil {
		t.Fatalf("provision --json failure = %s (%v)", out, err)
	}
	env, _, _ = testEnv(vars)
	if code := run(context.Background(), []string{"provision"}, env); code != ExitError {
		t.Fatalf("bare provision exit = %d", code)
	}
	env, _, _ = testEnv(vars)
	if code := run(context.Background(), []string{"dns", "bogus"}, env); code != ExitError {
		t.Fatalf("dns bogus exit = %d", code)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("data dir touched: %v %v", entries, err)
	}
}

// TestDoctorCommand runs the doctor offline: it creates and seeds the
// database like a first start, prints every check, and exits 2 because the
// AWS checks fail without clients.
func TestDoctorCommand(t *testing.T) {
	offline(t)
	vars := bootstrapVars(t)
	env, out, errOut := testEnv(vars)
	if code := run(context.Background(), []string{"doctor", "--json"}, env); code != ExitCheckFailed {
		t.Fatalf("exit = %d: %s", code, errOut)
	}
	var rep system.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("report %s: %v", out, err)
	}
	if rep.Status != system.StatusFail || len(rep.Checks) != len(system.CheckNames()) {
		t.Fatalf("report = %+v", rep)
	}
	byName := map[string]system.Check{}
	for _, c := range rep.Checks {
		byName[c.Name] = c
	}
	if c := byName[system.CheckSESIdentity]; c.Status != system.StatusFail || c.FixHint == "" {
		t.Fatalf("ses check = %+v", c)
	}
	if c := byName[system.CheckConfig]; c.Status == system.StatusFail {
		t.Fatalf("config check = %+v (settings not seeded?)", c)
	}
	d := openTestDB(t, vars)
	if n := testutil.Count(t, d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'settings.seed'`); n != 1 {
		t.Fatalf("settings seeded %d times", n)
	}

	env, out, _ = testEnv(vars)
	if code := run(context.Background(), []string{"doctor"}, env); code != ExitCheckFailed || !strings.Contains(out.String(), "overall: FAIL") {
		t.Fatalf("text doctor = %d %q", code, out.String())
	}
}

// TestBackupAndRestoreCommands: an offline backup is a valid database
// that restore accepts, restore keeps a safety copy, and both refuse bad
// input.
func TestBackupAndRestoreCommands(t *testing.T) {
	offline(t)
	vars := bootstrapVars(t)
	env, _, errOut := testEnv(vars)
	if code := run(context.Background(), []string{"backup"}, env); code != ExitError || !strings.Contains(errOut.String(), "no database") {
		t.Fatalf("backup without a database = %d %q", code, errOut)
	}
	if code := run(context.Background(), []string{"admin", "create", "--email", "owner@example.com"}, env); code != ExitOK {
		t.Fatalf("admin create failed: %s", errOut)
	}

	env, out, errOut := testEnv(vars)
	if code := run(context.Background(), []string{"backup", "--json"}, env); code != ExitOK {
		t.Fatalf("backup exit = %d: %s", code, errOut)
	}
	var b system.BackupResult
	if err := json.Unmarshal(out.Bytes(), &b); err != nil || b.Path == "" {
		t.Fatalf("backup = %s (%v)", out, err)
	}
	if _, err := os.Stat(b.Path); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(b.Path) != filepath.Join(vars["MAILROOM_DATA_DIR"], "backups") {
		t.Fatalf("backup path = %s", b.Path)
	}

	// A user created after the backup disappears with the restore.
	env, _, errOut = testEnv(vars)
	if code := run(context.Background(), []string{"admin", "create", "--email", "later@example.com"}, env); code != ExitOK {
		t.Fatalf("second admin: %s", errOut)
	}
	env, out, errOut = testEnv(vars)
	if code := run(context.Background(), []string{"restore", b.Path, "--json"}, env); code != ExitOK {
		t.Fatalf("restore exit = %d: %s", code, errOut)
	}
	var r system.RestoreResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.SafetyBackup == "" || r.SchemaVersion == 0 ||
		!r.SendingPaused || r.CarriedOver == nil || len(r.Warnings) == 0 {
		t.Fatalf("restore = %s (%v)", out, err)
	}
	d := openTestDB(t, vars)
	if n := testutil.Count(t, d.Read, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("users after restore = %d", n)
	}
	// Sending stays paused until someone resumes it on purpose.
	if n := testutil.Count(t, d.Read, `SELECT count(*) FROM settings WHERE key = 'sending_state'
		AND json_extract(value, '$.paused') AND json_extract(value, '$.source') = 'manual'`); n != 1 {
		t.Fatal("the restored database doesn't have sending paused")
	}
	d.Close()
	// The plain-text output says so. (The same second's safety backup name
	// is taken: move it away.)
	if err := os.Rename(r.SafetyBackup, r.SafetyBackup+".first"); err != nil {
		t.Fatal(err)
	}
	env, out, errOut = testEnv(vars)
	if code := run(context.Background(), []string{"restore", b.Path}, env); code != ExitOK {
		t.Fatalf("restore exit = %d: %s", code, errOut)
	}
	if !strings.Contains(out.String(), "WARNING: All sending is paused") || !strings.Contains(out.String(), "carried over from the replaced database") {
		t.Fatalf("restore output = %s", out)
	}

	for _, args := range [][]string{
		{"restore"},
		{"restore", "a.db", "b.db"},
		{"restore", filepath.Join(t.TempDir(), "missing.db")},
		{"backup", "extra"},
	} {
		env, _, _ := testEnv(vars)
		if code := run(context.Background(), args, env); code != ExitError {
			t.Errorf("%v: exit = %d", args, code)
		}
	}
}

// TestServeCommand starts the real server on a free port with fakes
// injected, checks /healthz and the API, and shuts it down by cancelling
// the context.
func TestServeCommand(t *testing.T) {
	vars := bootstrapVars(t)
	vars["HTTP_ONLY"] = "1"
	vars["HTTP_ADDR"] = "127.0.0.1:0"
	vars["BASE_URL"] = "http://localhost:8080"

	built := make(chan *app.App, 1)
	orig := newApp
	t.Cleanup(func() { newApp = orig })
	newApp = func(ctx context.Context, cfg config.Config, opts app.Options) (*app.App, error) {
		opts.Offline, opts.Provider = true, testutil.NewFakeProvider()
		a, err := orig(ctx, cfg, opts)
		if err == nil {
			built <- a
		}
		return a, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env, _, errOut := testEnv(vars)
	env.Stdout = io.Discard
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve"}, env) }()

	var a *app.App
	select {
	case a = <-built:
	case code := <-done:
		t.Fatalf("serve exited early: %d %s", code, errOut)
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not start")
	}
	var addr string
	for deadline := time.Now().Add(10 * time.Second); addr == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if as := a.Server.Addrs(); len(as) > 0 {
			addr = as[0].String()
		}
	}
	if addr == "" {
		t.Fatal("server is not listening")
	}
	for path, want := range map[string]int{"/healthz": 200, "/api/v1/system/health": 401, "/api/v1/openapi.json": 200} {
		resp, err := http.Get(fmt.Sprintf("http://%s%s", addr, path))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	cancel()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("serve exit = %d: %s", code, errOut)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not stop")
	}
}
