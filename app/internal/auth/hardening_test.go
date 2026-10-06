package auth_test

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/testutil"
)

func (f *fixture) login(email, password, code, ip string) error {
	_, err := f.svc.Login(f.ctx, auth.LoginInput{Email: email, Password: password, TOTPCode: code, IP: ip})
	return err
}

// TestLoginConcurrentAttemptsAreCounted: attempts are counted before the
// slow password check, so a burst can't get more guesses than the limit.
func TestLoginConcurrentAttemptsAreCounted(t *testing.T) {
	f := newFixture(t)
	f.user(t, "owner@example.com", "the right password")
	const n = 40
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.login("owner@example.com", "guess "+strconv.Itoa(i), "", "6.6.6.6")
		}()
	}
	wg.Wait()
	checked, locked := 0, 0
	for _, err := range errs {
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			checked++
		case errors.Is(err, auth.ErrLoginLocked):
			locked++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if checked != auth.MaxLoginFailures || locked != n-auth.MaxLoginFailures {
		t.Fatalf("passwords checked = %d, locked = %d; want %d checked", checked, locked, auth.MaxLoginFailures)
	}
}

func TestLoginPerAccountLimit(t *testing.T) {
	f := newFixture(t)
	f.user(t, "owner@example.com", "the right password")
	if err := f.login("owner@example.com", "the right password", "", "8.8.8.8"); err != nil {
		t.Fatal(err) // 8.8.8.8 is now a known address of the account
	}
	// A distributed attack: one failure from each of many addresses.
	for i := range auth.MaxLoginFailuresPerAccount {
		if err := f.login("owner@example.com", "wrong", "", fmt.Sprintf("10.0.%d.1", i)); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("failure %d = %v", i, err)
		}
	}
	if err := f.login("owner@example.com", "the right password", "", "10.9.9.9"); !errors.Is(err, auth.ErrLoginLocked) {
		t.Fatalf("new address after the account budget = %v", err)
	}
	if err := f.login("owner@example.com", "the right password", "", "8.8.8.8"); err != nil {
		t.Fatalf("known address locked out = %v", err)
	}
	// Other accounts are not affected.
	f.user(t, "editor@example.com", "editor password 1")
	if err := f.login("editor@example.com", "editor password 1", "", "10.9.9.9"); err != nil {
		t.Fatalf("other account = %v", err)
	}
	f.clock.Advance(auth.LoginWindow)
	if err := f.login("owner@example.com", "the right password", "", "10.9.9.9"); err != nil {
		t.Fatalf("after the window = %v", err)
	}
}

// TestLoginIPv6Networks: an IPv6 client is limited per /64, so rotating
// addresses inside its network doesn't reset the budget.
func TestLoginIPv6Networks(t *testing.T) {
	f := newFixture(t)
	f.user(t, "owner@example.com", "the right password")
	for i := range auth.MaxLoginFailuresPerIP {
		_ = f.login(fmt.Sprintf("user%d@example.com", i), "x", "", fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	if err := f.login("owner@example.com", "the right password", "", "2001:db8:1:2:ffff::1"); !errors.Is(err, auth.ErrLoginLocked) {
		t.Fatalf("same /64 = %v", err)
	}
	if err := f.login("owner@example.com", "the right password", "", "2001:db8:1:3::1"); err != nil {
		t.Fatalf("other /64 = %v", err)
	}
}

func TestTOTPFailureBudget(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "owner@example.com", "the right password")
	if err := f.login("owner@example.com", "the right password", "", "8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	enr, _ := f.svc.BeginTOTP(f.ctx, u.ID)
	code, _ := auth.TOTPCode(enr.Secret, f.clock.Now())
	if _, err := f.svc.ConfirmTOTP(f.ctx, u.ID, enr.Secret, code); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(30 * time.Second)
	for i := range auth.MaxTOTPFailuresPerAccount {
		if err := f.login("owner@example.com", "the right password", "000000", fmt.Sprintf("10.1.%d.1", i)); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("code failure %d = %v", i, err)
		}
	}
	good, _ := auth.TOTPCode(enr.Secret, f.clock.Now())
	if good == "000000" {
		t.Skip("unlucky code")
	}
	// Once exhausted, unknown addresses are refused before the password is
	// checked, whatever the password: the lock reveals nothing.
	for _, pw := range []string{"the right password", "wrong"} {
		if err := f.login("owner@example.com", pw, good, "10.2.0.1"); !errors.Is(err, auth.ErrLoginLocked) {
			t.Fatalf("password %q after the TOTP budget = %v", pw, err)
		}
	}
	if err := f.login("owner@example.com", "the right password", good, "8.8.8.8"); err != nil {
		t.Fatalf("known address = %v", err)
	}
	f.clock.Advance(auth.TOTPWindow)
	next, _ := auth.TOTPCode(enr.Secret, f.clock.Now())
	if err := f.login("owner@example.com", "the right password", next, "10.2.0.1"); err != nil {
		t.Fatalf("after the TOTP window = %v", err)
	}
}

func TestSessionsEndOnSecurityChanges(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "owner@example.com", "the right password")
	var ip int
	newSession := func() string {
		t.Helper()
		ip++
		code := ""
		if cur, _ := f.svc.GetUser(f.ctx, u.ID); cur.TOTPEnabled {
			t.Fatal("helper needs TOTP off")
		}
		s, err := f.svc.Login(f.ctx, auth.LoginInput{Email: "owner@example.com", Password: "the right password", TOTPCode: code, IP: "7.7.7." + strconv.Itoa(ip)})
		if err != nil {
			t.Fatal(err)
		}
		return s.Token
	}
	alive := func(tok string) bool {
		_, _, err := f.svc.AuthenticateSession(f.ctx, tok)
		if err != nil && !errors.Is(err, api.ErrUnauthorized) {
			t.Fatal(err)
		}
		return err == nil
	}

	tok := newSession()
	name := "Owner"
	if _, err := f.svc.UpdateUser(f.ctx, u.ID, auth.UpdateUserInput{Name: &name}, 0); err != nil || !alive(tok) {
		t.Fatalf("a name change must keep sessions (err %v)", err)
	}
	email := "Owner@Example.com" // same address after normalisation
	if _, err := f.svc.UpdateUser(f.ctx, u.ID, auth.UpdateUserInput{Email: &email}, 0); err != nil || !alive(tok) {
		t.Fatalf("an unchanged email must keep sessions (err %v)", err)
	}
	email = "boss@example.com"
	if _, err := f.svc.UpdateUser(f.ctx, u.ID, auth.UpdateUserInput{Email: &email}, 0); err != nil || alive(tok) {
		t.Fatalf("an email change must end sessions (err %v)", err)
	}
	email = "owner@example.com"
	if _, err := f.svc.UpdateUser(f.ctx, u.ID, auth.UpdateUserInput{Email: &email}, 0); err != nil {
		t.Fatal(err)
	}

	tok = newSession()
	enr, _ := f.svc.BeginTOTP(f.ctx, u.ID)
	code, _ := auth.TOTPCode(enr.Secret, f.clock.Now())
	if _, err := f.svc.ConfirmTOTP(f.ctx, u.ID, enr.Secret, code); err != nil || alive(tok) {
		t.Fatalf("enabling TOTP must end sessions (err %v)", err)
	}
	f.clock.Advance(30 * time.Second)
	code, _ = auth.TOTPCode(enr.Secret, f.clock.Now())
	s, err := f.svc.Login(f.ctx, auth.LoginInput{Email: "owner@example.com", Password: "the right password", TOTPCode: code, IP: "7.7.8.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ResetTOTP(f.ctx, u.ID, 0); err != nil || alive(s.Token) {
		t.Fatalf("resetting TOTP must end sessions (err %v)", err)
	}
}

func TestSessionAbsoluteLifetime(t *testing.T) {
	f := newFixture(t)
	f.user(t, "owner@example.com", "the right password")
	s, err := f.svc.Login(f.ctx, auth.LoginInput{Email: "owner@example.com", Password: "the right password"})
	if err != nil {
		t.Fatal(err)
	}
	for day := 10; day < 30; day += 10 {
		f.clock.Advance(10 * 24 * time.Hour)
		if _, _, err := f.svc.AuthenticateSession(f.ctx, s.Token); err != nil {
			t.Fatalf("day %d = %v", day, err)
		}
	}
	f.clock.Advance(10 * 24 * time.Hour) // day 30: used every 10 days, but too old
	if _, _, err := f.svc.AuthenticateSession(f.ctx, s.Token); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("day 30 = %v", err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatal("expired session kept")
	}
}
