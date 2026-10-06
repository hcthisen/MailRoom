package auth_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

// fastArgon keeps tests quick; production uses DefaultArgon2Params.
var fastArgon = auth.Argon2Params{Time: 1, Memory: 64, Threads: 1, SaltLen: 16, KeyLen: 32}

type fixture struct {
	d     *db.DB
	clock *testutil.Clock
	svc   *auth.Service
	ctx   context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{d: testutil.NewDB(t), clock: testutil.NewClock(time.Time{})}
	f.svc = auth.New(auth.Deps{DB: f.d, Clock: f.clock, Argon2: fastArgon, Issuer: "Mailroom example.com"})
	f.ctx = core.WithActor(context.Background(), core.Actor{Type: core.ActorCLI})
	return f
}

func (f *fixture) user(t *testing.T, email, password string) auth.User {
	t.Helper()
	u, _, err := f.svc.CreateUser(f.ctx, auth.CreateUserInput{Email: email, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPasswordHashing(t *testing.T) {
	h, err := auth.HashPassword(context.Background(), "correct horse battery", auth.DefaultArgon2Params)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=2,p=2$") {
		t.Fatalf("PHC = %s", h)
	}
	for pw, want := range map[string]bool{"correct horse battery": true, "correct horse batterY": false, "": false} {
		if ok, err := auth.VerifyPassword(context.Background(), pw, h); err != nil || ok != want {
			t.Errorf("Verify(%q) = %v %v", pw, ok, err)
		}
	}
	h2, _ := auth.HashPassword(context.Background(), "correct horse battery", fastArgon)
	if h2 == h {
		t.Fatal("hashes are not salted")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=65536,t=2,p=2$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=99999999,t=2,p=2$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", "$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA"} {
		if _, err := auth.VerifyPassword(context.Background(), "x", bad); err == nil {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	tests := map[string]bool{
		"short":                    false,
		"exactly12chr":             true,
		"            ":             false,
		strings.Repeat("é", 12):    true,
		strings.Repeat("a", 256):   true,
		strings.Repeat("a", 257):   false,
		"correct horse battery ok": true,
	}
	for pw, ok := range tests {
		err := auth.CheckPasswordPolicy(pw)
		if (err == nil) != ok {
			t.Errorf("policy(%q) = %v", pw, err)
		}
		var ve *core.ValidationError
		if err != nil && (!errors.As(err, &ve) || ve.Field != "password") {
			t.Errorf("policy error type = %v", err)
		}
	}
	gen, err := auth.GeneratePassword()
	if err != nil || len(gen) != 24 || auth.CheckPasswordPolicy(gen) != nil {
		t.Fatalf("generated = %q %v", gen, err)
	}
}

func TestTOTPVerifyWithClock(t *testing.T) {
	secret, err := auth.NewTOTPSecret()
	if err != nil || len(secret) != 32 {
		t.Fatalf("secret = %q %v", secret, err)
	}
	now := testutil.Epoch
	code, _ := auth.TOTPCode(secret, now)
	tests := []struct {
		offset time.Duration
		ok     bool
	}{
		{0, true}, {29 * time.Second, true}, {30 * time.Second, true}, {-30 * time.Second, true},
		{61 * time.Second, false}, {-31 * time.Second, false}, {5 * time.Minute, false},
	}
	for _, tt := range tests {
		if got := auth.VerifyTOTP(secret, code, now.Add(tt.offset)); got != tt.ok {
			t.Errorf("offset %v: %v", tt.offset, got)
		}
	}
	if auth.VerifyTOTP(secret, "12345", now) || auth.VerifyTOTP("not base32!", code, now) {
		t.Fatal("bad input accepted")
	}
	if !auth.VerifyTOTP(strings.ToLower(secret), code[:3]+" "+code[3:], now) {
		t.Fatal("spaces/lowercase not tolerated")
	}
	uri := auth.TOTPURI("Mailroom", "owner@example.com", secret)
	if !strings.HasPrefix(uri, "otpauth://totp/Mailroom:owner@example.com?") || !strings.Contains(uri, "secret="+secret) ||
		!strings.Contains(uri, "digits=6") || !strings.Contains(uri, "period=30") {
		t.Fatalf("uri = %s", uri)
	}
}

func TestUsers(t *testing.T) {
	f := newFixture(t)
	u, gen, err := f.svc.CreateUser(f.ctx, auth.CreateUserInput{Email: " Owner@Example.com ", Name: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "owner@example.com" || u.Version != 1 || u.TOTPEnabled || len(gen) != 24 {
		t.Fatalf("user = %+v gen %q", u, gen)
	}
	if _, _, err := f.svc.CreateUser(f.ctx, auth.CreateUserInput{Email: "OWNER@example.com"}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	for _, in := range []auth.CreateUserInput{
		{Email: "nope"}, {Email: "a@example.com", Password: "short"}, {Email: "a@example.com", Name: "x\ny"},
	} {
		if _, _, err := f.svc.CreateUser(f.ctx, in); !errors.Is(err, core.ErrValidation) {
			t.Errorf("CreateUser(%+v) = %v", in, err)
		}
	}

	name := "Alex"
	u2, err := f.svc.UpdateUser(f.ctx, u.ID, auth.UpdateUserInput{Name: &name}, 1)
	if err != nil || u2.Name != "Alex" || u2.Version != 2 {
		t.Fatalf("update = %+v %v", u2, err)
	}
	if _, err := f.svc.UpdateUser(f.ctx, u.ID, auth.UpdateUserInput{Name: &name}, 1); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale update = %v", err)
	}
	if same, _ := f.svc.UpdateUser(f.ctx, u.ID, auth.UpdateUserInput{Name: &name}, 0); same.Version != 2 {
		t.Fatalf("no-op bumped the version: %+v", same)
	}
	if _, err := f.svc.UpdateUser(f.ctx, 999, auth.UpdateUserInput{Name: &name}, 0); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}

	// A password change ends the user's sessions.
	sess, err := f.svc.Login(f.ctx, auth.LoginInput{Email: "owner@example.com", Password: gen, IP: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	pw := "a brand new password"
	if _, err := f.svc.UpdateUser(f.ctx, u.ID, auth.UpdateUserInput{Password: &pw}, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.AuthenticateSession(f.ctx, sess.Token); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("session after password change = %v", err)
	}
	if _, err := f.svc.Login(f.ctx, auth.LoginInput{Email: "owner@example.com", Password: pw, IP: "1.2.3.4"}); err != nil {
		t.Fatalf("login with new password = %v", err)
	}

	// The last user can't be deleted.
	if err := f.svc.DeleteUser(f.ctx, u.ID, 0); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("delete last = %v", err)
	}
	other := f.user(t, "editor@example.com", "editor password 1")
	if err := f.svc.DeleteUser(f.ctx, other.ID, 7); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale delete = %v", err)
	}
	if err := f.svc.DeleteUser(f.ctx, other.ID, other.Version); err != nil {
		t.Fatal(err)
	}
	list, err := f.svc.ListUsers(f.ctx, 0, 10)
	if err != nil || len(list) != 1 || list[0].ID != u.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE actor_type = 'cli' AND operation LIKE 'user.%'`); n != 5 {
		t.Fatalf("user audit rows = %d", n)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE changes LIKE '%password%' AND changes NOT LIKE '%"password":"changed"%'`); n != 0 {
		t.Fatal("a password reached the audit log")
	}
}

func TestLoginAndLockout(t *testing.T) {
	f := newFixture(t)
	f.user(t, "owner@example.com", "the right password")
	login := func(email, pw, ip string) error {
		_, err := f.svc.Login(f.ctx, auth.LoginInput{Email: email, Password: pw, IP: ip})
		return err
	}
	if err := login("owner@example.com", "the right password", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if err := login("nobody@example.com", "whatever pass", "1.1.1.1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown email = %v", err)
	}
	for i := 0; i < auth.MaxLoginFailures; i++ {
		if err := login("owner@example.com", "wrong", "2.2.2.2"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("failure %d = %v", i, err)
		}
	}
	if err := login("owner@example.com", "the right password", "2.2.2.2"); !errors.Is(err, auth.ErrLoginLocked) {
		t.Fatalf("locked = %v", err)
	}
	// Other IPs are not locked out of this account.
	if err := login("Owner@Example.com", "the right password", "3.3.3.3"); err != nil {
		t.Fatalf("other IP = %v", err)
	}
	f.clock.Advance(auth.LoginWindow)
	if err := login("owner@example.com", "the right password", "2.2.2.2"); err != nil {
		t.Fatalf("after window = %v", err)
	}

	// Per-IP limit across emails.
	for i := 0; i < auth.MaxLoginFailuresPerIP; i++ {
		_ = login("user"+string(rune('a'+i))+"@example.com", "x", "4.4.4.4")
	}
	if err := login("owner@example.com", "the right password", "4.4.4.4"); !errors.Is(err, auth.ErrLoginLocked) {
		t.Fatalf("per-IP lock = %v", err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'auth.login' AND actor_type = 'user'`); n != 3 {
		t.Fatalf("login audit rows = %d", n)
	}
}

func TestSessions(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "owner@example.com", "the right password")
	sess, err := f.svc.Login(f.ctx, auth.LoginInput{Email: "owner@example.com", Password: "the right password", IP: "1.1.1.1", UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Token) != 43 || len(sess.CSRFToken) != 43 || sess.Token == sess.CSRFToken || !sess.ExpiresAt.Equal(testutil.Epoch.Add(auth.SessionTTL)) {
		t.Fatalf("session = %+v", sess)
	}
	// Only the hash is stored.
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM sessions WHERE id = ?`, sess.Token); n != 0 {
		t.Fatal("cookie value stored in clear")
	}
	p, csrf, err := f.svc.AuthenticateSession(f.ctx, sess.Token)
	if err != nil || csrf != sess.CSRFToken || p.Scope != api.ScopeAdmin || p.Actor.String() != "user:"+itoa(u.ID) || p.Actor.Name != u.Email {
		t.Fatalf("authenticate = %+v %q %v", p, csrf, err)
	}
	// Sliding expiry: each use extends the session by SessionTTL.
	f.clock.Advance(13 * 24 * time.Hour)
	if _, _, err := f.svc.AuthenticateSession(f.ctx, sess.Token); err != nil {
		t.Fatalf("day 13 = %v", err)
	}
	f.clock.Advance(13 * 24 * time.Hour)
	if _, _, err := f.svc.AuthenticateSession(f.ctx, sess.Token); err != nil {
		t.Fatalf("day 26 = %v", err)
	}
	f.clock.Advance(auth.SessionTTL)
	if _, _, err := f.svc.AuthenticateSession(f.ctx, sess.Token); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("expired = %v", err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatal("expired session not deleted")
	}

	sess2, _ := f.svc.Login(f.ctx, auth.LoginInput{Email: "owner@example.com", Password: "the right password"})
	if err := f.svc.Logout(f.ctx, sess2.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.AuthenticateSession(f.ctx, sess2.Token); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("after logout = %v", err)
	}
	for _, bad := range []string{"", "nope", strings.Repeat("x", 500)} {
		if _, _, err := f.svc.AuthenticateSession(f.ctx, bad); !errors.Is(err, api.ErrUnauthorized) {
			t.Errorf("cookie %q = %v", bad, err)
		}
	}
}

func TestTOTPLogin(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "owner@example.com", "the right password")
	enr, err := f.svc.BeginTOTP(f.ctx, u.ID)
	if err != nil || !strings.Contains(enr.OtpauthURI, "Mailroom%20example.com:owner@example.com") {
		t.Fatalf("begin = %+v %v", enr, err)
	}
	if _, err := f.svc.ConfirmTOTP(f.ctx, u.ID, enr.Secret, "000000"); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("wrong confirm code = %v", err)
	}
	confirmCode, _ := auth.TOTPCode(enr.Secret, f.clock.Now())
	u2, err := f.svc.ConfirmTOTP(f.ctx, u.ID, enr.Secret, confirmCode)
	if err != nil || !u2.TOTPEnabled {
		t.Fatalf("confirm = %+v %v", u2, err)
	}
	login := func(code, ip string) error {
		_, err := f.svc.Login(f.ctx, auth.LoginInput{Email: "owner@example.com", Password: "the right password", TOTPCode: code, IP: ip})
		return err
	}
	// A missing code looks like any other failure: no password oracle.
	if err := login("", "9.9.9.1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("no code = %v", err)
	}
	if err := login("123456", "9.9.9.2"); !errors.Is(err, auth.ErrInvalidCredentials) && confirmCode != "123456" {
		t.Fatalf("wrong code = %v", err)
	}
	// The code used to confirm can't log in.
	if err := login(confirmCode, "9.9.9.3"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("confirmation code reused = %v", err)
	}
	f.clock.Advance(30 * time.Second)
	code, _ := auth.TOTPCode(enr.Secret, f.clock.Now())
	if err := login(code, "9.9.9.4"); err != nil {
		t.Fatalf("fresh code = %v", err)
	}
	// Replay of the same code (RFC 6238 §5.2), even within its window.
	if err := login(code, "9.9.9.5"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("replayed code = %v", err)
	}
	f.clock.Advance(30 * time.Second)
	next, _ := auth.TOTPCode(enr.Secret, f.clock.Now())
	if err := login(next, "9.9.9.6"); err != nil {
		t.Fatalf("next step = %v", err)
	}
	f.clock.Advance(2 * time.Minute)
	if err := login(next, "9.9.9.7"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old code = %v", err)
	}
	if _, err := f.svc.ResetTOTP(f.ctx, u.ID, 1); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale reset = %v", err)
	}
	u3, err := f.svc.ResetTOTP(f.ctx, u.ID, 0)
	if err != nil || u3.TOTPEnabled {
		t.Fatalf("reset = %+v %v", u3, err)
	}
	if err := login("", "9.9.9.8"); err != nil {
		t.Fatalf("after reset = %v", err)
	}
}

func TestTokens(t *testing.T) {
	f := newFixture(t)
	tok, secret, err := f.svc.CreateCLIToken(context.Background(), auth.TokenInput{Name: "setup-agent", Scope: api.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "mr_") || len(secret) != auth.TokenLength || tok.Prefix != secret[:12] || tok.CreatedBy != "cli" {
		t.Fatalf("token = %+v %q", tok, secret)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM api_tokens WHERE token_hash = ? OR prefix = ?`, secret, secret); n != 0 {
		t.Fatal("token stored in clear")
	}
	p, err := f.svc.AuthenticateToken(f.ctx, secret)
	if err != nil || p.Scope != api.ScopeAdmin || p.Actor != (core.Actor{Type: core.ActorToken, ID: itoa(tok.ID), Name: "setup-agent"}) {
		t.Fatalf("authenticate = %+v %v", p, err)
	}
	lastUsed := func() string {
		var s string
		_ = f.d.Read.QueryRow(`SELECT coalesce(last_used_at, '') FROM api_tokens WHERE id = ?`, tok.ID).Scan(&s)
		return s
	}
	first := lastUsed()
	if first != db.FormatTime(testutil.Epoch) {
		t.Fatalf("last_used_at = %q", first)
	}
	f.clock.Advance(30 * time.Second)
	_, _ = f.svc.AuthenticateToken(f.ctx, secret)
	if lastUsed() != first {
		t.Fatal("last_used_at updated within a minute")
	}
	f.clock.Advance(31 * time.Second)
	_, _ = f.svc.AuthenticateToken(f.ctx, secret)
	if lastUsed() == first {
		t.Fatal("last_used_at not updated after a minute")
	}

	exp := f.clock.Now().Add(time.Hour)
	readTok, readSecret, err := f.svc.CreateToken(f.ctx, auth.TokenInput{Name: "reporting", Scope: api.ScopeRead, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := f.svc.AuthenticateToken(f.ctx, readSecret); err != nil || p.Scope != api.ScopeRead {
		t.Fatalf("read token = %+v %v", p, err)
	}
	f.clock.Advance(time.Hour)
	if _, err := f.svc.AuthenticateToken(f.ctx, readSecret); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("expired = %v", err)
	}

	if err := f.svc.RevokeToken(f.ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RevokeToken(f.ctx, tok.ID); err != nil {
		t.Fatalf("second revoke = %v", err)
	}
	if _, err := f.svc.AuthenticateToken(f.ctx, secret); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("revoked = %v", err)
	}
	for _, bad := range []string{"", "mr_", "mr_" + strings.Repeat("A", 52), secret[:len(secret)-1] + "1", "xx" + secret[2:]} {
		if _, err := f.svc.AuthenticateToken(f.ctx, bad); !errors.Is(err, api.ErrUnauthorized) {
			t.Errorf("token %q = %v", bad, err)
		}
	}

	if _, _, err := f.svc.CreateToken(f.ctx, auth.TokenInput{Name: "setup-agent", Scope: api.ScopeRead}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("reused name = %v", err)
	}
	past := f.clock.Now().Add(-time.Second)
	for _, in := range []auth.TokenInput{{Name: "", Scope: api.ScopeRead}, {Name: "x", Scope: "root"}, {Name: "x", Scope: api.ScopeRead, ExpiresAt: &past}} {
		if _, _, err := f.svc.CreateToken(f.ctx, in); !errors.Is(err, core.ErrValidation) {
			t.Errorf("CreateToken(%+v) = %v", in, err)
		}
	}
	active, _ := f.svc.ListTokens(f.ctx, 0, 10, false)
	all, _ := f.svc.ListTokens(f.ctx, 0, 10, true)
	if len(active) != 1 || active[0].ID != readTok.ID || len(all) != 2 || all[0].RevokedAt == nil {
		t.Fatalf("lists = %+v / %+v", active, all)
	}
}

// TestDeleteUserRevokesTheirTokens: the API tokens a user created stop
// working when the user is deleted; other tokens keep working.
func TestDeleteUserRevokesTheirTokens(t *testing.T) {
	f := newFixture(t)
	f.user(t, "owner@example.com", "owner password 1")
	leaver := f.user(t, "leaver@example.com", "leaver password 1")
	asLeaver := core.WithActor(f.ctx, core.Actor{Type: core.ActorUser, ID: strconv.FormatInt(leaver.ID, 10), Name: leaver.Email})
	_, leaverSecret, err := f.svc.CreateToken(asLeaver, auth.TokenInput{Name: "leaver-admin", Scope: api.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	_, cliSecret, err := f.svc.CreateToken(f.ctx, auth.TokenInput{Name: "setup", Scope: api.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if toks, err := f.svc.UserTokens(f.ctx, leaver.ID); err != nil || len(toks) != 1 || toks[0].Name != "leaver-admin" {
		t.Fatalf("user tokens = %+v %v", toks, err)
	}
	if err := f.svc.DeleteUser(f.ctx, leaver.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AuthenticateToken(f.ctx, leaverSecret); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("the deleted user's token = %v, want revoked", err)
	}
	if _, err := f.svc.AuthenticateToken(f.ctx, cliSecret); err != nil {
		t.Fatalf("another token = %v", err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'user.delete' AND changes LIKE '%"tokens_revoked":1%'`); n != 1 {
		t.Fatalf("audit of the delete = %d", n)
	}
}
