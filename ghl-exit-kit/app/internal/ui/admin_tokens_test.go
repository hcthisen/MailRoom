package ui_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
)

func TestAdminTokens(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	c.get("/admin/tokens").want(t, http.StatusOK).contains(t, "No API tokens", "Create a token", `<option value="read" selected>`)

	// Validation errors land on the field.
	c.post("/admin/tokens", url.Values{"name": {""}, "scope": {"read"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-name-error"`, "is required")
	c.post("/admin/tokens", url.Values{"name": {"old"}, "scope": {"read"}, "expires_at": {"2001-01-01T10:00"}}).
		want(t, http.StatusUnprocessableEntity).contains(t, `id="f-expires_at-error"`, "must be in the future")

	// Create: the secret is shown once, on the response itself.
	r := c.post("/admin/tokens", url.Values{"name": {"website-sync"}, "scope": {"read"}}).want(t, http.StatusOK).
		contains(t, "Copy the token now", "This is the only time the token is shown", `data-copy="token-secret"`)
	m := regexp.MustCompile(`value="(mr_[a-z2-7]+)"`).FindStringSubmatch(r.body)
	if m == nil {
		t.Fatalf("no secret in:\n%s", r.body)
	}
	secret := m[1]
	if r.header.Get("Set-Cookie") != "" && regexp.MustCompile(regexp.QuoteMeta(secret)).MatchString(r.header.Get("Set-Cookie")) {
		t.Fatal("the secret went into a cookie")
	}
	me := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := me(); code != http.StatusOK {
		t.Fatalf("new token: GET /me = %d", code)
	}
	list := c.get("/admin/tokens").want(t, http.StatusOK).contains(t, "website-sync", secret[:12]).lacks(t, secret)
	_ = list
	toks, err := hs.app.Auth.ListTokens(hs.ctx, 0, 10, false)
	if err != nil || len(toks) != 1 || toks[0].Scope != "read" {
		t.Fatalf("tokens = %+v, %v", toks, err)
	}
	// The same name again: a conflict above the form.
	c.post("/admin/tokens", url.Values{"name": {"website-sync"}, "scope": {"admin"}}).want(t, http.StatusConflict).
		contains(t, `class="form-error"`, "already exists")

	// Revoke asks first, naming the token.
	path := "/admin/tokens/" + admItoa(toks[0].ID) + "/revoke"
	c.post(path, nil).want(t, http.StatusOK).contains(t, "Revoke this token?", "Name: website-sync", "stops working immediately", "Last used")
	if code := me(); code != http.StatusOK {
		t.Fatalf("the confirmation step revoked the token: %d", code)
	}
	c.post(path, url.Values{"confirm": {"yes"}}).wantRedirect(t, "/admin/tokens")
	if code := me(); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: GET /me = %d", code)
	}
	c.get("/admin/tokens").want(t, http.StatusOK).contains(t, `The token &#34;website-sync&#34; is revoked.`, "No API tokens")
	c.get("/admin/tokens?revoked=1").want(t, http.StatusOK).contains(t, "website-sync", "revoked")
	c.post(path, nil).wantRedirect(t, "/admin/tokens")
	c.post("/admin/tokens/999/revoke", nil).want(t, http.StatusNotFound)
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation IN ('token.create', 'token.revoke') AND actor_name = ?`, hs.email); n != 2 {
		t.Fatalf("token audit entries = %d", n)
	}
}
