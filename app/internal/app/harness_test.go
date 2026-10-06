package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/app"
	"mailroom/internal/auth"
	"mailroom/internal/config"
	"mailroom/internal/jobs"
	"mailroom/internal/testutil"
)

// harness is a fully wired app on a temp data directory with a fake clock
// and a fake email provider, and no network access. The SNS webhook fetches
// its signing certificate from a fake HTTP client (see snsCertURL).
type harness struct {
	t        *testing.T
	ctx      context.Context
	app      *app.App
	h        http.Handler
	clock    *testutil.Clock
	provider *testutil.FakeProvider
	gate     *gatedProvider // wraps provider; the app sends through it
	token    string         // admin API token
	vars     map[string]string
	tune     func(*app.Options)
	webhook  *certFetcher
	stopJobs func() // stops the jobs runner started by startJobs; nil when not running
}

// testVars is a valid environment for the harness.
func testVars(t *testing.T) map[string]string {
	return map[string]string{
		"MAILROOM_DATA_DIR": t.TempDir(),
		"DOMAIN":            "example.com",
		"FROM_NAME":         "Example News",
		"FROM_EMAIL":        "news@example.com",
		"REPLY_TO_EMAIL":    "hello@example.com",
		"OWNER_EMAIL":       "owner@example.org",
		"COMPANY_FOOTER":    "Example Ltd, 1 Street, Town",
		"MAILROOM_SECRET":   "0123456789abcdef0123456789abcdef",
		"AWS_REGION":        testRegion,
		"SNS_TOPIC_ARN":     testTopicARN,
	}
}

func loadConfig(t *testing.T, vars map[string]string) config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newHarness builds the app with opts adjusted by tune (may be nil).
func newHarness(t *testing.T, tune func(*app.Options)) *harness {
	t.Helper()
	provider := testutil.NewFakeProvider()
	hs := &harness{
		t: t, ctx: context.Background(), clock: testutil.NewClock(testutil.Epoch),
		provider: provider, gate: &gatedProvider{FakeProvider: provider},
		vars: testVars(t), tune: tune, webhook: newCertFetcher(),
	}
	hs.open()
	t.Cleanup(hs.close)
	_, token, err := app.CreateToken(hs.ctx, hs.app.DB, auth.TokenInput{Name: "test-agent", Scope: api.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	hs.token = token
	return hs
}

// open builds the app on the harness's data directory.
func (hs *harness) open() {
	hs.t.Helper()
	opts := app.Options{Clock: hs.clock, Logger: quietLogger(), Offline: true, Provider: hs.gate, WebhookHTTP: hs.webhook}
	if hs.tune != nil {
		hs.tune(&opts)
	}
	a, err := app.New(hs.ctx, loadConfig(hs.t, hs.vars), opts)
	if err != nil {
		hs.t.Fatal(err)
	}
	hs.app, hs.h = a, a.Handler()
}

// close stops the jobs runner and closes the app.
func (hs *harness) close() {
	if hs.stopJobs != nil {
		hs.stopJobs()
		hs.stopJobs = nil
	}
	if hs.app != nil {
		hs.app.Close()
	}
}

// restart simulates a process restart: the jobs runner stops, the app is
// closed, and a new app is built on the same data directory (database,
// secret, clock and email provider carry over). The jobs runner is started
// again if it was running.
func (hs *harness) restart() {
	hs.t.Helper()
	jobs := hs.stopJobs != nil
	hs.close()
	hs.open()
	if jobs {
		hs.startJobs()
	}
}

// startJobs runs the jobs runner in the background until the test ends or
// the app restarts. Tests wait for jobs with waitJob, never by sleeping.
func (hs *harness) startJobs() {
	hs.t.Helper()
	if hs.stopJobs != nil {
		return
	}
	ctx, cancel := context.WithCancel(hs.ctx)
	done := make(chan error, 1)
	runner := hs.app.Jobs
	go func() { done <- runner.Run(ctx) }()
	hs.stopJobs = func() {
		cancel()
		if err := <-done; err != nil {
			hs.t.Errorf("jobs runner: %v", err)
		}
	}
}

// waitJob waits (bounded) for the job to finish and fails the test unless
// it succeeded.
func (hs *harness) waitJob(id int64) jobs.Job {
	hs.t.Helper()
	ctx, cancel := context.WithTimeout(hs.ctx, 30*time.Second)
	defer cancel()
	j, err := hs.app.Jobs.Wait(ctx, id)
	if err != nil {
		hs.t.Fatalf("wait for job %d: %v", id, err)
	}
	if j.Status != jobs.StatusSucceeded {
		hs.t.Fatalf("job %d (%s) = %s: %s", id, j.Type, j.Status, j.Error)
	}
	return j
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (r resp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// want fails unless the response has the status.
func (r resp) want(t *testing.T, code int) resp {
	t.Helper()
	if r.code != code {
		t.Fatalf("status %d, want %d: %s", r.code, code, r.body)
	}
	return r
}

// raw sends a request through the full handler (server middleware
// included). headers are name/value pairs.
func (hs *harness) raw(method, path, contentType, body string, headers ...string) resp {
	hs.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if rd != nil {
		// Like a real server's body (httptest's is a NopCloser): once it
		// is closed it can't be read any more.
		req.Body = &serverBody{r: rd}
	}
	req.RemoteAddr = "192.0.2.10:4321"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return resp{code: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// serverBody is a request body that behaves like net/http's server body:
// reads after Close fail.
type serverBody struct {
	r      io.Reader
	closed bool
}

func (b *serverBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, errors.New("http: invalid Read on closed Body")
	}
	return b.r.Read(p)
}

func (b *serverBody) Close() error { b.closed = true; return nil }

// api calls /api/v1+path with the admin token and a JSON body.
func (hs *harness) api(method, path, body string, headers ...string) resp {
	hs.t.Helper()
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	return hs.raw(method, api.Prefix+path, ct, body, append([]string{"Authorization", "Bearer " + hs.token}, headers...)...)
}

func (hs *harness) count(query string, args ...any) int {
	hs.t.Helper()
	return testutil.Count(hs.t, hs.app.DB.Read, query, args...)
}
