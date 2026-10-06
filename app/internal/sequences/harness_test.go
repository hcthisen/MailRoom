package sequences_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/contactstore"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/hooks"
	"mailroom/internal/jobs"
	"mailroom/internal/sequences"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

const (
	adminToken = "mr_admin"
	readToken  = "mr_read"
	ownerEmail = "owner@example.org"
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

// harness wires a sequences service with real foundation and W1
// dependencies on a temp database.
type harness struct {
	t        *testing.T
	ctx      context.Context
	d        *db.DB
	clock    *testutil.Clock
	bus      *hooks.Bus
	store    *contactstore.Store
	settings *settings.Service
	content  *content.Service
	runner   *jobs.Runner
	svc      *sequences.Service
	h        http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	hs := &harness{t: t, ctx: context.Background(), d: testutil.NewDB(t), clock: testutil.NewClock(time.Time{}), bus: hooks.New()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hs.store = &contactstore.Store{DB: hs.d, Bus: hs.bus, Clock: hs.clock}
	hs.settings = settings.New(settings.Deps{DB: hs.d, Clock: hs.clock, Domain: "example.com"})
	owner, footer := ownerEmail, "Example Ltd, 1 Street, Town"
	if _, err := hs.settings.Update(hs.ctx, settings.Patch{OwnerEmail: &owner, CompanyFooter: &footer}, 0); err != nil {
		t.Fatal(err)
	}
	hs.content = content.New(content.Deps{
		DB: hs.d, Settings: hs.settings, Signer: testutil.NewSigner(t, hs.clock), Contacts: hs.store,
		BaseURL: "https://newsletter.example.com", DataDir: t.TempDir(), Clock: hs.clock, Logger: logger,
	})
	hs.runner = jobs.New(jobs.Deps{DB: hs.d, Clock: hs.clock, Logger: logger, PollInterval: 10 * time.Millisecond, ProgressInterval: 5 * time.Millisecond})
	hs.svc = sequences.New(sequences.Deps{
		DB: hs.d, Store: hs.store, Content: hs.content, Bus: hs.bus, Jobs: hs.runner, Settings: hs.settings,
		Clock: hs.clock, Logger: logger, SchedulerInterval: 20 * time.Millisecond,
	})
	hs.svc.RegisterHooks(nil)
	hs.svc.RegisterJobs()
	a, err := api.New(api.Config{DB: hs.d, Clock: hs.clock, Logger: logger, Auth: fakeAuth{}, BaseURL: "https://newsletter.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	hs.svc.RegisterAPI(a)
	hs.runner.RegisterAPI(a)
	hs.h = a.Handler()
	return hs
}

// tx runs fn in a write transaction and fails the test on error.
func (hs *harness) tx(fn func(tx *sql.Tx) error) {
	hs.t.Helper()
	if err := hs.d.WithTx(hs.ctx, fn); err != nil {
		hs.t.Fatal(err)
	}
}

// contact creates an active contact through the store (hooks fire).
func (hs *harness) contact(email string, tags ...string) core.Contact {
	hs.t.Helper()
	var c core.Contact
	hs.tx(func(tx *sql.Tx) error {
		var err error
		c, err = hs.store.Create(hs.ctx, tx, contactstore.CreateInput{Email: email, Status: core.StatusActive, Tags: tags, Source: "api"})
		return err
	})
	return c
}

// addTags adds tags to a contact through the store with source.
func (hs *harness) addTags(contactID int64, source string, tags ...string) {
	hs.t.Helper()
	hs.tx(func(tx *sql.Tx) error { return hs.store.AddTags(hs.ctx, tx, contactID, tags, source) })
}

// setStatus changes a contact's status through the store.
func (hs *harness) setStatus(contactID int64, to core.ContactStatus) {
	hs.t.Helper()
	hs.tx(func(tx *sql.Tx) error {
		_, err := hs.store.SetStatus(hs.ctx, tx, contactID, to, contactstore.StatusChange{Source: contactstore.SourceAPI, Note: "test"})
		return err
	})
}

// sequence creates a sequence with steps of the given delays (seconds).
func (hs *harness) sequence(in sequences.SequenceInput, delays ...int64) sequences.Sequence {
	hs.t.Helper()
	seq, err := hs.svc.Create(hs.ctx, in)
	if err != nil {
		hs.t.Fatalf("create sequence: %v", err)
	}
	for i, d := range delays {
		seq, _, err = hs.svc.AddStep(hs.ctx, seq.ID, sequences.StepInput{
			Email: &sequences.StepEmail{Subject: fmt.Sprintf("Step %d", i+1), Body: fmt.Sprintf("Body %d", i+1)}, DelaySeconds: d,
		}, 0)
		if err != nil {
			hs.t.Fatalf("add step: %v", err)
		}
	}
	return seq
}

// enroll enrols a contact and returns the enrollment ID (0 = skipped).
func (hs *harness) enroll(seqID, contactID int64, opts core.EnrollOptions) int64 {
	hs.t.Helper()
	var id int64
	hs.tx(func(tx *sql.Tx) error {
		var err error
		id, err = hs.svc.Enroll(hs.ctx, tx, seqID, contactID, opts)
		return err
	})
	return id
}

// run runs the scheduler once.
func (hs *harness) run() sequences.SchedulerResult {
	hs.t.Helper()
	res, err := hs.svc.RunDue(hs.ctx)
	if err != nil {
		hs.t.Fatalf("RunDue: %v", err)
	}
	return res
}

// enrollment is an enrollments row.
type enrollment struct {
	Status, ExitReason, Source string
	NextStepID                 sql.NullInt64
	NextRunAt                  sql.NullString
}

func (hs *harness) enrollment(id int64) enrollment {
	hs.t.Helper()
	var e enrollment
	if err := hs.d.Read.QueryRow(`SELECT status, exit_reason, source, next_step_id, next_run_at FROM enrollments WHERE id = ?`, id).
		Scan(&e.Status, &e.ExitReason, &e.Source, &e.NextStepID, &e.NextRunAt); err != nil {
		hs.t.Fatalf("enrollment %d: %v", id, err)
	}
	return e
}

// nextRun returns an enrollment's next_run_at.
func (hs *harness) nextRun(id int64) time.Time {
	hs.t.Helper()
	e := hs.enrollment(id)
	if !e.NextRunAt.Valid {
		hs.t.Fatalf("enrollment %d has no next_run_at (%+v)", id, e)
	}
	ts, err := db.ParseTime(e.NextRunAt.String)
	if err != nil {
		hs.t.Fatal(err)
	}
	return ts
}

func (hs *harness) count(query string, args ...any) int {
	hs.t.Helper()
	return testutil.Count(hs.t, hs.d.Read, query, args...)
}

// messages returns the step IDs of an enrollment's messages in order.
func (hs *harness) messages(enrollmentID int64) []int64 {
	hs.t.Helper()
	rows, err := hs.d.Read.Query(`SELECT step_id FROM messages WHERE enrollment_id = ? ORDER BY id`, enrollmentID)
	if err != nil {
		hs.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			hs.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func (hs *harness) hasTag(contactID int64, tag string) bool {
	return hs.count(`SELECT count(*) FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id WHERE ct.contact_id = ? AND t.name = ?`, contactID, tag) > 0
}

func strp(s string) *string { return &s }

// fieldOf returns the field of a validation error, or "!" when err isn't one.
func fieldOf(err error) string {
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		return ve.Field
	}
	return "!"
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

// wantError checks an error response's status, code and (optional) field.
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

func (hs *harness) do(method, path, body string, headers ...string) resp {
	hs.t.Helper()
	return hs.doAs(adminToken, method, path, body, headers...)
}

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
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return resp{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// jsonBody encodes v as JSON.
func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// startJobs runs the job runner until the test ends.
func (hs *harness) startJobs() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hs.runner.Run(ctx) }()
	hs.t.Cleanup(func() { cancel(); <-done })
}

func (hs *harness) waitJob(id int64) jobs.Job {
	hs.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	j, err := hs.runner.Wait(ctx, id)
	if err != nil {
		hs.t.Fatalf("wait job %d: %v", id, err)
	}
	return j
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
