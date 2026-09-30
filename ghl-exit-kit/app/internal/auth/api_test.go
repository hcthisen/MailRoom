package auth_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/auth"
	"mailroom/internal/testutil"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

type client struct {
	t *testing.T
	h http.Handler
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) code() string {
	var e struct{ Error api.ErrorBody }
	_ = json.Unmarshal(r.body, &e)
	return e.Error.Code
}

func (c client) do(method, path, body string, headers ...string) response {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "198.51.100.7:5555"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return response{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

func bearer(secret string) []string { return []string{"Authorization", "Bearer " + secret} }

func TestAPIEndToEnd(t *testing.T) {
	f := newFixture(t)
	a, err := api.New(api.Config{DB: f.d, Clock: f.clock, BaseURL: "https://newsletter.example.com", Auth: f.svc})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.RegisterAPI(a)
	audit.New(f.d).RegisterAPI(a)
	c := client{t: t, h: a.Handler()}

	// Bootstrap: the first admin token comes from the CLI.
	_, admin, err := f.svc.CreateCLIToken(f.ctx, auth.TokenInput{Name: "setup-agent", Scope: api.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}

	r := c.do("POST", "/api/v1/tokens", `{"name":"reporting","scope":"read"}`, append(bearer(admin), api.IdempotencyHeader, "tok-1")...)
	if r.status != 201 || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create token = %d %s", r.status, r.body)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM idempotency_keys WHERE length(response_body) > 0`); n != 0 {
		t.Fatal("a token secret was stored for idempotent replay")
	}
	// The key was recorded with the new token, so a retry can't create a
	// second one; it answers 409 instead of replaying the secret.
	if r := c.do("POST", "/api/v1/tokens", `{"name":"reporting","scope":"read"}`, append(bearer(admin), api.IdempotencyHeader, "tok-1")...); r.status != 409 {
		t.Fatalf("retry = %d %s", r.status, r.body)
	}
	var created struct {
		Token  auth.Token `json:"token"`
		Secret string     `json:"secret"`
	}
	_ = json.Unmarshal(r.body, &created)
	if created.Token.Scope != api.ScopeRead || created.Token.CreatedBy == "" || !strings.HasPrefix(created.Secret, "mr_") {
		t.Fatalf("created = %s", r.body)
	}
	read := created.Secret

	// /me reports the caller.
	r = c.do("GET", "/api/v1/me", "", bearer(read)...)
	var me struct {
		Actor auth.MeActor `json:"actor"`
		Scope api.Scope    `json:"scope"`
		Token *auth.Token  `json:"token"`
	}
	_ = json.Unmarshal(r.body, &me)
	if r.status != 200 || me.Scope != api.ScopeRead || me.Actor.Name != "reporting" || me.Token == nil || me.Token.ID != created.Token.ID {
		t.Fatalf("me = %d %s", r.status, r.body)
	}

	// Scopes.
	if r := c.do("GET", "/api/v1/tokens", "", bearer(read)...); r.status != 200 {
		t.Fatalf("read GET = %d", r.status)
	}
	if r := c.do("POST", "/api/v1/tokens", `{"name":"sneaky","scope":"admin"}`, bearer(read)...); r.status != 403 || r.code() != api.CodeForbidden {
		t.Fatalf("read POST = %d %s", r.status, r.body)
	}
	if r := c.do("POST", "/api/v1/tokens", `{"name":"x","scope":"root"}`, bearer(admin)...); r.status != 422 {
		t.Fatalf("bad scope = %d", r.status)
	}
	if r := c.do("POST", "/api/v1/tokens", `{"name":"reporting","scope":"read"}`, bearer(admin)...); r.status != 409 {
		t.Fatalf("duplicate name = %d", r.status)
	}

	// Expired token.
	exp := f.clock.Now().Add(time.Minute).Format(time.RFC3339)
	r = c.do("POST", "/api/v1/tokens", `{"name":"short","scope":"admin","expires_at":"`+exp+`"}`, bearer(admin)...)
	_ = json.Unmarshal(r.body, &created)
	short := created.Secret
	if r := c.do("GET", "/api/v1/me", "", bearer(short)...); r.status != 200 {
		t.Fatalf("fresh token = %d", r.status)
	}
	f.clock.Advance(time.Minute)
	if r := c.do("GET", "/api/v1/me", "", bearer(short)...); r.status != 401 || r.code() != api.CodeUnauthorized {
		t.Fatalf("expired token = %d %s", r.status, r.body)
	}

	// get-token: a read token may read it; the secret is never returned.
	r = c.do("GET", "/api/v1/tokens/"+itoa(created.Token.ID), "", bearer(read)...)
	var got auth.Token
	if err := json.Unmarshal(r.body, &got); r.status != 200 || err != nil || got.ID != created.Token.ID || got.Name != "short" ||
		strings.Contains(string(r.body), short) {
		t.Fatalf("get-token = %d %s", r.status, r.body)
	}
	if r := c.do("GET", "/api/v1/tokens/99999", "", bearer(admin)...); r.status != 404 || r.code() != api.CodeNotFound {
		t.Fatalf("get unknown token = %d %s", r.status, r.body)
	}

	// Revoked token.
	if r := c.do("DELETE", "/api/v1/tokens/"+itoa(created.Token.ID), "", bearer(admin)...); r.status != 204 {
		t.Fatalf("revoke = %d %s", r.status, r.body)
	}
	var list api.ListBody[auth.Token]
	_ = json.Unmarshal(c.do("GET", "/api/v1/tokens", "", bearer(admin)...).body, &list)
	if len(list.Items) != 2 {
		t.Fatalf("active tokens = %d", len(list.Items))
	}
	_ = json.Unmarshal(c.do("GET", "/api/v1/tokens?include_revoked=true", "", bearer(admin)...).body, &list)
	if len(list.Items) != 3 {
		t.Fatalf("all tokens = %d", len(list.Items))
	}

	// Users.
	r = c.do("POST", "/api/v1/users", `{"email":"owner@example.com","name":"Owner"}`, bearer(admin)...)
	var cu struct {
		User              auth.User `json:"user"`
		GeneratedPassword string    `json:"generated_password"`
	}
	_ = json.Unmarshal(r.body, &cu)
	if r.status != 201 || cu.User.ID == 0 || len(cu.GeneratedPassword) != 24 {
		t.Fatalf("create user = %d %s", r.status, r.body)
	}
	uid := itoa(cu.User.ID)
	if r := c.do("POST", "/api/v1/users", `{"email":"x@example.com","password":"short"}`, bearer(admin)...); r.status != 422 {
		t.Fatalf("short password = %d", r.status)
	}
	r = c.do("GET", "/api/v1/users/"+uid, "", bearer(read)...)
	if r.status != 200 || r.header.Get("ETag") != `"v1"` || strings.Contains(string(r.body), "argon2") {
		t.Fatalf("get user = %d %v %s", r.status, r.header, r.body)
	}
	r = c.do("PATCH", "/api/v1/users/"+uid, `{"name":"Alex"}`, append(bearer(admin), "If-Match", `"v1"`)...)
	if r.status != 200 || r.header.Get("ETag") != `"v2"` {
		t.Fatalf("patch user = %d %s", r.status, r.body)
	}
	if r := c.do("PATCH", "/api/v1/users/"+uid, `{"name":"Sam"}`, append(bearer(admin), "If-Match", `"v1"`)...); r.status != 412 || r.code() != api.CodePreconditionFailed {
		t.Fatalf("stale patch = %d %s", r.status, r.body)
	}
	if r := c.do("GET", "/api/v1/users/999", "", bearer(admin)...); r.status != 404 {
		t.Fatalf("missing user = %d", r.status)
	}

	// TOTP enrolment and reset through the API.
	r = c.do("POST", "/api/v1/users/"+uid+"/totp", "", bearer(admin)...)
	var enr auth.TOTPEnrollment
	_ = json.Unmarshal(r.body, &enr)
	if r.status != 200 || enr.Secret == "" || !strings.HasPrefix(enr.OtpauthURI, "otpauth://totp/") {
		t.Fatalf("begin totp = %d %s", r.status, r.body)
	}
	code, _ := auth.TOTPCode(enr.Secret, f.clock.Now())
	r = c.do("POST", "/api/v1/users/"+uid+"/totp/confirm", `{"secret":"`+enr.Secret+`","code":"`+code+`"}`, bearer(admin)...)
	if r.status != 200 || !strings.Contains(string(r.body), `"totp_enabled":true`) {
		t.Fatalf("confirm totp = %d %s", r.status, r.body)
	}
	if r := c.do("DELETE", "/api/v1/users/"+uid+"/totp", "", append(bearer(admin), "If-Match", `"v1"`)...); r.status != 412 {
		t.Fatalf("stale reset-user-totp = %d %s", r.status, r.body)
	}
	if r := c.do("DELETE", "/api/v1/users/"+uid+"/totp", "", bearer(admin)...); r.status != 200 || !strings.Contains(string(r.body), `"totp_enabled":false`) {
		t.Fatalf("reset totp = %d %s", r.status, r.body)
	}

	// A UI session: GETs need only the cookie, writes need the CSRF token too.
	sess, err := f.svc.Login(f.ctx, auth.LoginInput{Email: "owner@example.com", Password: cu.GeneratedPassword, IP: "198.51.100.7"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := []string{"Cookie", api.SessionCookie + "=" + sess.Token}
	r = c.do("GET", "/api/v1/me", "", cookie...)
	if r.status != 200 || !strings.Contains(string(r.body), `"email":"owner@example.com"`) {
		t.Fatalf("session me = %d %s", r.status, r.body)
	}
	if r := c.do("POST", "/api/v1/tokens", `{"name":"ui","scope":"read"}`, cookie...); r.status != 403 {
		t.Fatalf("session POST without CSRF = %d", r.status)
	}
	if r := c.do("POST", "/api/v1/tokens", `{"name":"ui","scope":"read"}`, append(cookie, api.CSRFHeader, sess.CSRFToken)...); r.status != 201 {
		t.Fatalf("session POST with CSRF = %d %s", r.status, r.body)
	}

	// Deleting: the last user is protected.
	if r := c.do("DELETE", "/api/v1/users/"+uid, "", bearer(admin)...); r.status != 409 {
		t.Fatalf("delete last user = %d", r.status)
	}

	// Audit trail with actors.
	var entries api.ListBody[audit.Record]
	_ = json.Unmarshal(c.do("GET", "/api/v1/audit?operation=token.create", "", bearer(admin)...).body, &entries)
	actors := map[string]bool{}
	for _, e := range entries.Items {
		actors[e.Actor.Type+":"+e.Actor.Name] = true
	}
	if len(entries.Items) != 4 || !actors["cli:"] || !actors["token:setup-agent"] || !actors["user:owner@example.com"] {
		t.Fatalf("token.create audit = %+v", entries.Items)
	}

	// No secret appears in list responses.
	if body := string(c.do("GET", "/api/v1/tokens?include_revoked=true", "", bearer(admin)...).body); strings.Contains(body, admin) || strings.Contains(body, read) {
		t.Fatal("token secret listed")
	}
}
