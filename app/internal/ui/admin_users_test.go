package ui_test

import (
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"mailroom/internal/auth"
)

func TestAdminUsers(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	c.get("/admin/users").want(t, http.StatusOK).contains(t, hs.email, `<span class="tag">you</span>`, "Add a user")

	// Create with a password that is too short: the error is on the field.
	c.post("/admin/users", url.Values{"email": {"ed@example.org"}, "password": {"short"}}).
		want(t, http.StatusUnprocessableEntity).contains(t, `id="f-password-error"`, "must be at least").lacks(t, `value="short"`)
	c.post("/admin/users", url.Values{"email": {"not an email"}}).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-email-error"`)

	// Without a password one is generated and shown once.
	r := c.post("/admin/users", url.Values{"email": {"Ed@Example.org"}, "name": {"Ed"}}).want(t, http.StatusOK).
		contains(t, "Give ed@example.org their password", "This is the only time the password is shown", `data-copy="user-password"`)
	pw := admMatch(t, `id="user-password" class="mono" type="text" readonly value="([^"]+)"`, r.body)
	if _, err := hs.app.Auth.Login(hs.ctx, auth.LoginInput{Email: "ed@example.org", Password: pw, IP: "192.0.2.20"}); err != nil {
		t.Fatalf("the shown password doesn't work: %v", err)
	}
	c.post("/admin/users", url.Values{"email": {"ed@example.org"}, "password": {"long enough password"}}).
		want(t, http.StatusConflict).contains(t, "already exists")

	users, err := hs.app.Auth.ListUsers(hs.ctx, 0, 10)
	if err != nil || len(users) != 2 {
		t.Fatalf("users = %+v, %v", users, err)
	}
	ed := users[1]
	edURL := "/admin/users/" + admItoa(ed.ID)
	page := c.get(edURL).want(t, http.StatusOK).contains(t, "<h1>Ed</h1>", "Only ed@example.org can set it up")
	version := page.field(t, "version")

	// Update: new name and password; Ed is signed out everywhere.
	c.post(edURL, url.Values{"version": {version}, "email": {"ed@example.org"}, "name": {"Edward"}, "password": {"a brand new password"}}).
		wantRedirect(t, edURL)
	if u, _ := hs.app.Auth.GetUser(hs.ctx, ed.ID); u.Name != "Edward" {
		t.Fatalf("user = %+v", u)
	}
	c.get(edURL).contains(t, "They were signed out everywhere")
	// The old form's version is stale now: nothing is saved.
	c.post(edURL, url.Values{"version": {version}, "email": {"ed@example.org"}, "name": {"Overwrite"}}).
		want(t, http.StatusPreconditionFailed).contains(t, "Someone else changed this user")
	c.post(edURL, url.Values{"version": {"x"}, "email": {"ed@example.org"}}).want(t, http.StatusUnprocessableEntity)

	// TOTP enrolment is only for your own account.
	c.post(edURL+"/totp", nil).want(t, http.StatusForbidden).contains(t, "Only ed@example.org can set up two-factor sign-in")

	// Reset another user's TOTP (after a lost phone).
	e, err := hs.app.Auth.BeginTOTP(hs.ctx, ed.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := auth.TOTPCode(e.Secret, hs.clock.Now())
	if _, err := hs.app.Auth.ConfirmTOTP(hs.ctx, ed.ID, e.Secret, code); err != nil {
		t.Fatal(err)
	}
	conf := c.post(edURL+"/totp/reset", nil).want(t, http.StatusOK).contains(t, "Turn off two-factor sign-in?", "User: ed@example.org")
	c.post(edURL+"/totp/reset", url.Values{"confirm": {"yes"}, "version": {conf.field(t, "version")}}).wantRedirect(t, edURL)
	if u, _ := hs.app.Auth.GetUser(hs.ctx, ed.ID); u.TOTPEnabled {
		t.Fatal("TOTP still on")
	}
	c.post(edURL+"/totp/reset", nil).wantRedirect(t, edURL)

	// Delete asks first; the confirmation carries the version.
	conf = c.post(edURL+"/delete", nil).want(t, http.StatusOK).contains(t, "Delete this user?", "Email: ed@example.org")
	if hs.count(`SELECT count(*) FROM users`) != 2 {
		t.Fatal("the confirmation step deleted the user")
	}
	c.post(edURL+"/delete", url.Values{"confirm": {"yes"}, "version": {conf.field(t, "version")}}).wantRedirect(t, "/admin/users")
	if hs.count(`SELECT count(*) FROM users`) != 1 {
		t.Fatal("user not deleted")
	}
	c.get("/admin/users").contains(t, "The user ed@example.org was deleted.").lacks(t, "ed@example.org</a>")
	c.get(edURL).want(t, http.StatusNotFound)

	// The last user can't be deleted.
	me := "/admin/users/" + admItoa(users[0].ID)
	conf = c.post(me+"/delete", nil).want(t, http.StatusOK).contains(t, "This is your own account")
	c.post(me+"/delete", url.Values{"confirm": {"yes"}, "version": {conf.field(t, "version")}}).wantRedirect(t, me)
	c.get(me).want(t, http.StatusOK).contains(t, "The user was not deleted", "last user")
}

func TestAdminOwnTOTP(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	users, err := hs.app.Auth.ListUsers(hs.ctx, 0, 10)
	if err != nil || len(users) != 1 {
		t.Fatalf("users = %+v, %v", users, err)
	}
	me := "/admin/users/" + admItoa(users[0].ID)
	c.get(me).want(t, http.StatusOK).contains(t, "Set up two-factor sign-in")

	r := c.post(me+"/totp", nil).want(t, http.StatusOK).contains(t, "Add Mailroom to your authenticator app", "otpauth://totp/",
		`<svg class="qr"`, "scan this QR code", `data-copy="totp-secret"`)
	secret := r.field(t, "secret")
	uri := r.field(t, "otpauth_uri")

	// A wrong code: the error is on the field, the same secret stays.
	c.post(me+"/totp/confirm", url.Values{"secret": {secret}, "otpauth_uri": {uri}, "code": {"000000"}}).
		want(t, http.StatusUnprocessableEntity).contains(t, `id="f-code-error"`, "is not the current code", secret)
	if u, _ := hs.app.Auth.GetUser(hs.ctx, users[0].ID); u.TOTPEnabled {
		t.Fatal("TOTP on after a wrong code")
	}

	code, err := auth.TOTPCode(secret, hs.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	c.post(me+"/totp/confirm", url.Values{"secret": {secret}, "code": {code[:3] + " " + code[3:]}}).wantRedirect(t, "/admin/login")
	if u, _ := hs.app.Auth.GetUser(hs.ctx, users[0].ID); !u.TOTPEnabled {
		t.Fatal("TOTP not on")
	}
	// Confirming ended the session: sign in again with a code.
	c.get("/admin/").wantLogin(t)
	c.get("/admin/login").want(t, http.StatusOK).contains(t, "Two-factor sign-in is on.")
	if _, err := hs.app.Auth.Login(hs.ctx, auth.LoginInput{Email: hs.email, Password: hs.password, IP: "192.0.2.30"}); err == nil {
		t.Fatal("login without a code worked")
	}
}

// admMatch returns the first submatch of pattern in s.
func admMatch(t *testing.T, pattern, s string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match for %s in:\n%.3000s", pattern, s)
	}
	return htmlUnescape(m[1])
}
