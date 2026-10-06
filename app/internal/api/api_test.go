package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

// Test credentials.
const (
	adminToken = "mr_admin"
	readToken  = "mr_read"
	otherAdmin = "mr_other"
	sessionID  = "session-cookie"
	csrfToken  = "csrf-123"
)

type fakeAuth struct{}

func (fakeAuth) AuthenticateToken(_ context.Context, tok string) (api.Principal, error) {
	switch tok {
	case adminToken:
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "1", Name: "admin"}, Scope: api.ScopeAdmin}, nil
	case otherAdmin:
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "3", Name: "other"}, Scope: api.ScopeAdmin}, nil
	case readToken:
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "2", Name: "reader"}, Scope: api.ScopeRead}, nil
	}
	return api.Principal{}, fmt.Errorf("unknown: %w", api.ErrUnauthorized)
}

func (fakeAuth) AuthenticateSession(_ context.Context, cookie string) (api.Principal, string, error) {
	if cookie != sessionID {
		return api.Principal{}, "", api.ErrUnauthorized
	}
	return api.Principal{Actor: core.Actor{Type: core.ActorUser, ID: "7", Name: "owner@example.com"}, Scope: api.ScopeAdmin}, csrfToken, nil
}

type harness struct {
	t       *testing.T
	api     *api.API
	h       http.Handler
	db      *db.DB
	clock   *testutil.Clock
	created atomic.Int64 // POST /things executions
	block   chan struct{}
	entered chan struct{}
}

type thing struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type thingOutput struct {
	ETag string `header:"ETag"`
	Body thing
}

type createThingInput struct {
	Body struct {
		Name  string `json:"name" minLength:"1" maxLength:"20"`
		Count int    `json:"count,omitempty" minimum:"0"`
	}
}

type createThingOutput struct {
	Body struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Actor string `json:"actor"`
	}
}

type errorInput struct {
	Kind string `path:"kind"`
}

type patchThingInput struct {
	ID int64 `path:"id"`
	api.IfMatchParam
	Body struct {
		Name string `json:"name"`
	}
}

type listThingsInput struct {
	api.PageParams
}

func newHarness(t *testing.T, cfg api.Config) *harness {
	t.Helper()
	hs := &harness{t: t, db: testutil.NewDB(t), clock: testutil.NewClock(time.Time{})}
	cfg.DB, cfg.Clock, cfg.BaseURL = hs.db, hs.clock, "https://newsletter.example.com"
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if cfg.Auth == nil {
		cfg.Auth = fakeAuth{}
	}
	a, err := api.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hs.api = a

	api.Register(a, huma.Operation{
		OperationID: "get-error", Method: http.MethodGet, Path: "/errors/{kind}",
		Summary: "Error", Description: "Returns an error.", Tags: []string{"Test"},
	}, func(ctx context.Context, in *errorInput) (*thingOutput, error) {
		switch in.Kind {
		case "not_found":
			return nil, fmt.Errorf("thing 9: %w", core.ErrNotFound)
		case "conflict":
			return nil, fmt.Errorf("exists: %w", core.ErrConflict)
		case "suppressed":
			return nil, fmt.Errorf("x@example.com: %w", core.ErrSuppressed)
		case "validation":
			return nil, fmt.Errorf("create: %w", core.Invalid("email", "is not a valid email address"))
		case "validation_plain":
			return nil, fmt.Errorf("bad: %w", core.ErrValidation)
		case "precondition":
			return nil, fmt.Errorf("stale: %w", core.ErrPrecondition)
		case "forbidden":
			return nil, fmt.Errorf("nope: %w", core.ErrForbidden)
		case "huma":
			return nil, huma.Error400BadRequest("bad thing")
		case "panic":
			panic("boom")
		}
		return nil, fmt.Errorf("sql: database is locked at /data/mailroom.db")
	})

	api.Register(a, huma.Operation{
		OperationID: "create-thing", Method: http.MethodPost, Path: "/things", DefaultStatus: http.StatusCreated,
		Summary: "Create", Description: "Creates a thing.", Tags: []string{"Test"},
		Metadata: map[string]any{api.MetaAlsoResponds: map[int]string{http.StatusOK: "Dry run: the thing that would be created"}},
	}, func(ctx context.Context, in *createThingInput) (*createThingOutput, error) {
		if hs.block != nil {
			hs.entered <- struct{}{}
			<-hs.block
		}
		out := &createThingOutput{}
		out.Body.ID = hs.created.Add(1)
		out.Body.Name = in.Body.Name
		out.Body.Actor = core.ActorFromContext(ctx).String()
		return out, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-secret", Method: http.MethodPost, Path: "/secrets", DefaultStatus: http.StatusCreated,
		Summary: "Secret", Description: "Returns a one-time secret.", Tags: []string{"Test"},
		Metadata: map[string]any{api.MetaSensitive: true},
	}, func(ctx context.Context, in *createThingInput) (*createThingOutput, error) {
		out := &createThingOutput{}
		out.Body.ID = hs.created.Add(1)
		out.Body.Name = "secret-" + strconv.FormatInt(out.Body.ID, 10)
		return out, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "preview-thing", Method: http.MethodPost, Path: "/things/preview",
		Summary: "Preview", Description: "Read-only POST.", Tags: []string{"Test"},
		Metadata: map[string]any{api.MetaScope: api.ScopeRead},
	}, func(ctx context.Context, in *createThingInput) (*thingOutput, error) {
		return &thingOutput{Body: thing{Name: in.Body.Name}}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-thing", Method: http.MethodPatch, Path: "/things/{id}",
		Summary: "Update", Description: "Updates a thing at version 3.", Tags: []string{"Test"},
	}, func(ctx context.Context, in *patchThingInput) (*thingOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		if err := api.CheckVersion(v, 3); err != nil {
			return nil, err
		}
		return &thingOutput{ETag: api.ETag(4), Body: thing{ID: in.ID, Name: in.Body.Name}}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-things", Method: http.MethodGet, Path: "/things",
		Summary: "List", Description: "Lists things 1..120.", Tags: []string{"Test"},
	}, func(ctx context.Context, in *listThingsInput) (*api.ListOutput[thing], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		var items []thing
		for id := after + 1; id <= 120 && len(items) < limit+1; id++ {
			items = append(items, thing{ID: id})
		}
		return &api.ListOutput[thing]{Body: api.NewList(items, limit, func(x thing) int64 { return x.ID })}, nil
	})
	hs.h = a.Handler()
	return hs
}

type resp struct {
	status int
	header http.Header
	body   []byte
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

func (r resp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// do sends a request; token "" sends no credentials.
func (hs *harness) do(method, path, token, body string, headers ...string) resp {
	hs.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "192.0.2.1:1234"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
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

func TestErrorFormatForCoreErrors(t *testing.T) {
	hs := newHarness(t, api.Config{})
	tests := []struct {
		kind, code, field string
		status            int
	}{
		{"not_found", api.CodeNotFound, "", 404},
		{"conflict", api.CodeConflict, "", 409},
		{"suppressed", api.CodeConflict, "", 409},
		{"validation", api.CodeValidation, "email", 422},
		{"validation_plain", api.CodeValidation, "", 422},
		{"precondition", api.CodePreconditionFailed, "", 412},
		{"forbidden", api.CodeForbidden, "", 403},
		{"huma", api.CodeValidation, "", 400},
		{"internal", api.CodeInternal, "", 500},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			r := hs.do("GET", "/api/v1/errors/"+tt.kind, adminToken, "")
			if r.status != tt.status {
				t.Fatalf("status = %d, want %d (%s)", r.status, tt.status, r.body)
			}
			e := r.errBody(t)
			if e.Code != tt.code || e.Field != tt.field || e.Message == "" {
				t.Fatalf("error = %+v, want code %s field %q", e, tt.code, tt.field)
			}
			if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("content type %q", ct)
			}
		})
	}
	r := hs.do("GET", "/api/v1/errors/internal", adminToken, "")
	if strings.Contains(string(r.body), "sql") || strings.Contains(string(r.body), "mailroom.db") {
		t.Fatalf("internal error leaks details: %s", r.body)
	}
	if e := hs.do("GET", "/api/v1/errors/validation", adminToken, "").errBody(t); e.Message != "is not a valid email address" {
		t.Fatalf("validation message = %q", e.Message)
	}
}

func TestErrorFormatForHumaValidation(t *testing.T) {
	hs := newHarness(t, api.Config{})
	tests := []struct {
		name, body, field string
		status            int
	}{
		{"missing required", `{}`, "name", 422},
		{"minimum", `{"name":"x","count":-1}`, "count", 422},
		{"too long", `{"name":"` + strings.Repeat("x", 21) + `"}`, "name", 422},
		{"unknown property", `{"name":"x","bogus":1}`, "bogus", 422},
		{"malformed json", `{"name":`, "", 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := hs.do("POST", "/api/v1/things", adminToken, tt.body)
			if r.status != tt.status {
				t.Fatalf("status = %d, want %d: %s", r.status, tt.status, r.body)
			}
			e := r.errBody(t)
			if e.Code != api.CodeValidation || e.Field != tt.field {
				t.Fatalf("error = %+v, want field %q", e, tt.field)
			}
		})
	}
	if e := hs.do("POST", "/api/v1/things", adminToken, `{}`).errBody(t); e.Message != "is required" {
		t.Fatalf("message = %q", e.Message)
	}
	if hs.created.Load() != 0 {
		t.Fatal("invalid requests reached the handler")
	}
}

func TestUnknownRouteAndPanic(t *testing.T) {
	hs := newHarness(t, api.Config{})
	r := hs.do("GET", "/api/v1/nope", adminToken, "")
	if r.status != 404 || r.errBody(t).Code != api.CodeNotFound {
		t.Fatalf("unknown route = %d %s", r.status, r.body)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected the panic to propagate to the server's recoverer")
		}
	}()
	hs.do("GET", "/api/v1/errors/panic", adminToken, "")
}

func TestAuthentication(t *testing.T) {
	hs := newHarness(t, api.Config{})
	tests := []struct {
		name, method, path, token, body string
		headers                         []string
		status                          int
		code                            string
	}{
		{"no credentials", "GET", "/api/v1/things", "", "", nil, 401, api.CodeUnauthorized},
		{"unknown token", "GET", "/api/v1/things", "mr_nope", "", nil, 401, api.CodeUnauthorized},
		{"wrong scheme", "GET", "/api/v1/things", "", "", []string{"Authorization", "Basic abc"}, 401, api.CodeUnauthorized},
		{"not an mr_ token", "GET", "/api/v1/things", "", "", []string{"Authorization", "Bearer abc"}, 401, api.CodeUnauthorized},
		{"read token GET", "GET", "/api/v1/things", readToken, "", nil, 200, ""},
		{"read token POST", "POST", "/api/v1/things", readToken, `{"name":"a"}`, nil, 403, api.CodeForbidden},
		{"read token PATCH", "PATCH", "/api/v1/things/1", readToken, `{"name":"a"}`, nil, 403, api.CodeForbidden},
		{"read token read-scoped POST", "POST", "/api/v1/things/preview", readToken, `{"name":"a"}`, nil, 200, ""},
		{"admin POST", "POST", "/api/v1/things", adminToken, `{"name":"a"}`, nil, 201, ""},
		{"public spec", "GET", "/api/v1/openapi.json", "", "", nil, 200, ""},
		{"public yaml", "GET", "/api/v1/openapi.yaml", "", "", nil, 200, ""},
		{"public docs", "GET", "/api/v1/docs", "", "", nil, 200, ""},
		{"public spec with stale cookie", "GET", "/api/v1/openapi.json", "", "", []string{"Cookie", api.SessionCookie + "=stale"}, 200, ""},
		{"no huma docs page", "GET", "/api/v1/schemas/Thing", adminToken, "", nil, 404, api.CodeNotFound},
		{"session GET without CSRF", "GET", "/api/v1/things", "", "", []string{"Cookie", api.SessionCookie + "=" + sessionID}, 200, ""},
		{"session POST without CSRF", "POST", "/api/v1/things", "", `{"name":"a"}`, []string{"Cookie", api.SessionCookie + "=" + sessionID}, 403, api.CodeForbidden},
		{"session POST wrong CSRF", "POST", "/api/v1/things", "", `{"name":"a"}`, []string{"Cookie", api.SessionCookie + "=" + sessionID, api.CSRFHeader, "nope"}, 403, api.CodeForbidden},
		{"session POST with CSRF", "POST", "/api/v1/things", "", `{"name":"a"}`, []string{"Cookie", api.SessionCookie + "=" + sessionID, api.CSRFHeader, csrfToken}, 201, ""},
		{"unknown session", "GET", "/api/v1/things", "", "", []string{"Cookie", api.SessionCookie + "=bogus"}, 401, api.CodeUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := hs.do(tt.method, tt.path, tt.token, tt.body, tt.headers...)
			if r.status != tt.status {
				t.Fatalf("status = %d, want %d: %s", r.status, tt.status, r.body)
			}
			if tt.code != "" && r.errBody(t).Code != tt.code {
				t.Fatalf("code = %s, want %s", r.errBody(t).Code, tt.code)
			}
			if tt.status == 401 && r.header.Get("WWW-Authenticate") == "" {
				t.Fatal("401 without WWW-Authenticate")
			}
		})
	}
	// The actor reaches the handler's context.
	var out createThingOutput
	hs.do("POST", "/api/v1/things", adminToken, `{"name":"a"}`).decode(t, &out.Body)
	if out.Body.Actor != "token:1" {
		t.Fatalf("actor = %q", out.Body.Actor)
	}
	hs.do("POST", "/api/v1/things", "", `{"name":"a"}`, "Cookie", api.SessionCookie+"="+sessionID, api.CSRFHeader, csrfToken).decode(t, &out.Body)
	if out.Body.Actor != "user:7" {
		t.Fatalf("session actor = %q", out.Body.Actor)
	}
}

func TestNoAuthenticatorFailsClosed(t *testing.T) {
	d := testutil.NewDB(t)
	a, err := api.New(api.Config{DB: d, BaseURL: "https://x.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/anything", nil)
	req.Header.Set("Authorization", "Bearer mr_x")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	if _, err := api.New(api.Config{}); err == nil {
		t.Fatal("New without DB succeeded")
	}
}

func TestRateLimit(t *testing.T) {
	hs := newHarness(t, api.Config{Rate: 1, Burst: 2})
	for i := 0; i < 2; i++ {
		r := hs.do("GET", "/api/v1/things", adminToken, "")
		if r.status != 200 {
			t.Fatalf("request %d = %d", i, r.status)
		}
		if r.header.Get("RateLimit-Limit") != "2" || r.header.Get("RateLimit-Remaining") != strconv.Itoa(1-i) {
			t.Fatalf("headers = %v", r.header)
		}
	}
	r := hs.do("GET", "/api/v1/things", adminToken, "")
	if r.status != 429 || r.errBody(t).Code != api.CodeRateLimited {
		t.Fatalf("third request = %d %s", r.status, r.body)
	}
	if r.header.Get("Retry-After") != "1" || r.header.Get("RateLimit-Remaining") != "0" {
		t.Fatalf("429 headers = %v", r.header)
	}
	// Buckets are per actor.
	if r := hs.do("GET", "/api/v1/things", readToken, ""); r.status != 200 {
		t.Fatalf("other actor = %d", r.status)
	}
	hs.clock.Advance(time.Second)
	if r := hs.do("GET", "/api/v1/things", adminToken, ""); r.status != 200 {
		t.Fatalf("after refill = %d", r.status)
	}
	// Anonymous requests are limited per IP.
	for i := 0; i < 2; i++ {
		hs.do("GET", "/api/v1/things", "", "")
	}
	if r := hs.do("GET", "/api/v1/openapi.json", "", ""); r.status != 429 {
		t.Fatalf("anonymous = %d", r.status)
	}
}

func TestLimiterHelpers(t *testing.T) {
	clock := testutil.NewClock(time.Time{})
	l := api.NewLimiter(2, 1, clock)
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	h := l.Middleware(func(r *http.Request) string { return api.ClientIP(r, proxies) },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	call := func(xff string) int {
		req := httptest.NewRequest("POST", "/f/abc", nil)
		req.RemoteAddr = "10.0.0.1:9"
		req.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if call("1.1.1.1, 203.0.113.5") != 204 || call("9.9.9.9, 203.0.113.5") != 429 || call("203.0.113.6") != 204 {
		t.Fatal("per-IP limiting via the last X-Forwarded-For entry failed")
	}
	clock.Advance(500 * time.Millisecond)
	if call("203.0.113.5") != 204 {
		t.Fatal("bucket did not refill")
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "[2001:db8::1]:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := api.ClientIP(req, nil); got != "2001:db8::1" {
		t.Fatalf("ClientIP untrusted = %q", got)
	}
	if got := api.ClientIP(req, []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}); got != "203.0.113.9" {
		t.Fatalf("ClientIP trusted = %q", got)
	}
}

func TestIdempotency(t *testing.T) {
	hs := newHarness(t, api.Config{})
	post := func(token, key, body string) resp {
		return hs.do("POST", "/api/v1/things", token, body, api.IdempotencyHeader, key)
	}
	first := post(adminToken, "k1", `{"name":"a"}`)
	if first.status != 201 || first.header.Get(api.IdempotencyReplayHeader) != "" {
		t.Fatalf("first = %d %v", first.status, first.header)
	}
	replay := post(adminToken, "k1", `{"name":"a"}`)
	if replay.status != 201 || string(replay.body) != string(first.body) || replay.header.Get(api.IdempotencyReplayHeader) != "true" {
		t.Fatalf("replay = %d %s %v", replay.status, replay.body, replay.header)
	}
	if hs.created.Load() != 1 {
		t.Fatalf("handler ran %d times", hs.created.Load())
	}

	mismatch := post(adminToken, "k1", `{"name":"b"}`)
	if mismatch.status != 422 || mismatch.errBody(t).Field != "header.Idempotency-Key" {
		t.Fatalf("mismatch = %d %s", mismatch.status, mismatch.body)
	}
	if r := hs.do("POST", "/api/v1/things?dry_run=true", adminToken, `{"name":"a"}`, api.IdempotencyHeader, "k1"); r.status != 422 {
		t.Fatalf("same key, other query = %d", r.status)
	}

	// Keys are per actor.
	if r := post(otherAdmin, "k1", `{"name":"a"}`); r.status != 201 || r.header.Get(api.IdempotencyReplayHeader) != "" {
		t.Fatalf("other actor = %d %v", r.status, r.header)
	}
	// A failed request keeps only its hash: the same request runs again,
	// a different one with the same key is refused.
	if r := post(adminToken, "k2", `{}`); r.status != 422 {
		t.Fatalf("invalid = %d", r.status)
	}
	if r := post(adminToken, "k2", `{"name":"c"}`); r.status != 422 || r.errBody(t).Field != "header.Idempotency-Key" {
		t.Fatalf("other request after failure = %d %s", r.status, r.body)
	}
	if r := post(adminToken, "k2", `{}`); r.status != 422 || r.header.Get(api.IdempotencyReplayHeader) != "" || r.errBody(t).Field != "name" {
		t.Fatalf("same request after failure = %d %s", r.status, r.body)
	}
	// Invalid keys.
	if r := post(adminToken, strings.Repeat("x", 256), `{"name":"a"}`); r.status != 422 {
		t.Fatalf("long key = %d", r.status)
	}
	if n := testutil.Count(t, hs.db.Read, `SELECT count(*) FROM idempotency_keys`); n != 3 {
		t.Fatalf("stored keys = %d", n)
	}

	// After 24 h the key can be reused and cleanup removes old rows.
	before := hs.created.Load()
	hs.clock.Advance(api.IdempotencyTTL + time.Second)
	if r := post(adminToken, "k1", `{"name":"z"}`); r.status != 201 || r.header.Get(api.IdempotencyReplayHeader) != "" {
		t.Fatalf("expired key = %d %v", r.status, r.header)
	}
	if hs.created.Load() != before+1 {
		t.Fatal("expired key was replayed")
	}
	n, err := hs.api.CleanupIdempotency(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("cleanup = %d, %v", n, err)
	}
}

func TestIdempotencySkipsSensitiveResponses(t *testing.T) {
	hs := newHarness(t, api.Config{})
	for i := 0; i < 2; i++ {
		r := hs.do("POST", "/api/v1/secrets", adminToken, `{"name":"a"}`, api.IdempotencyHeader, "k")
		if r.status != 201 || r.header.Get("Cache-Control") != "no-store" || r.header.Get(api.IdempotencyReplayHeader) != "" {
			t.Fatalf("call %d = %d %v", i, r.status, r.header)
		}
	}
	if hs.created.Load() != 2 {
		t.Fatalf("handler ran %d times", hs.created.Load())
	}
	if n := testutil.Count(t, hs.db.Read, `SELECT count(*) FROM idempotency_keys WHERE length(response_body) > 0`); n != 0 {
		t.Fatalf("stored %d sensitive responses", n)
	}
}

func TestIdempotencyConcurrentRequest(t *testing.T) {
	hs := newHarness(t, api.Config{})
	hs.block, hs.entered = make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	var first resp
	wg.Add(1)
	go func() {
		defer wg.Done()
		first = hs.do("POST", "/api/v1/things", adminToken, `{"name":"a"}`, api.IdempotencyHeader, "same")
	}()
	<-hs.entered
	second := hs.do("POST", "/api/v1/things", adminToken, `{"name":"a"}`, api.IdempotencyHeader, "same")
	close(hs.block)
	wg.Wait()
	if second.status != 409 || second.errBody(t).Code != api.CodeConflict {
		t.Fatalf("concurrent = %d %s", second.status, second.body)
	}
	if first.status != 201 {
		t.Fatalf("first = %d", first.status)
	}
	hs.block = nil
	if r := hs.do("POST", "/api/v1/things", adminToken, `{"name":"a"}`, api.IdempotencyHeader, "same"); r.header.Get(api.IdempotencyReplayHeader) != "true" {
		t.Fatalf("after completion = %d %v", r.status, r.header)
	}
}

func TestETagIfMatch(t *testing.T) {
	hs := newHarness(t, api.Config{})
	tests := []struct {
		ifMatch string
		status  int
		code    string
	}{
		{"", 200, ""},
		{`"v3"`, 200, ""},
		{`W/"v3"`, 200, ""},
		{`*`, 200, ""},
		{`"v2"`, 412, api.CodePreconditionFailed},
		{`"abc"`, 422, api.CodeValidation},
	}
	for _, tt := range tests {
		t.Run(tt.ifMatch, func(t *testing.T) {
			var hdr []string
			if tt.ifMatch != "" {
				hdr = []string{"If-Match", tt.ifMatch}
			}
			r := hs.do("PATCH", "/api/v1/things/5", adminToken, `{"name":"n"}`, hdr...)
			if r.status != tt.status {
				t.Fatalf("status = %d, want %d: %s", r.status, tt.status, r.body)
			}
			if tt.code != "" {
				if e := r.errBody(t); e.Code != tt.code {
					t.Fatalf("code = %s", e.Code)
				}
			} else if r.header.Get("ETag") != `"v4"` {
				t.Fatalf("ETag = %q", r.header.Get("ETag"))
			}
		})
	}
}

func TestParseIfMatch(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"", 0, true}, {"*", 0, true}, {`"v12"`, 12, true}, {`W/"v12"`, 12, true}, {"v12", 12, true},
		{` "v7" `, 7, true}, {`"v0"`, 0, false}, {`"12"`, 0, false}, {`"v"`, 0, false}, {`"v1x"`, 0, false}, {`"v-3"`, 0, false},
	}
	for _, tt := range tests {
		got, err := api.ParseIfMatch(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("ParseIfMatch(%q) = %d, %v", tt.in, got, err)
		}
	}
	if api.ETag(12) != `"v12"` {
		t.Fatal(api.ETag(12))
	}
	if v, _ := api.ParseIfMatch(api.ETag(99)); v != 99 {
		t.Fatal("ETag does not round-trip")
	}
}

func TestPagination(t *testing.T) {
	hs := newHarness(t, api.Config{})
	var ids []int64
	cursor := ""
	for page := 0; ; page++ {
		path := "/api/v1/things?limit=50"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := hs.do("GET", path, readToken, "")
		if r.status != 200 {
			t.Fatalf("page %d = %d %s", page, r.status, r.body)
		}
		var body api.ListBody[thing]
		r.decode(t, &body)
		for _, x := range body.Items {
			ids = append(ids, x.ID)
		}
		if body.NextCursor == "" {
			break
		}
		cursor = body.NextCursor
		if page > 5 {
			t.Fatal("no end")
		}
	}
	if len(ids) != 120 || ids[0] != 1 || ids[119] != 120 {
		t.Fatalf("got %d ids", len(ids))
	}

	var def api.ListBody[thing]
	hs.do("GET", "/api/v1/things", readToken, "").decode(t, &def)
	if len(def.Items) != api.DefaultLimit {
		t.Fatalf("default page = %d", len(def.Items))
	}
	for _, bad := range []struct{ q, field string }{
		{"cursor=%21%21", "query.cursor"},
		{"cursor=" + "Zm9v", "query.cursor"},
		{"limit=501", "query.limit"},
		{"limit=0", "query.limit"},
	} {
		r := hs.do("GET", "/api/v1/things?"+bad.q, readToken, "")
		if r.status != 422 || r.errBody(t).Field != bad.field {
			t.Errorf("%s = %d %s", bad.q, r.status, r.body)
		}
	}
	empty := api.NewList[thing](nil, 10, func(x thing) int64 { return x.ID })
	if b, _ := json.Marshal(empty); string(b) != `{"items":[],"next_cursor":""}` {
		t.Fatalf("empty list = %s", b)
	}
	if id, err := api.DecodeCursor(api.EncodeCursor(42)); err != nil || id != 42 {
		t.Fatalf("cursor round trip = %d %v", id, err)
	}
}

func TestRoutesAndRegisterRules(t *testing.T) {
	hs := newHarness(t, api.Config{})
	got := map[string]api.RouteInfo{}
	for _, r := range hs.api.Routes() {
		got[r.OperationID] = r
	}
	want := map[string]api.Scope{"get-error": api.ScopeRead, "create-thing": api.ScopeAdmin, "preview-thing": api.ScopeRead,
		"update-thing": api.ScopeAdmin, "list-things": api.ScopeRead}
	for id, scope := range want {
		r, ok := got[id]
		if !ok || r.Scope != scope || !strings.HasPrefix(r.Path, api.Prefix+"/") {
			t.Errorf("route %s = %+v", id, r)
		}
	}

	mustPanic := func(name string, op huma.Operation) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: Register did not panic", name)
			}
		}()
		api.Register(hs.api, op, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	}
	base := huma.Operation{OperationID: "x", Method: "GET", Path: "/x", Summary: "s", Description: "d", Tags: []string{"T"}}
	for name, mutate := range map[string]func(*huma.Operation){
		"no id":          func(o *huma.Operation) { o.OperationID = "" },
		"no summary":     func(o *huma.Operation) { o.Summary = "" },
		"no description": func(o *huma.Operation) { o.Description = "" },
		"no tags":        func(o *huma.Operation) { o.Tags = nil },
		"bad scope":      func(o *huma.Operation) { o.Metadata = map[string]any{api.MetaScope: "root"} },
		"duplicate":      func(o *huma.Operation) { o.OperationID = "list-things"; o.Path = "/other" },
	} {
		op := base
		mutate(&op)
		mustPanic(name, op)
	}
}

func TestOpenAPISpec(t *testing.T) {
	hs := newHarness(t, api.Config{Version: "1.2.3", Guide: []byte("# Guide\n")})
	r := hs.do("GET", "/api/v1/openapi.json", "", "")
	var spec struct {
		OpenAPI string `json:"openapi"`
		Info    struct{ Title, Version string }
		Servers []struct{ URL string }
		Paths   map[string]map[string]struct {
			OperationID string                     `json:"operationId"`
			Security    []map[string][]string      `json:"security"`
			Responses   map[string]json.RawMessage `json:"responses"`
		}
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
		}
	}
	r.decode(t, &spec)
	if spec.OpenAPI != "3.1.0" || spec.Info.Title != "Mailroom API" || spec.Info.Version != "1.2.3" {
		t.Fatalf("info = %+v %s", spec.Info, spec.OpenAPI)
	}
	if len(spec.Servers) != 1 || spec.Servers[0].URL != "https://newsletter.example.com/api/v1" {
		t.Fatalf("servers = %+v", spec.Servers)
	}
	op := spec.Paths["/things"]["post"]
	if op.OperationID != "create-thing" || len(op.Security) != 1 {
		t.Fatalf("create-thing = %+v", op)
	}
	for _, code := range []string{"200", "201", "401", "403", "422", "429", "500"} {
		if _, ok := op.Responses[code]; !ok {
			t.Errorf("create-thing lacks %s", code)
		}
	}
	// MetaAlsoResponds: the extra status has the default response's body.
	var ok200, ok201 struct {
		Description string          `json:"description"`
		Content     json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(op.Responses["200"], &ok200)
	_ = json.Unmarshal(op.Responses["201"], &ok201)
	if ok200.Description != "Dry run: the thing that would be created" || len(ok201.Content) == 0 || string(ok200.Content) != string(ok201.Content) {
		t.Errorf("200 = %+v, 201 = %+v", ok200, ok201)
	}
	errSchema, ok := spec.Components.Schemas["Error"]
	if !ok {
		t.Fatalf("no Error schema; have %v", keys(spec.Components.Schemas))
	}
	if _, ok := errSchema.Properties["error"]; !ok {
		t.Fatalf("Error schema = %+v", errSchema)
	}
	if y := hs.do("GET", "/api/v1/openapi.yaml", "", ""); y.status != 200 || !strings.Contains(string(y.body), "Mailroom API") {
		t.Fatalf("yaml = %d", y.status)
	}
	if g := hs.do("GET", "/api/v1/docs", "", ""); string(g.body) != "# Guide\n" || !strings.HasPrefix(g.header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("guide = %q %v", g.body, g.header)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMapError(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("a: %w", fmt.Errorf("b: %w", core.ErrNotFound)), 404, api.CodeNotFound},
		{api.ErrUnauthorized, 401, api.CodeUnauthorized},
		{fmt.Errorf("x: %w", core.ErrSuppressed), 409, api.CodeConflict},
		{api.NewError(418, "teapot", "short and stout", ""), 418, "teapot"},
		{huma.Error404NotFound("gone"), 404, api.CodeNotFound},
		{context.Canceled, 500, api.CodeInternal},
	}
	for _, tt := range tests {
		se := api.MapError(tt.err)
		e, ok := se.(*api.Error)
		if !ok || se.GetStatus() != tt.status || e.Body.Code != tt.code {
			t.Errorf("MapError(%v) = %#v", tt.err, se)
		}
	}
	if api.MapError(nil) != nil {
		t.Fatal("MapError(nil) != nil")
	}
}

func TestDryRunTx(t *testing.T) {
	d := testutil.NewDB(t)
	ctx := context.Background()
	insert := func(key string) func(tx *sqlTx) error {
		return func(tx *sqlTx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, '{}', 'x')`, key)
			return err
		}
	}
	if err := api.Tx(ctx, d, true, insert("dry")); err != nil {
		t.Fatal(err)
	}
	if err := api.Tx(ctx, d, false, insert("wet")); err != nil {
		t.Fatal(err)
	}
	if n := testutil.Count(t, d.Read, `SELECT count(*) FROM settings`); n != 1 {
		t.Fatalf("rows = %d, want only the non-dry one", n)
	}
	boom := fmt.Errorf("boom")
	if err := api.Tx(ctx, d, true, func(*sqlTx) error { return boom }); err != boom {
		t.Fatalf("err = %v", err)
	}
}

type sqlTx = sql.Tx

// TestIdempotencyThroughRealServer: behind a real net/http server the
// request body can't be read after the handler (Huma) closed it; a replay
// of a JSON POST must still work.
func TestIdempotencyThroughRealServer(t *testing.T) {
	hs := newHarness(t, api.Config{})
	srv := httptest.NewServer(hs.h)
	defer srv.Close()
	post := func(body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/things", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set(api.IdempotencyHeader, "real-1")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	first, firstBody := post(`{"name":"a"}`)
	replay, replayBody := post(`{"name":"a"}`)
	if first.StatusCode != 201 || replay.StatusCode != 201 || replay.Header.Get(api.IdempotencyReplayHeader) != "true" || replayBody != firstBody {
		t.Fatalf("first %d %s, replay %d %q %s", first.StatusCode, firstBody, replay.StatusCode, replay.Header.Get(api.IdempotencyReplayHeader), replayBody)
	}
	if hs.created.Load() != 1 {
		t.Fatalf("handler ran %d times", hs.created.Load())
	}
	if other, _ := post(`{"name":"b"}`); other.StatusCode != 422 {
		t.Fatalf("different body under the same key = %d", other.StatusCode)
	}
}

// TestUnknownQueryParameterRefused: a query parameter the operation doesn't
// declare is a 422 on query.<name> and the handler never runs: a dry_run
// sent to an operation without dry runs, or a misspelled flag, must not
// carry out the real write.
func TestUnknownQueryParameterRefused(t *testing.T) {
	hs := newHarness(t, api.Config{})
	for _, tc := range []struct{ query, field string }{
		{"?dry_run=true", "query.dry_run"},
		{"?dryrun=true", "query.dryrun"},
	} {
		r := hs.do("POST", "/api/v1/things"+tc.query, adminToken, `{"name":"x"}`)
		if r.status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d: %s", tc.query, r.status, r.body)
		}
		if e := r.errBody(t); e.Code != api.CodeValidation || e.Field != tc.field || !strings.Contains(e.Message, "nothing was changed") {
			t.Fatalf("%s: error = %+v", tc.query, e)
		}
	}
	if hs.created.Load() != 0 {
		t.Fatal("a request with an unknown query parameter reached the handler")
	}
	// Declared parameters (here the pagination ones) still work.
	if r := hs.do("GET", "/api/v1/things?limit=5", adminToken, ""); r.status != http.StatusOK {
		t.Fatalf("declared parameter: %d %s", r.status, r.body)
	}
	if e := hs.do("GET", "/api/v1/things?limit=5&colour=red", adminToken, "").errBody(t); e.Field != "query.colour" || !strings.Contains(e.Message, "cursor, limit") {
		t.Fatalf("unknown list parameter: %+v", e)
	}
}
