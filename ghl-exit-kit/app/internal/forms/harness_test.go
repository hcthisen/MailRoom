package forms_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/config"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/forms"
	"mailroom/internal/hooks"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

const (
	baseURL    = "https://newsletter.example.com"
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

// enrollCall is one recorded Enroll call.
type enrollCall struct {
	SequenceID, ContactID int64
	Source                string
}

// fakeEnroller records enrolments; it writes nothing.
type fakeEnroller struct {
	mu    sync.Mutex
	calls []enrollCall
	err   error
}

func (f *fakeEnroller) Enroll(_ context.Context, tx *sql.Tx, sequenceID, contactID int64, opts core.EnrollOptions) (int64, error) {
	if tx == nil {
		return 0, fmt.Errorf("Enroll called without a transaction")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.calls = append(f.calls, enrollCall{sequenceID, contactID, opts.Source})
	return int64(len(f.calls)), nil
}

func (f *fakeEnroller) Calls() []enrollCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]enrollCall(nil), f.calls...)
}

// fakeAlerter records alerts by key; it writes nothing.
type fakeAlerter struct {
	mu   sync.Mutex
	keys []string
}

func (f *fakeAlerter) Alert(_ context.Context, _ *sql.Tx, key, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	return nil
}

func (f *fakeAlerter) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

type harness struct {
	t        *testing.T
	alerts   *fakeAlerter
	d        *db.DB
	clock    *testutil.Clock
	store    *contactstore.Store
	enroller *fakeEnroller
	svc      *forms.Service
	api      http.Handler
	public   *http.ServeMux

	mu       sync.Mutex
	tagAdded []hooks.TagAdded
	ipSeq    int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, d: testutil.NewDB(t), clock: testutil.NewClock(time.Time{}), enroller: &fakeEnroller{}, alerts: &fakeAlerter{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := hooks.New()
	bus.OnTagAdded(func(_ context.Context, _ *sql.Tx, e hooks.TagAdded) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.tagAdded = append(h.tagAdded, e)
		return nil
	})
	h.store = &contactstore.Store{DB: h.d, Bus: bus, Clock: h.clock}
	h.svc = forms.New(forms.Deps{
		DB: h.d, Store: h.store, Enroller: h.enroller, Alerter: h.alerts,
		Settings: settings.New(settings.Deps{DB: h.d, Clock: h.clock, Domain: "example.com"}),
		Signer:   testutil.NewSigner(t, h.clock),
		Config:   config.Config{BaseURL: baseURL},
		Clock:    h.clock, Logger: logger,
	})
	a, err := api.New(api.Config{DB: h.d, Clock: h.clock, Logger: logger, Auth: fakeAuth{}, BaseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.RegisterAPI(a)
	h.api = a.Handler()
	h.public = http.NewServeMux()
	h.svc.Mount(h.public)
	return h
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

func (r resp) wantStatus(t *testing.T, status int) resp {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
	return r
}

// wantError checks an API-format error response.
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

func (r resp) text() string { return string(r.body) }

// plain returns the body with HTML entities decoded, for text checks.
func (r resp) plain() string { return html.UnescapeString(string(r.body)) }

// do sends an API request with the admin token.
func (h *harness) do(method, path, body string, headers ...string) resp {
	h.t.Helper()
	return h.doAs(adminToken, method, path, body, headers...)
}

func (h *harness) doAs(token, method, path, body string, headers ...string) resp {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
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
	h.api.ServeHTTP(rec, req)
	return resp{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// createForm creates a form through the API.
func (h *harness) createForm(body string) forms.Form {
	h.t.Helper()
	var f forms.Form
	h.do("POST", "/forms", body).wantStatus(h.t, http.StatusCreated).decode(h.t, &f)
	return f
}

// nextIP returns a fresh client address, so tests don't hit the per-IP
// rate limit unless they want to.
func (h *harness) nextIP() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ipSeq++
	return fmt.Sprintf("198.51.100.%d:5555", h.ipSeq%250+1)
}

// public sends a request to the public mux. headers are name/value pairs;
// "RemoteAddr" sets the client address (default: a fresh one).
func (h *harness) publicReq(method, path, contentType, body string, headers ...string) resp {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = h.nextIP()
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "RemoteAddr" {
			req.RemoteAddr = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.public.ServeHTTP(rec, req)
	return resp{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// submit posts form-encoded values to /f/{publicID}.
func (h *harness) submit(publicID string, values url.Values, headers ...string) resp {
	h.t.Helper()
	return h.publicReq("POST", "/f/"+publicID, "application/x-www-form-urlencoded", values.Encode(), headers...)
}

// submitJSON posts a JSON body to /f/{publicID}.
func (h *harness) submitJSON(publicID, body string, headers ...string) resp {
	h.t.Helper()
	return h.publicReq("POST", "/f/"+publicID, "application/json", body, headers...)
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	return testutil.Count(h.t, h.d.Read, query, args...)
}

// contact returns the contact with the address.
func (h *harness) contact(email string) core.Contact {
	h.t.Helper()
	c, err := h.store.GetByEmail(context.Background(), nil, email)
	if err != nil {
		h.t.Fatalf("contact %s: %v", email, err)
	}
	return c
}

// lastOutcome returns the newest submission outcome of a form.
func (h *harness) lastOutcome(formID int64) string {
	h.t.Helper()
	var o string
	if err := h.d.Read.QueryRow(`SELECT outcome FROM form_submissions WHERE form_id = ? ORDER BY id DESC LIMIT 1`, formID).Scan(&o); err != nil {
		h.t.Fatalf("last outcome: %v", err)
	}
	return o
}

// optinMessage is a queued confirmation email.
type optinMessage struct {
	ID         int64
	Template   string
	ToEmail    string
	ConfirmURL string
	FormName   string
}

// optinMessages returns the confirmation emails queued for a contact, oldest first.
func (h *harness) optinMessages(contactID int64) []optinMessage {
	h.t.Helper()
	rows, err := h.d.Read.Query(`SELECT id, system_template, COALESCE(to_email, ''), data FROM messages
		WHERE contact_id = ? AND kind = 'optin' ORDER BY id`, contactID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []optinMessage
	for rows.Next() {
		var m optinMessage
		var data string
		if err := rows.Scan(&m.ID, &m.Template, &m.ToEmail, &data); err != nil {
			h.t.Fatal(err)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			h.t.Fatal(err)
		}
		m.ConfirmURL, _ = d["confirm_url"].(string)
		m.FormName, _ = d["form_name"].(string)
		out = append(out, m)
	}
	return out
}

// confirmPath returns the path of the newest confirmation link for a contact.
func (h *harness) confirmPath(contactID int64) string {
	h.t.Helper()
	ms := h.optinMessages(contactID)
	if len(ms) == 0 {
		h.t.Fatalf("no confirmation email for contact %d", contactID)
	}
	p, ok := strings.CutPrefix(ms[len(ms)-1].ConfirmURL, baseURL)
	if !ok || !strings.HasPrefix(p, "/f/confirm/") {
		h.t.Fatalf("confirm URL = %q", ms[len(ms)-1].ConfirmURL)
	}
	return p
}

// consentEvent is a consent_events row.
type consentEvent struct {
	Type, Source, IP, UserAgent, ConsentText string
	FormID                                   sql.NullInt64
}

func (h *harness) consents(contactID int64) []consentEvent {
	h.t.Helper()
	rows, err := h.d.Read.Query(`SELECT type, source, ip, user_agent, consent_text, form_id FROM consent_events WHERE contact_id = ? ORDER BY id`, contactID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []consentEvent
	for rows.Next() {
		var e consentEvent
		if err := rows.Scan(&e.Type, &e.Source, &e.IP, &e.UserAgent, &e.ConsentText, &e.FormID); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func (h *harness) tagEvents() []hooks.TagAdded {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hooks.TagAdded(nil), h.tagAdded...)
}

// setStatus moves a contact to a status directly in the database.
func (h *harness) setStatus(id int64, status core.ContactStatus) {
	h.t.Helper()
	testutil.Exec(h.t, h.d, `UPDATE contacts SET status = ? WHERE id = ?`, string(status), id)
}

func vals(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}
