package ui_test

// Test helpers shared by every UI test file (package ui_test): a fully
// wired app (internal/app, offline, fake clock and email provider), a
// signed-in browser-like client that keeps cookies, and response checks.
//
//	hs := newHarness(t)
//	c := hs.login()                       // signed in as hs.email
//	c.get("/admin/contacts").want(t, 200).contains(t, "Contacts")
//	c.post("/admin/contacts", url.Values{"email": {"a@example.org"}}).want(t, 303)
//	c.postNoCSRF("/admin/contacts", nil).want(t, 403)
//	hs.anon().get("/admin/contacts").wantLogin(t)

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/app"
	"mailroom/internal/auth"
	"mailroom/internal/config"
	"mailroom/internal/testutil"
)

// harness is a wired app with one admin user.
type harness struct {
	t        *testing.T
	ctx      context.Context
	app      *app.App
	h        http.Handler
	clock    *testutil.Clock
	provider *testutil.FakeProvider
	email    string // the admin user's email
	password string // and password
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
		"TIMEZONE":          "Europe/London",
	}
}

// newHarness builds the app. tune may adjust the options (nil: none).
func newHarness(t *testing.T, tune ...func(*app.Options)) *harness {
	t.Helper()
	vars := testVars(t)
	cfg, err := config.LoadFrom(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	hs := &harness{t: t, ctx: context.Background(), clock: testutil.NewClock(testutil.Epoch),
		provider: testutil.NewFakeProvider(), email: "owner@example.org"}
	opts := app.Options{Clock: hs.clock, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Offline: true, Provider: hs.provider, Version: "1.2.3-test"}
	for _, fn := range tune {
		fn(&opts)
	}
	a, err := app.New(hs.ctx, cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	hs.app, hs.h = a, a.Handler()
	if _, hs.password, err = app.CreateAdmin(hs.ctx, a.DB, hs.email, ""); err != nil {
		t.Fatal(err)
	}
	return hs
}

// client is a browser: it keeps cookies between requests.
type client struct {
	hs      *harness
	cookies map[string]*http.Cookie
	csrf    string // the session's CSRF token, once known
}

// anon returns a client without a session.
func (hs *harness) anon() *client { return &client{hs: hs, cookies: map[string]*http.Cookie{}} }

// login signs in through the login form and returns the signed-in client.
func (hs *harness) login() *client {
	hs.t.Helper()
	c := hs.anon()
	page := c.get("/admin/login").want(hs.t, http.StatusOK)
	tok := page.field(hs.t, "csrf_token")
	c.postRaw("/admin/login", url.Values{"csrf_token": {tok}, "email": {hs.email}, "password": {hs.password}, "next": {"/admin/"}}).
		want(hs.t, http.StatusSeeOther)
	c.loadCSRF()
	return c
}

// session signs in with auth.Login directly (no login form) and returns
// the client; faster to read in tests that aren't about signing in.
func (hs *harness) session() *client {
	hs.t.Helper()
	sess, err := hs.app.Auth.Login(hs.ctx, auth.LoginInput{Email: hs.email, Password: hs.password, IP: "192.0.2.10"})
	if err != nil {
		hs.t.Fatal(err)
	}
	c := hs.anon()
	c.cookies[api.SessionCookie] = &http.Cookie{Name: api.SessionCookie, Value: sess.Token}
	c.csrf = sess.CSRFToken
	return c
}

// loadCSRF reads the session's CSRF token from the dashboard.
func (c *client) loadCSRF() {
	c.hs.t.Helper()
	c.csrf = c.get("/admin/").want(c.hs.t, http.StatusOK).field(c.hs.t, "csrf_token")
}

// resp is a response.
type resp struct {
	code   int
	header http.Header
	body   string
}

// do sends req with the client's cookies and keeps the ones it sets.
func (c *client) do(req *http.Request) resp {
	req.RemoteAddr = "192.0.2.10:4321"
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	c.hs.h.ServeHTTP(rec, req)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck
		}
	}
	return resp{code: rec.Code, header: rec.Header(), body: rec.Body.String()}
}

// get requests path.
func (c *client) get(path string) resp {
	return c.do(httptest.NewRequest(http.MethodGet, path, nil))
}

// post submits a form with the session's CSRF token.
func (c *client) post(path string, form url.Values) resp {
	f := url.Values{}
	for k, v := range form {
		f[k] = v
	}
	f.Set("csrf_token", c.csrf)
	return c.postRaw(path, f)
}

// postNoCSRF submits a form without a CSRF token (expect 403).
func (c *client) postNoCSRF(path string, form url.Values) resp { return c.postRaw(path, form) }

// postRaw submits form as it is.
func (c *client) postRaw(path string, form url.Values) resp {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

// htmx sends an htmx request (HX-Request, CSRF header) with a form body.
func (c *client) htmx(method, path string, form url.Values) resp {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("HX-Request", "true")
	if c.csrf != "" {
		req.Header.Set(api.CSRFHeader, c.csrf)
	}
	return c.do(req)
}

// want fails unless the response has the status.
func (r resp) want(t *testing.T, code int) resp {
	t.Helper()
	if r.code != code {
		t.Fatalf("status %d, want %d; Location %q\n%.3000s", r.code, code, r.header.Get("Location"), r.body)
	}
	return r
}

// wantRedirect fails unless the response redirects (3xx with Location)
// to a URL starting with prefix.
func (r resp) wantRedirect(t *testing.T, prefix string) resp {
	t.Helper()
	if r.code < 300 || r.code > 308 || r.code == http.StatusNotModified {
		t.Fatalf("status %d, want a redirect to %s\n%.2000s", r.code, prefix, r.body)
	}
	if loc := r.header.Get("Location"); !strings.HasPrefix(loc, prefix) {
		t.Fatalf("redirect to %q, want %s...", loc, prefix)
	}
	return r
}

// wantLogin fails unless the response sends a logged-out user to the
// login page.
func (r resp) wantLogin(t *testing.T) resp {
	t.Helper()
	return r.wantRedirect(t, "/admin/login")
}

// contains fails unless the body contains every string.
func (r resp) contains(t *testing.T, subs ...string) resp {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(r.body, s) {
			t.Fatalf("body lacks %q:\n%.4000s", s, r.body)
		}
	}
	return r
}

// lacks fails if the body contains any of the strings.
func (r resp) lacks(t *testing.T, subs ...string) resp {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(r.body, s) {
			t.Fatalf("body contains %q:\n%.4000s", s, r.body)
		}
	}
	return r
}

// field returns the value of the first input named name.
func (r resp) field(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`).FindStringSubmatch(r.body)
	if m == nil {
		t.Fatalf("no input %q in:\n%.3000s", name, r.body)
	}
	return htmlUnescape(m[1])
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&#34;", `"`, "&#39;", "'", "&#43;", "+").Replace(s)
}

// count runs a count query (test assertions only).
func (hs *harness) count(query string, args ...any) int {
	hs.t.Helper()
	return testutil.Count(hs.t, hs.app.DB.Read, query, args...)
}
