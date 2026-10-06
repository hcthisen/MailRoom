package system

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
	"mailroom/internal/testutil"
)

const (
	adminToken = api.TokenPrefix + "admin"
	readToken  = api.TokenPrefix + "read"
)

// fakeAuth accepts the two fixed test tokens.
type fakeAuth struct{}

func (fakeAuth) AuthenticateToken(_ context.Context, tok string) (api.Principal, error) {
	switch tok {
	case adminToken:
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "1", Name: "setup-agent"}, Scope: api.ScopeAdmin}, nil
	case readToken:
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "2", Name: "reporting"}, Scope: api.ScopeRead}, nil
	}
	return api.Principal{}, fmt.Errorf("unknown token: %w", api.ErrUnauthorized)
}

func (fakeAuth) AuthenticateSession(context.Context, string) (api.Principal, string, error) {
	return api.Principal{}, "", api.ErrUnauthorized
}

type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

func (r apiResponse) code() string {
	var e struct{ Error api.ErrorBody }
	_ = json.Unmarshal(r.body, &e)
	return e.Error.Code
}

func (r apiResponse) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

type apiClient struct {
	t *testing.T
	h http.Handler
}

func (c apiClient) do(method, path, token string, headers ...string) apiResponse {
	c.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "198.51.100.7:5555"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return apiResponse{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

func newAPI(t *testing.T, f *fixture, s *Service) apiClient {
	t.Helper()
	a, err := api.New(api.Config{DB: f.d, Clock: f.clock, BaseURL: f.cfg.BaseURL, Auth: fakeAuth{}, Logger: quietLogger(), Rate: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	s.RegisterAPI(a)
	f.runner.RegisterAPI(a)
	return apiClient{t: t, h: a.Handler()}
}

func TestAPIRoutes(t *testing.T) {
	f := newFixture(t)
	a, err := api.New(api.Config{DB: f.d, Clock: f.clock, Auth: fakeAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	f.service().RegisterAPI(a)
	want := map[string]api.Scope{
		"get-system-health": api.ScopeRead, "get-system-version": api.ScopeRead, "run-doctor": api.ScopeRead,
		"get-ses-status": api.ScopeRead, "list-backups": api.ScopeRead, "create-backup": api.ScopeAdmin,
		"download-backup": api.ScopeAdmin,
	}
	got := map[string]api.Scope{}
	for _, r := range a.Routes() {
		got[r.OperationID] = r.Scope
		if len(r.Tags) != 1 || r.Tags[0] != "System" || !strings.HasPrefix(r.Path, "/api/v1/system/") {
			t.Errorf("%s: tags %v path %s", r.OperationID, r.Tags, r.Path)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %v", got)
	}
	for id, scope := range want {
		if got[id] != scope {
			t.Errorf("%s scope = %q, want %q", id, got[id], scope)
		}
	}
}

func TestAPIAuthAndErrors(t *testing.T) {
	f := newFixture(t)
	c := newAPI(t, f, f.service())
	r := c.do("GET", "/api/v1/system/health", "")
	if r.status != 401 || r.code() != api.CodeUnauthorized {
		t.Fatalf("anonymous = %d %s", r.status, r.body)
	}
	if r := c.do("GET", "/api/v1/system/health", api.TokenPrefix+"bogus"); r.status != 401 {
		t.Fatalf("bad token = %d", r.status)
	}
	if r := c.do("DELETE", "/api/v1/system/backups", adminToken); r.status != 404 && r.status != 405 {
		t.Fatalf("unknown method = %d %s", r.status, r.body)
	}
}

func TestAPIHealthAndVersion(t *testing.T) {
	f := newFixture(t)
	now := db.FormatTime(f.clock.Now())
	testutil.Exec(t, f.d, `INSERT INTO messages (kind, to_email, status, next_attempt_at, queued_at, updated_at) VALUES
		('test', 'a@example.org', 'queued', ?, ?, ?), ('test', 'b@example.org', 'queued', ?, ?, ?), ('test', 'c@example.org', 'sent', ?, ?, ?)`,
		now, now, now, db.FormatTime(f.clock.Now().Add(time.Hour)), now, now, now, now, now)
	if err := f.settings.PutRaw(f.ctx, f.d.Write, SettingSendingState, map[string]any{"paused": true, "reason": "ses_paused"}); err != nil {
		t.Fatal(err)
	}
	c := newAPI(t, f, f.service())

	r := c.do("GET", "/api/v1/system/health", readToken)
	if r.status != 200 {
		t.Fatalf("health = %d %s", r.status, r.body)
	}
	var h Health
	r.decode(t, &h)
	if h.Status != "ok" || h.Database != "ok" || h.SchemaVersion != latestSchema() || h.SchemaLatest != latestSchema() ||
		h.QueueDepth != 2 || h.QueueDue != 1 || h.Version != "1.2.3" || h.UptimeSeconds != 3600 {
		t.Fatalf("health = %+v", h)
	}
	var state map[string]any
	if err := json.Unmarshal(h.SendingState, &state); err != nil || state["paused"] != true {
		t.Fatalf("sending_state = %s", h.SendingState)
	}

	r = c.do("GET", "/api/v1/system/version", readToken)
	var v VersionInfo
	r.decode(t, &v)
	if r.status != 200 || v.Version != "1.2.3" || !strings.HasPrefix(v.GoVersion, "go") || v.UptimeSeconds != 3600 || v.SchemaLatest != latestSchema() {
		t.Fatalf("version = %d %+v", r.status, v)
	}
}

func TestHealthStates(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	h := s.Health(f.ctx)
	if h.Status != "ok" || string(h.SendingState) != "null" {
		t.Fatalf("health = %+v (%s)", h, h.SendingState)
	}
	if err := s.Liveness(f.ctx); err != nil {
		t.Fatal(err)
	}
	// A pending migration is "degraded".
	testutil.Exec(t, f.d, `DELETE FROM schema_migrations`)
	if h := s.Health(f.ctx); h.Status != "degraded" || h.SchemaVersion != 0 {
		t.Fatalf("health = %+v", h)
	}
	// An unreachable database is "fail".
	f.d.Close()
	if h := s.Health(f.ctx); h.Status != "fail" || h.Database == "ok" {
		t.Fatalf("health = %+v", h)
	}
	if err := s.Liveness(f.ctx); err == nil {
		t.Fatal("liveness ok with a closed database")
	}
	s.db = nil
	if h := s.Health(f.ctx); h.Status != "fail" {
		t.Fatalf("health = %+v", h)
	}
}

func TestAPIDoctor(t *testing.T) {
	f := healthyFixture(t)
	c := newAPI(t, f, f.service())
	r := c.do("GET", "/api/v1/system/doctor", readToken)
	if r.status != 200 {
		t.Fatalf("doctor = %d %s", r.status, r.body)
	}
	var rep Report
	r.decode(t, &rep)
	if rep.Status != StatusOK || len(rep.Checks) != len(CheckNames()) {
		t.Fatalf("report = %+v", rep)
	}

	f.ses.account.ProductionAccessEnabled = false
	r = c.do("GET", "/api/v1/system/doctor", readToken)
	r.decode(t, &rep)
	if rep.Status != StatusWarn || findCheck(t, rep, CheckSESProduction).FixHint == "" {
		t.Fatalf("sandbox report = %s", r.body)
	}
}

func TestAPISESStatus(t *testing.T) {
	f := newFixture(t)
	s := f.service()
	c := newAPI(t, f, s)
	var st SESStatus

	// No cache: read live.
	r := c.do("GET", "/api/v1/system/ses", readToken)
	r.decode(t, &st)
	if r.status != 200 || st.Source != "live" || st.Account.Max24HourSend != 50000 || st.Account.EnforcementStatus != "HEALTHY" ||
		!st.Account.FetchedAt.Equal(f.clock.Now()) || st.QuotaUsed != 1200.0/50000 {
		t.Fatalf("live = %d %s", r.status, r.body)
	}

	// Fresh cache written by the sender's poller.
	cached := SESAccount{ProductionAccessEnabled: true, SendingEnabled: false, Max24HourSend: 200, SentLast24Hours: 50, FetchedAt: f.clock.Now().Add(-5 * time.Minute)}
	if err := f.settings.PutRaw(f.ctx, f.d.Write, SettingSESAccount, cached); err != nil {
		t.Fatal(err)
	}
	r = c.do("GET", "/api/v1/system/ses", readToken)
	r.decode(t, &st)
	if st.Source != "cache" || st.Account.SendingEnabled || st.QuotaUsed != 0.25 {
		t.Fatalf("cached = %s", r.body)
	}
	calls := f.ses.callCount("GetAccount")

	// refresh=true reads SES.
	r = c.do("GET", "/api/v1/system/ses?refresh=true", readToken)
	r.decode(t, &st)
	if st.Source != "live" || f.ses.callCount("GetAccount") != calls+1 {
		t.Fatalf("refresh = %s", r.body)
	}

	// A stale cache is refreshed; when SES fails the stale copy is served.
	f.clock.Advance(time.Hour)
	f.ses.accountErr = errBoom
	r = c.do("GET", "/api/v1/system/ses", readToken)
	r.decode(t, &st)
	if r.status != 200 || st.Source != "cache" {
		t.Fatalf("stale with failing SES = %d %s", r.status, r.body)
	}

	// No cache and SES failing: 500 with the error format.
	testutil.Exec(t, f.d, `DELETE FROM settings WHERE key = ?`, SettingSESAccount)
	if r := c.do("GET", "/api/v1/system/ses", readToken); r.status != 500 || r.code() != api.CodeInternal || strings.Contains(string(r.body), "boom") {
		t.Fatalf("SES error = %d %s", r.status, r.body)
	}

	// No AWS at all: 409 conflict.
	f.ses = nil
	c = newAPI(t, f, f.service())
	if r := c.do("GET", "/api/v1/system/ses", readToken); r.status != 409 || r.code() != api.CodeConflict || !strings.Contains(string(r.body), "provision ses") {
		t.Fatalf("no AWS = %d %s", r.status, r.body)
	}
}

func TestAPIBackups(t *testing.T) {
	f := newFixture(t)
	setTimezone(t, f, "Europe/Copenhagen")
	f.cfg.BackupBucket = "mailroom-backup-example"
	s := f.service()
	s.RegisterJobs()
	c := newAPI(t, f, s)

	// Empty list with the schedule.
	var list BackupList
	r := c.do("GET", "/api/v1/system/backups", readToken)
	r.decode(t, &list)
	if r.status != 200 || list.Items == nil || len(list.Items) != 0 || list.NextCursor != "" {
		t.Fatalf("list = %d %s", r.status, r.body)
	}
	if !list.Schedule.NextRunAt.Equal(time.Date(2026, 1, 6, 1, 30, 0, 0, time.UTC)) || list.Schedule.LocalTime != "02:30" ||
		list.Schedule.Timezone != "Europe/Copenhagen" || list.Schedule.RetentionDays != 14 || list.Schedule.S3Bucket != "mailroom-backup-example" {
		t.Fatalf("schedule = %+v", list.Schedule)
	}

	// Creating needs admin.
	if r := c.do("POST", "/api/v1/system/backups", readToken); r.status != 403 || r.code() != api.CodeForbidden {
		t.Fatalf("read POST = %d %s", r.status, r.body)
	}

	// Dry run: 202 with the would-be job, nothing stored.
	r = c.do("POST", "/api/v1/system/backups?dry_run=true", adminToken)
	var accepted jobs.AcceptedBody
	r.decode(t, &accepted)
	if r.status != 202 || accepted.Job.Type != JobTypeBackup || accepted.Job.ID != 0 {
		t.Fatalf("dry run = %d %s", r.status, r.body)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("dry run stored %d jobs", n)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'system.backup'`); n != 0 {
		t.Fatalf("dry run stored %d audit rows", n)
	}

	// Real request: 202 {"job": ...}, with an audit record naming the actor.
	r = c.do("POST", "/api/v1/system/backups", adminToken, api.IdempotencyHeader, "backup-1")
	r.decode(t, &accepted)
	if r.status != 202 || accepted.Job.ID == 0 || accepted.Job.Status != jobs.StatusQueued || accepted.Job.CreatedBy != "token:1" {
		t.Fatalf("create = %d %s", r.status, r.body)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'system.backup' AND actor_name = 'setup-agent'`); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	// Idempotent retry: same job, no second one.
	r = c.do("POST", "/api/v1/system/backups", adminToken, api.IdempotencyHeader, "backup-1")
	var again jobs.AcceptedBody
	r.decode(t, &again)
	if r.status != 202 || again.Job.ID != accepted.Job.ID || testutil.Count(t, f.d.Read, `SELECT count(*) FROM jobs`) != 1 {
		t.Fatalf("retry = %d %s", r.status, r.body)
	}

	// Run it and poll the job through the API.
	startRunner(t, f.runner)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	if j, err := f.runner.Wait(ctx, accepted.Job.ID); err != nil || j.Status != jobs.StatusSucceeded {
		t.Fatalf("job = %+v, %v", j, err)
	}
	r = c.do("GET", fmt.Sprintf("/api/v1/jobs/%d", accepted.Job.ID), readToken)
	var job jobs.Job
	r.decode(t, &job)
	var res BackupResult
	if err := json.Unmarshal(job.Result, &res); err != nil || res.S3Key != "backups/"+res.Backup.Name || len(f.s3.puts) != 1 {
		t.Fatalf("job result = %s", job.Result)
	}

	// Listed.
	r = c.do("GET", "/api/v1/system/backups", readToken)
	r.decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].Name != res.Backup.Name || list.Items[0].SizeBytes == 0 {
		t.Fatalf("list = %s", r.body)
	}

	// Download: admin only, the exact bytes, as an attachment.
	path := "/api/v1/system/backups/" + res.Backup.Name
	if r := c.do("GET", path, readToken); r.status != 403 || r.code() != api.CodeForbidden {
		t.Fatalf("read download = %d %s", r.status, r.body)
	}
	r = c.do("GET", path, adminToken)
	want, _ := os.ReadFile(filepath.Join(f.cfg.BackupsDir(), res.Backup.Name))
	if r.status != 200 || string(r.body) != string(want) {
		t.Fatalf("download = %d, %d bytes (want %d)", r.status, len(r.body), len(want))
	}
	if ct := r.header.Get("Content-Type"); ct != "application/vnd.sqlite3" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := r.header.Get("Content-Disposition"); cd != `attachment; filename="`+res.Backup.Name+`"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if r.header.Get("Content-Length") != fmt.Sprint(len(want)) || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", r.header)
	}
	if !strings.HasPrefix(string(r.body), "SQLite format 3") {
		t.Error("download is not a SQLite file")
	}

	// Unknown backup: 404 in the error format.
	if r := c.do("GET", "/api/v1/system/backups/mailroom-20200101T000000Z.db", adminToken); r.status != 404 || r.code() != api.CodeNotFound {
		t.Fatalf("unknown = %d %s", r.status, r.body)
	}
}

func TestAPIDownloadRejectsTraversal(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(f.cfg.BackupsDir(), 0o700)
	os.WriteFile(filepath.Join(f.cfg.DataDir, "secret.key"), []byte("TOPSECRET"), 0o600)
	os.WriteFile(f.cfg.DBPath(), []byte("SQLite format 3\x00TOPSECRET"), 0o600)
	c := newAPI(t, f, f.service())
	for _, name := range []string{
		"..%2Fsecret.key",
		"..%2F..%2Fsecret.key",
		"%2E%2E%2Fmailroom.db",
		"..%5Csecret.key",
		"secret.key",
		"mailroom.db",
		"mailroom-20260105T090000Z.db%2F..%2F..%2Fsecret.key",
		"%2Fetc%2Fpasswd",
		"mailroom-20260105T090000Z.db%00",
	} {
		r := c.do("GET", "/api/v1/system/backups/"+name, adminToken)
		if r.status == 200 || strings.Contains(string(r.body), "TOPSECRET") {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		if r.status != 422 || r.code() != api.CodeValidation {
			t.Errorf("%s: %d %s, want 422 validation", name, r.status, r.body)
		}
	}
	// Unescaped traversal never reaches the handler with a bad name.
	r := c.do("GET", "/api/v1/system/backups/../../secret.key", adminToken)
	if r.status == 200 || strings.Contains(string(r.body), "TOPSECRET") {
		t.Fatalf("raw traversal: %d %s", r.status, r.body)
	}
}

func TestAPIDownloadStreamsLargeFile(t *testing.T) {
	f := newFixture(t)
	name := backupName(BackupPrefix, f.clock.Now())
	os.MkdirAll(f.cfg.BackupsDir(), 0o700)
	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB
	os.WriteFile(filepath.Join(f.cfg.BackupsDir(), name), []byte(big), 0o600)
	c := newAPI(t, f, f.service())
	r := c.do("GET", "/api/v1/system/backups/"+name, adminToken)
	if r.status != 200 || len(r.body) != len(big) {
		t.Fatalf("download = %d, %d bytes", r.status, len(r.body))
	}
}
