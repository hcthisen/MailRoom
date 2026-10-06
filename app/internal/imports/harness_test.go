package imports

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/hooks"
	"mailroom/internal/jobs"
	"mailroom/internal/testutil"
)

const (
	adminToken = "mr_admin"
	readToken  = "mr_read"
)

type fakeAuth struct{}

func (fakeAuth) AuthenticateToken(_ context.Context, tok string) (api.Principal, error) {
	switch tok {
	case adminToken:
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "1", Name: "setup-agent"}, Scope: api.ScopeAdmin}, nil
	case readToken:
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "2", Name: "reader"}, Scope: api.ScopeRead}, nil
	}
	return api.Principal{}, fmt.Errorf("unknown token: %w", api.ErrUnauthorized)
}

func (fakeAuth) AuthenticateSession(context.Context, string) (api.Principal, string, error) {
	return api.Principal{}, "", api.ErrUnauthorized
}

// harness is a full imports setup on a temp database: store with hooks,
// job runner, service and API.
type harness struct {
	t       *testing.T
	d       *db.DB
	clock   *testutil.Clock
	store   *contactstore.Store
	svc     *Service
	runner  *jobs.Runner
	a       *api.API
	h       http.Handler
	dataDir string

	mu        sync.Mutex
	tagAdded  []hooks.TagAdded
	statusChg []hooks.StatusChanged
	created   []hooks.ContactCreated
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	hs := &harness{t: t, d: testutil.NewDB(t), clock: testutil.NewClock(time.Time{}), dataDir: t.TempDir()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := hooks.New()
	bus.OnTagAdded(func(_ context.Context, _ *sql.Tx, e hooks.TagAdded) error {
		hs.mu.Lock()
		defer hs.mu.Unlock()
		hs.tagAdded = append(hs.tagAdded, e)
		return nil
	})
	bus.OnStatusChanged(func(_ context.Context, _ *sql.Tx, e hooks.StatusChanged) error {
		hs.mu.Lock()
		defer hs.mu.Unlock()
		hs.statusChg = append(hs.statusChg, e)
		return nil
	})
	bus.OnContactCreated(func(_ context.Context, _ *sql.Tx, e hooks.ContactCreated) error {
		hs.mu.Lock()
		defer hs.mu.Unlock()
		hs.created = append(hs.created, e)
		return nil
	})
	hs.store = &contactstore.Store{DB: hs.d, Bus: bus, Clock: hs.clock}
	hs.runner = jobs.New(jobs.Deps{
		DB: hs.d, Clock: hs.clock, Logger: logger,
		PollInterval: 10 * time.Millisecond, ProgressInterval: 5 * time.Millisecond,
	})
	hs.svc = New(Deps{DB: hs.d, Store: hs.store, Jobs: hs.runner, Clock: hs.clock, Logger: logger, DataDir: hs.dataDir})
	hs.svc.RegisterJobs(hs.runner)
	hs.runner.Register("other.job", func(context.Context, jobs.JobContext) (any, error) { return nil, nil })
	a, err := api.New(api.Config{DB: hs.d, Clock: hs.clock, Logger: logger, Auth: fakeAuth{}, BaseURL: "https://newsletter.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	hs.svc.RegisterAPI(a)
	hs.runner.RegisterAPI(a)
	hs.a, hs.h = a, a.Handler()
	return hs
}

// startJobs runs the job runner until the test ends.
func (hs *harness) startJobs() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hs.runner.Run(ctx) }()
	hs.t.Cleanup(func() { cancel(); <-done })
}

// waitJob waits for a job to finish.
func (hs *harness) waitJob(id int64) jobs.Job {
	hs.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	j, err := hs.runner.Wait(ctx, id)
	if err != nil {
		hs.t.Fatalf("wait job %d: %v", id, err)
	}
	return j
}

// actorCtx is a context with the admin token's actor.
func actorCtx() context.Context {
	return core.WithActor(context.Background(), core.Actor{Type: core.ActorToken, ID: "1", Name: "setup-agent"})
}

// importNow runs a real import of csv through the job runner (which must
// be started) and returns the finished import.
func (hs *harness) importNow(csv, onExisting string) ImportJob {
	hs.t.Helper()
	res, err := hs.svc.Import(actorCtx(), strings.NewReader(csv), Options{OnExisting: onExisting, Filename: "contacts.csv"}, false)
	if err != nil {
		hs.t.Fatalf("Import: %v", err)
	}
	if res.Job == nil || res.Report != nil {
		hs.t.Fatalf("Import result = %+v", res)
	}
	if j := hs.waitJob(res.Job.ID); j.Status != jobs.StatusSucceeded {
		hs.t.Fatalf("job %d %s: %s", j.ID, j.Status, j.Error)
	}
	ij, err := hs.svc.Get(context.Background(), res.Job.ID)
	if err != nil {
		hs.t.Fatal(err)
	}
	if ij.Report == nil {
		hs.t.Fatalf("no report: %+v", ij)
	}
	return ij
}

// dryRun evaluates csv without writing.
func (hs *harness) dryRun(csv, onExisting string) ImportReport {
	hs.t.Helper()
	res, err := hs.svc.Import(actorCtx(), strings.NewReader(csv), Options{OnExisting: onExisting}, true)
	if err != nil {
		hs.t.Fatalf("dry run: %v", err)
	}
	if res.Report == nil || res.Job != nil || !res.Report.DryRun {
		hs.t.Fatalf("dry run result = %+v", res)
	}
	return *res.Report
}

// contact returns the contact with email.
func (hs *harness) contact(email string) core.Contact {
	hs.t.Helper()
	c, err := hs.store.GetByEmail(context.Background(), nil, email)
	if err != nil {
		hs.t.Fatalf("contact %s: %v", email, err)
	}
	return c
}

func (hs *harness) count(query string, args ...any) int {
	hs.t.Helper()
	return testutil.Count(hs.t, hs.d.Read, query, args...)
}

// uploads lists the files in the imports directory.
func (hs *harness) uploads() []string {
	hs.t.Helper()
	entries, err := os.ReadDir(hs.svc.Dir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		hs.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (hs *harness) tagEvents() []hooks.TagAdded {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]hooks.TagAdded(nil), hs.tagAdded...)
}

func (hs *harness) statusEvents() []hooks.StatusChanged {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]hooks.StatusChanged(nil), hs.statusChg...)
}

// ---------------------------------------------------------------- HTTP

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (r resp) wantStatus(t *testing.T, status int) resp {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
	return r
}

// wantError checks an error response's status, code and field.
func (r resp) wantError(t *testing.T, status int, code, field string) api.ErrorBody {
	t.Helper()
	r.wantStatus(t, status)
	var e struct {
		Error api.ErrorBody `json:"error"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil || e.Error.Code == "" {
		t.Fatalf("not an error response (%d): %s", r.status, r.body)
	}
	if e.Error.Code != code || (field != "" && e.Error.Field != field) {
		t.Fatalf("error = %+v, want code %s field %s", e.Error, code, field)
	}
	return e.Error
}

// send sends a request with body and content type as token.
func (hs *harness) send(token, method, path, contentType string, body io.Reader, headers ...string) resp {
	hs.t.Helper()
	req := httptest.NewRequest(method, "/api/v1"+path, body)
	req.RemoteAddr = "192.0.2.1:1234"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return resp{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

func (hs *harness) get(path string) resp {
	hs.t.Helper()
	return hs.send(adminToken, http.MethodGet, path, "", nil)
}

// multipartBody builds a form with the file part and extra fields.
func multipartBody(t *testing.T, csv string, fields ...string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i := 0; i+1 < len(fields); i += 2 {
		if err := mw.WriteField(fields[i], fields[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("file", "contacts.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw, csv); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), &buf
}

// postMultipart posts csv as a multipart upload.
func (hs *harness) postMultipart(query, csv string, fields ...string) resp {
	hs.t.Helper()
	ct, body := multipartBody(hs.t, csv, fields...)
	return hs.send(adminToken, http.MethodPost, "/imports"+query, ct, body)
}

// fakeJob is a JobContext for calling the handler directly.
type fakeJob struct {
	id       int64
	params   json.RawMessage
	resumed  bool
	progress float64
}

func (f *fakeJob) ID() int64             { return f.id }
func (f *fakeJob) Type() string          { return JobType }
func (f *fakeJob) Params(v any) error    { return json.Unmarshal(f.params, v) }
func (f *fakeJob) SetProgress(p float64) { f.progress = p }
func (f *fakeJob) Resumed() bool         { return f.resumed }

// csvRows builds a CSV with a header and n generated rows.
func csvRows(header string, n int, row func(i int) string) string {
	var b strings.Builder
	b.WriteString(header + "\n")
	for i := range n {
		b.WriteString(row(i) + "\n")
	}
	return b.String()
}
