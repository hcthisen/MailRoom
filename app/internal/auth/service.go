// Package auth manages UI users (argon2id passwords, optional TOTP,
// sessions with CSRF tokens, login rate limiting) and API tokens (PRD A-4,
// G-4, L-5). It implements api.Authenticator for the API middleware,
// registers the users, tokens and /me operations, and provides the CLI
// bootstrap functions CreateAdmin and CreateToken.
package auth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Session and credential lifetimes.
const (
	// SessionTTL is the sliding session lifetime: every use extends it, up
	// to SessionMaxAge after the login.
	SessionTTL = 14 * 24 * time.Hour
	// SessionMaxAge is the absolute session lifetime: a session ends this
	// long after the login, however often it is used.
	SessionMaxAge = 30 * 24 * time.Hour
	// touchInterval throttles last_seen_at / last_used_at writes.
	touchInterval = time.Minute
)

// Login errors. They are safe to show to the user. A login form always
// has the (optional) authenticator code field: a missing code is reported
// as ErrInvalidCredentials like any other failure, so the response never
// reveals that the password was right.
var (
	ErrInvalidCredentials = errors.New("invalid email, password or code")
	ErrLoginLocked        = errors.New("too many failed logins; try again later")
)

// Deps are the Service's dependencies.
type Deps struct {
	DB     *db.DB
	Clock  core.Clock   // default core.SystemClock
	Logger *slog.Logger // default slog.Default()
	// Issuer labels TOTP entries in authenticator apps (default "Mailroom").
	Issuer string
	// Argon2 overrides the password hashing cost (zero = DefaultArgon2Params).
	Argon2 Argon2Params
}

// Service manages users, sessions and API tokens.
type Service struct {
	db     *db.DB
	clock  core.Clock
	log    *slog.Logger
	issuer string
	argon  Argon2Params
	logins *loginLimiter
	// dummy is verified for unknown emails, so a login takes as long
	// whether or not the account exists.
	dummy string
}

var _ api.Authenticator = (*Service)(nil)

// New returns an auth service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Issuer == "" {
		d.Issuer = "Mailroom"
	}
	if d.Argon2 == (Argon2Params{}) {
		d.Argon2 = DefaultArgon2Params
	}
	return &Service{
		db: d.DB, clock: d.Clock, log: d.Logger, issuer: d.Issuer, argon: d.Argon2,
		logins: newLoginLimiter(d.Clock),
		dummy:  dummyHash(d.Argon2),
	}
}

// hash hashes a password with the service's parameters.
func (s *Service) hash(ctx context.Context, pw string) (string, error) {
	return HashPassword(ctx, pw, s.argon)
}
