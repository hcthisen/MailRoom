package ui_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/static"
)

func TestRootRedirectsToAdmin(t *testing.T) {
	hs := newHarness(t)
	r := hs.anon().get("/").wantRedirect(t, "/admin/")
	if loc := r.header.Get("Location"); loc != "/admin/" {
		t.Fatalf("Location = %q", loc)
	}
	hs.anon().get("/admin").wantRedirect(t, "/admin/")
}

func TestLoggedOutGoesToLogin(t *testing.T) {
	hs := newHarness(t)
	c := hs.anon()
	if loc := c.get("/admin/").wantLogin(t).header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("Location = %q", loc)
	}
	loc := c.get("/admin/contacts?status=active").wantLogin(t).header.Get("Location")
	if loc != "/admin/login?next="+url.QueryEscape("/admin/contacts?status=active") {
		t.Fatalf("Location = %q", loc)
	}
	// A POST without a session is not carried out.
	c.postRaw("/admin/preview/email", url.Values{"subject": {"x"}}).wantLogin(t)
	// htmx is told to load the login page.
	r := c.htmx(http.MethodPost, "/admin/preview/email", url.Values{"subject": {"x"}}).want(t, http.StatusUnauthorized)
	if r.header.Get("HX-Redirect") != "/admin/login" {
		t.Fatalf("HX-Redirect = %q", r.header.Get("HX-Redirect"))
	}
	// An invalid session cookie is treated as logged out and cleared.
	c.cookies[api.SessionCookie] = &http.Cookie{Name: api.SessionCookie, Value: "forged"}
	c.get("/admin/").wantLogin(t)
	if _, ok := c.cookies[api.SessionCookie]; ok {
		t.Fatal("the invalid session cookie was not cleared")
	}
}

func TestLoginLogout(t *testing.T) {
	hs := newHarness(t)
	c := hs.anon()
	page := c.get("/admin/login?next="+url.QueryEscape("/admin/segments")).want(t, http.StatusOK).
		contains(t, `name="email"`, `name="password"`, `name="totp_code"`, "Authenticator code", `value="/admin/segments"`)
	tok := page.field(t, "csrf_token")

	// Without the login form's token: refused.
	c.postRaw("/admin/login", url.Values{"email": {hs.email}, "password": {hs.password}}).want(t, http.StatusForbidden)
	// Wrong password: the form again with one generic message, the email kept.
	c.postRaw("/admin/login", url.Values{"csrf_token": {tok}, "email": {hs.email}, "password": {"wrong password!"}}).
		want(t, http.StatusUnauthorized).contains(t, "Invalid email, password or authenticator code.", `value="owner@example.org"`).
		lacks(t, "wrong password!")
	// Right password: redirected to next, with a session cookie.
	c.postRaw("/admin/login", url.Values{"csrf_token": {tok}, "email": {"Owner@Example.org"}, "password": {hs.password},
		"next": {"/admin/segments"}}).wantRedirect(t, "/admin/segments")
	ck := c.cookies[api.SessionCookie]
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %+v", ck)
	}
	// Signed in, the login page goes to the dashboard.
	c.get("/admin/login").wantRedirect(t, "/admin/")
	c.loadCSRF()
	c.get("/admin/").want(t, http.StatusOK).contains(t, "owner@example.org", "Sign out", "Mailroom 1.2.3-test", "Example Ltd")

	// Logout needs the CSRF token.
	c.postNoCSRF("/admin/logout", nil).want(t, http.StatusForbidden).contains(t, "Request refused")
	c.post("/admin/logout", nil).wantRedirect(t, "/admin/login")
	c.get("/admin/login").want(t, http.StatusOK).contains(t, "You are signed out.")
	c.get("/admin/").wantLogin(t)
	if n := hs.count(`SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("%d sessions after logout", n)
	}
}

func TestLoginNextStaysLocal(t *testing.T) {
	hs := newHarness(t)
	for _, next := range []string{"https://evil.example/", "//evil.example/admin/", "/api/v1/me", "/admin/login", "/admin\\x"} {
		c := hs.anon()
		tok := c.get("/admin/login").field(t, "csrf_token")
		loc := c.postRaw("/admin/login", url.Values{"csrf_token": {tok}, "email": {hs.email}, "password": {hs.password}, "next": {next}}).
			want(t, http.StatusSeeOther).header.Get("Location")
		if loc != "/admin/" {
			t.Errorf("next %q redirected to %q", next, loc)
		}
	}
}

func TestLoginRateLimited(t *testing.T) {
	hs := newHarness(t)
	c := hs.anon()
	tok := c.get("/admin/login").field(t, "csrf_token")
	var last resp
	for i := 0; i < 6; i++ {
		last = c.postRaw("/admin/login", url.Values{"csrf_token": {tok}, "email": {hs.email}, "password": {"not the password"}})
	}
	last.want(t, http.StatusTooManyRequests).contains(t, "too many failed logins")
}

func TestSecurityHeaders(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	for _, path := range []string{"/admin/", "/admin/login", "/admin/no-such-page"} {
		h := c.get(path).header
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-eval") || strings.Contains(strings.SplitN(strings.SplitN(csp, "script-src", 2)[1], ";", 2)[0], "unsafe-inline") {
			t.Errorf("%s: CSP allows inline or eval scripts: %q", path, csp)
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "same-origin" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: headers %v", path, h)
		}
	}
}

func TestNoInlineScripts(t *testing.T) {
	hs := newHarness(t)
	body := hs.session().get("/admin/").want(t, http.StatusOK).body
	for _, bad := range []string{"<script>", " onclick=", " onload=", "javascript:", "hx-on"} {
		if strings.Contains(body, bad) {
			t.Errorf("the dashboard contains %q", bad)
		}
	}
	if !strings.Contains(body, `<script src="/admin/static/htmx.min.js?v=`) {
		t.Error("htmx is not loaded from /admin/static")
	}
}

func TestStaticFiles(t *testing.T) {
	hs := newHarness(t)
	c := hs.anon()
	body := hs.session().get("/admin/").body
	i := strings.Index(body, "/admin/static/app.css?v=")
	if i < 0 {
		t.Fatal("no versioned app.css link")
	}
	href := body[i : i+strings.IndexByte(body[i:], '"')]
	r := c.get(href).want(t, http.StatusOK)
	if r.header.Get("Cache-Control") != "public, max-age=31536000, immutable" || !strings.HasPrefix(r.header.Get("Content-Type"), "text/css") {
		t.Fatalf("headers %v", r.header)
	}
	if !strings.Contains(r.body, "prefers-color-scheme: dark") {
		t.Fatal("app.css has no dark mode")
	}
	// Without the current hash: served, but cached briefly.
	if cc := c.get("/admin/static/htmx.min.js").want(t, http.StatusOK).header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	c.get("/admin/static/VENDOR.md").want(t, http.StatusNotFound)
	c.get("/admin/static/nope.js").want(t, http.StatusNotFound)
	etag := r.header.Get("ETag")
	req := httptest.NewRequest(http.MethodGet, href, nil)
	req.Header.Set("If-None-Match", etag)
	c.do(req).want(t, http.StatusNotModified)
}

// TestStaticLayoutRules pins CSS and script rules the pages rely on.
func TestStaticLayoutRules(t *testing.T) {
	hs := newHarness(t)
	c := hs.anon()
	css := c.get("/admin/static/app.css").want(t, http.StatusOK).body
	compact := func(s string) string { return strings.Join(strings.Fields(s), "") }
	cssC := compact(css)
	for _, rule := range []string{
		// The main column may shrink below its content, so wide tables
		// scroll inside .table-wrap instead of widening the page on phones
		// (a grid item's min-width is auto, and 1fr is minmax(auto, 1fr)).
		"min-width:0;}",
		"grid-template-columns:minmax(0,1fr);",
		// The hidden attribute wins over .btn's display (copy buttons).
		"[hidden]{display:none!important;}",
		".table-wrap{position:relative;",
		"h1{font-size:1.5rem;overflow-wrap:anywhere;}",
		".subnav{",
	} {
		if !strings.Contains(cssC, rule) {
			t.Errorf("app.css lacks %q", rule)
		}
	}
	if i := strings.Index(css, ".main {"); i < 0 || !strings.Contains(css[i:i+strings.IndexByte(css[i:], '}')], "min-width: 0") {
		t.Error(".main has no min-width: 0")
	}
	if strings.Contains(cssC, ".app{grid-template-columns:1fr;") {
		t.Error("the phone layout uses 1fr (minmax(auto, 1fr)) for the grid column")
	}
	js := c.get("/admin/static/app.js").want(t, http.StatusOK).body
	for _, s := range []string{`button[data-copy]`, "[data-copy-wrap]", "navigator.clipboard", `execCommand("copy")`} {
		if !strings.Contains(js, s) {
			t.Errorf("app.js lacks %q (the generic copy-button handler)", s)
		}
	}
}

// TestVendoredHTMX: the embedded htmx.min.js is the one VENDOR.md
// records (version 2.x and SHA-256).
func TestVendoredHTMX(t *testing.T) {
	js, err := static.FS.ReadFile("htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(js)
	vendor, err := static.FS.ReadFile("VENDOR.md")
	if err != nil {
		t.Fatal(err)
	}
	var row string
	sc := bufio.NewScanner(strings.NewReader(string(vendor)))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "| `htmx.min.js`") {
			row = sc.Text()
		}
	}
	if !strings.Contains(row, hex.EncodeToString(sum[:])) {
		t.Fatalf("VENDOR.md htmx row %q does not record the SHA-256 %x of the embedded file", row, sum)
	}
	if !strings.Contains(row, "| 2.") {
		t.Fatalf("VENDOR.md htmx row %q is not a 2.x version", row)
	}
}

func TestNotFoundPage(t *testing.T) {
	hs := newHarness(t)
	hs.session().get("/admin/no-such-page").want(t, http.StatusNotFound).contains(t, "Page not found", "Sign out", `aria-label="Main"`)
	// Logged out, unknown pages go to the login page like real ones.
	hs.anon().get("/admin/no-such-page").wantLogin(t)
}

func TestPreviewEmail(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	form := url.Values{"subject": {"Hello {{first_name | \"friend\"}}"}, "format": {"markdown"},
		"body": {"Hi **there** <script>alert(1)</script>"}}
	r := c.htmx(http.MethodPost, "/admin/preview/email", form).want(t, http.StatusOK).
		contains(t, `<iframe class="preview-frame"`, `sandbox=""`, "Hello friend", "srcdoc=\"")
	// The rendered HTML is escaped into the attribute, never live markup.
	if strings.Contains(r.body, "<strong>there</strong>") || !strings.Contains(r.body, "&lt;strong&gt;there&lt;/strong&gt;") {
		t.Fatalf("preview HTML not escaped into srcdoc:\n%s", r.body)
	}
	if strings.Contains(r.body, "<script>") {
		t.Fatal("a script tag reached the page")
	}
	// A validation error is shown in the panel.
	c.htmx(http.MethodPost, "/admin/preview/email", url.Values{"subject": {"Hi {{contact.name}}"}, "body": {"x"}}).
		want(t, http.StatusOK).contains(t, `class="preview-error"`, "unknown merge field")
	// Without the CSRF token: refused.
	c.csrf = ""
	c.htmx(http.MethodPost, "/admin/preview/email", form).want(t, http.StatusForbidden)
}

func TestFlashCookieIsSigned(t *testing.T) {
	hs := newHarness(t)
	c := hs.anon()
	// A made-up flash cookie is ignored.
	c.cookies["mailroom_flash"] = &http.Cookie{Name: "mailroom_flash", Value: "W3siayI6ImVycm9yIiwidCI6ImhhY2tlZCJ9XQ.bogus"}
	c.get("/admin/login").want(t, http.StatusOK).lacks(t, "hacked")
}
