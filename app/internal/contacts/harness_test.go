package contacts_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/contacts"
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
	sessionID  = "session-cookie"
	csrfToken  = "csrf-123"
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

func (fakeAuth) AuthenticateSession(_ context.Context, cookie string) (api.Principal, string, error) {
	if cookie != sessionID {
		return api.Principal{}, "", api.ErrUnauthorized
	}
	return api.Principal{Actor: core.Actor{Type: core.ActorUser, ID: "7", Name: "owner@example.com"}, Scope: api.ScopeAdmin}, csrfToken, nil
}

// fakeSegments resolves segment IDs to fixed SQL.
type fakeSegments map[int64]string

func (f fakeSegments) WhereSQL(_ context.Context, id int64) (string, []any, error) {
	expr, ok := f[id]
	if !ok {
		return "", nil, fmt.Errorf("segment %d: %w", id, core.ErrNotFound)
	}
	return expr, nil, nil
}

// fakeTagRefs stands in for the segments service's tag references. It
// records each rewrite as a settings row written in the caller's tx, so a
// test sees whether the rewrite committed with the tag change.
type fakeTagRefs struct {
	mu    sync.Mutex
	users map[string][]int64 // lower-case tag name -> segment IDs
	fail  error              // returned by ReplaceTagName when set
}

func (f *fakeTagRefs) ReplaceTagName(ctx context.Context, tx *sql.Tx, from, to string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	ids := f.users[strings.ToLower(from)]
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, '1', '')`, "tagref:"+from+">"+to); err != nil {
		return 0, err
	}
	delete(f.users, strings.ToLower(from))
	f.users[strings.ToLower(to)] = append(f.users[strings.ToLower(to)], ids...)
	return len(ids), nil
}

func (f *fakeTagRefs) SegmentsUsingTag(_ context.Context, tx *sql.Tx, name string) ([]int64, error) {
	if tx == nil {
		return nil, fmt.Errorf("SegmentsUsingTag called without a transaction")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[strings.ToLower(name)], nil
}

// Segments of the fixture.
const (
	segActive = 1 // active contacts
	segVIP    = 2 // contacts tagged vip
)

type harness struct {
	t      *testing.T
	d      *db.DB
	clock  *testutil.Clock
	store  *contactstore.Store
	svc    *contacts.Service
	runner *jobs.Runner
	h      http.Handler
	refs   *fakeTagRefs

	mu        sync.Mutex
	tagAdded  []hooks.TagAdded
	statusChg []hooks.StatusChanged
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	hs := &harness{t: t, d: testutil.NewDB(t), clock: testutil.NewClock(time.Time{}), refs: &fakeTagRefs{users: map[string][]int64{}}}
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
	hs.store = &contactstore.Store{DB: hs.d, Bus: bus, Clock: hs.clock}
	hs.runner = jobs.New(jobs.Deps{
		DB: hs.d, Clock: hs.clock, Logger: logger,
		PollInterval: 10 * time.Millisecond, ProgressInterval: 5 * time.Millisecond,
	})
	hs.svc = contacts.New(contacts.Deps{
		DB: hs.d, Store: hs.store, Jobs: hs.runner, Clock: hs.clock, Logger: logger, TagRefs: hs.refs,
		Segments: fakeSegments{
			segActive: "c.status = 'active'",
			segVIP:    "EXISTS (SELECT 1 FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id WHERE ct.contact_id = c.id AND t.name = 'vip')",
		},
	})
	hs.svc.RegisterJobs(hs.runner)
	a, err := api.New(api.Config{DB: hs.d, Clock: hs.clock, Logger: logger, Auth: fakeAuth{}, BaseURL: "https://newsletter.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	hs.svc.RegisterAPI(a)
	hs.runner.RegisterAPI(a)
	hs.h = a.Handler()
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	j, err := hs.runner.Wait(ctx, id)
	if err != nil {
		hs.t.Fatalf("wait job %d: %v", id, err)
	}
	return j
}

func (hs *harness) tagEvents() []hooks.TagAdded {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]hooks.TagAdded(nil), hs.tagAdded...)
}

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

func (r resp) errBody(t *testing.T) api.ErrorBody {
	t.Helper()
	var e struct {
		Error api.ErrorBody `json:"error"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil || e.Error.Code == "" {
		t.Fatalf("not an error response (%d): %s", r.status, r.body)
	}
	return e.Error
}

// wantStatus fails unless the response has the status.
func (r resp) wantStatus(t *testing.T, status int) resp {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
	return r
}

// wantError checks an error response's status, code and (optional) field.
func (r resp) wantError(t *testing.T, status int, code, field string) api.ErrorBody {
	t.Helper()
	r.wantStatus(t, status)
	e := r.errBody(t)
	if e.Code != code || (field != "" && e.Field != field) {
		t.Fatalf("error = %+v, want code %s field %s", e, code, field)
	}
	return e
}

// do sends a request with the admin token; see doAs.
func (hs *harness) do(method, path, body string, headers ...string) resp {
	hs.t.Helper()
	return hs.doAs(adminToken, method, path, body, headers...)
}

// doAs sends a request as token ("session" = the UI user's session).
func (hs *harness) doAs(token, method, path, body string, headers ...string) resp {
	hs.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	req.RemoteAddr = "192.0.2.1:1234"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch token {
	case "":
	case "session":
		req.AddCookie(&http.Cookie{Name: api.SessionCookie, Value: sessionID})
		req.Header.Set(api.CSRFHeader, csrfToken)
	default:
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return resp{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// createContact creates a contact through the API.
func (hs *harness) createContact(body string) contacts.Contact {
	hs.t.Helper()
	var c contacts.Contact
	hs.do("POST", "/contacts", body).wantStatus(hs.t, http.StatusCreated).decode(hs.t, &c)
	return c
}

// insertMessage inserts a messages row for a contact and returns its id.
func (hs *harness) insertMessage(contactID int64, email, kind, status, subject string, at time.Time) int64 {
	hs.t.Helper()
	ts := db.FormatTime(at)
	var id int64
	err := hs.d.Write.QueryRow(`INSERT INTO messages (kind, contact_id, to_email, subject, status, data, next_attempt_at, queued_at, sent_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '{"k":"v"}', ?, ?, CASE WHEN ? = 'queued' THEN NULL ELSE ? END, ?) RETURNING id`,
		kind, contactID, email, subject, status, ts, ts, status, ts, ts).Scan(&id)
	if err != nil {
		hs.t.Fatalf("insert message: %v", err)
	}
	return id
}

// insertEvent inserts a message event and returns its id.
func (hs *harness) insertEvent(messageID int64, typ, subtype string, at time.Time) int64 {
	hs.t.Helper()
	var id int64
	err := hs.d.Write.QueryRow(`INSERT INTO events (message_id, type, subtype, detail, created_at) VALUES (?, ?, ?, '{"raw":"x@example.com"}', ?) RETURNING id`,
		messageID, typ, subtype, db.FormatTime(at)).Scan(&id)
	if err != nil {
		hs.t.Fatalf("insert event: %v", err)
	}
	return id
}

// insertClick inserts a click on url and returns its id.
func (hs *harness) insertClick(contactID, messageID int64, url string, at time.Time) int64 {
	hs.t.Helper()
	ts := db.FormatTime(at)
	testutil.Exec(hs.t, hs.d, `INSERT INTO links (url, created_at) VALUES (?, ?) ON CONFLICT DO NOTHING`, url, ts)
	var id int64
	err := hs.d.Write.QueryRow(`INSERT INTO clicks (link_id, message_id, contact_id, created_at)
		VALUES ((SELECT id FROM links WHERE url = ?), ?, ?, ?) RETURNING id`, url, messageID, contactID, ts).Scan(&id)
	if err != nil {
		hs.t.Fatalf("insert click: %v", err)
	}
	return id
}

// count runs a count query on the read pool.
func (hs *harness) count(query string, args ...any) int {
	hs.t.Helper()
	return testutil.Count(hs.t, hs.d.Read, query, args...)
}
