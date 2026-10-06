package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/core"
)

const envToken = "mr_" + "abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"

func TestBootstrapFromEnvCreatesOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := auth.EnvBootstrap{AdminEmail: "owner@example.com", AdminPassword: "a-long-password-1", Token: envToken}

	res, err := f.svc.BootstrapFromEnv(ctx, in)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if !res.AdminCreated || !res.TokenCreated {
		t.Fatalf("first bootstrap result = %+v, want both created", res)
	}

	p, err := f.svc.AuthenticateToken(ctx, envToken)
	if err != nil {
		t.Fatalf("authenticate bootstrap token: %v", err)
	}
	if p.Scope != api.ScopeAdmin {
		t.Fatalf("token scope = %q, want admin", p.Scope)
	}

	// A restart with the same variables changes nothing.
	res, err = f.svc.BootstrapFromEnv(ctx, in)
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if res.AdminCreated || res.TokenCreated {
		t.Fatalf("second bootstrap result = %+v, want nothing created", res)
	}
	users, err := f.svc.ListUsers(ctx, 0, 10)
	if err != nil || len(users) != 1 {
		t.Fatalf("users = %v, %v; want exactly one", users, err)
	}
}

func TestBootstrapFromEnvSkipsAdminWhenUsersExist(t *testing.T) {
	f := newFixture(t)
	f.user(t, "existing@example.com", "a-long-password-1")
	res, err := f.svc.BootstrapFromEnv(context.Background(), auth.EnvBootstrap{AdminEmail: "other@example.com", AdminPassword: "a-long-password-2"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.AdminCreated {
		t.Fatal("admin created although a user already exists")
	}
}

func TestBootstrapFromEnvValidation(t *testing.T) {
	cases := []struct {
		name string
		in   auth.EnvBootstrap
		want string
	}{
		{"email without password", auth.EnvBootstrap{AdminEmail: "owner@example.com"}, "MAILROOM_ADMIN_PASSWORD"},
		{"short password", auth.EnvBootstrap{AdminEmail: "owner@example.com", AdminPassword: "short"}, "MAILROOM_ADMIN_EMAIL"},
		{"malformed token", auth.EnvBootstrap{Token: "mr_TOO-short"}, "MAILROOM_BOOTSTRAP_TOKEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.BootstrapFromEnv(context.Background(), tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %s", err, tc.want)
			}
			if !errors.Is(err, core.ErrValidation) {
				t.Fatalf("err = %v, want a validation error", err)
			}
		})
	}
}
