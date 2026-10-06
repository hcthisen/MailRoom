package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// API token format: "mr_" + 52 lowercase base32 characters (32 random
// bytes). Only the SHA-256 is stored, plus the first TokenPrefixLength
// characters for display.
const (
	tokenRandomBytes  = 32
	TokenLength       = len(api.TokenPrefix) + 52
	TokenPrefixLength = 12
	MaxTokenNameLen   = 100
)

var tokenEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Token is an API token as listed; the secret itself is shown only once,
// when it is created.
type Token struct {
	ID         int64      `json:"id" example:"3"`
	Name       string     `json:"name" example:"setup-agent"`
	Prefix     string     `json:"prefix" doc:"The first characters of the token, to recognise it" example:"mr_ab12cd34e"`
	Scope      api.Scope  `json:"scope" enum:"admin,read" example:"admin"`
	CreatedBy  string     `json:"created_by" doc:"Actor that created the token" example:"cli"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty" doc:"After this time the token no longer works; absent = never"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty" doc:"Last use, updated at most once a minute"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty" doc:"When the token was revoked; revoked tokens never work again"`
}

// TokenInput creates a token.
type TokenInput struct {
	Name      string
	Scope     api.Scope
	ExpiresAt *time.Time // nil = never
}

const tokenColumns = `id, name, prefix, scope, created_by, created_at, expires_at, last_used_at, revoked_at`

func scanToken(s interface{ Scan(...any) error }) (Token, error) {
	var t Token
	var scope, created string
	var exp, used, revoked sql.NullString
	if err := s.Scan(&t.ID, &t.Name, &t.Prefix, &scope, &t.CreatedBy, &created, &exp, &used, &revoked); err != nil {
		return Token{}, err
	}
	t.Scope = api.Scope(scope)
	var err error
	if t.CreatedAt, err = db.ParseTime(created); err != nil {
		return Token{}, err
	}
	if t.ExpiresAt, err = db.ParseNullTime(exp); err != nil {
		return Token{}, err
	}
	if t.LastUsedAt, err = db.ParseNullTime(used); err != nil {
		return Token{}, err
	}
	if t.RevokedAt, err = db.ParseNullTime(revoked); err != nil {
		return Token{}, err
	}
	return t, nil
}

func validTokenName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return name, core.Invalid("name", "is required")
	case utf8.RuneCountInString(name) > MaxTokenNameLen:
		return name, core.Invalid("name", "must be at most %d characters", MaxTokenNameLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return name, core.Invalid("name", "must not contain control characters")
		}
	}
	return name, nil
}

// newTokenSecret returns a fresh "mr_..." token.
func newTokenSecret() (string, error) {
	b := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: random: %w", err)
	}
	return api.TokenPrefix + tokenEncoding.EncodeToString(b), nil
}

// wellFormedToken checks the token shape before any database lookup.
func wellFormedToken(t string) bool {
	if len(t) != TokenLength || !strings.HasPrefix(t, api.TokenPrefix) {
		return false
	}
	for _, c := range t[len(api.TokenPrefix):] {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// CreateToken creates an API token for the actor in ctx and returns it
// with its secret, which is shown only this once. It is used by POST
// /tokens and by "mailroom token create". Names are unique, also among
// revoked tokens (ErrConflict).
func (s *Service) CreateToken(ctx context.Context, in TokenInput) (Token, string, error) {
	name, err := validTokenName(in.Name)
	if err != nil {
		return Token{}, "", err
	}
	if !in.Scope.Valid() {
		return Token{}, "", core.Invalid("scope", "must be admin or read")
	}
	now := s.clock.Now()
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		return Token{}, "", core.Invalid("expires_at", "must be in the future")
	}
	secret, err := newTokenSecret()
	if err != nil {
		return Token{}, "", err
	}
	var t Token
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO api_tokens (name, token_hash, prefix, scope, created_by, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			name, sha256Hex(secret), secret[:TokenPrefixLength], string(in.Scope),
			core.ActorFromContext(ctx).String(), db.FormatTime(now), db.NullTime(in.ExpiresAt)).Scan(&id)
		if db.IsUniqueViolation(err) {
			return fmt.Errorf("a token named %q already exists (revoked tokens keep their name): %w", name, core.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("create token: %w", err)
		}
		if t, err = s.getToken(ctx, tx, id); err != nil {
			return err
		}
		changes := map[string]any{"name": name, "scope": in.Scope}
		if in.ExpiresAt != nil {
			changes["expires_at"] = in.ExpiresAt.UTC()
		}
		return s.audit(ctx, tx, "token.create", "token", id, changes)
	})
	if err != nil {
		return Token{}, "", err
	}
	return t, secret, nil
}

func (s *Service) getToken(ctx context.Context, q db.Querier, id int64) (Token, error) {
	t, err := scanToken(q.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM api_tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, fmt.Errorf("token %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return Token{}, fmt.Errorf("get token %d: %w", id, err)
	}
	return t, nil
}

// GetToken returns token id (without its secret).
func (s *Service) GetToken(ctx context.Context, id int64) (Token, error) {
	return s.getToken(ctx, s.db.Read, id)
}

// ListTokens returns tokens with id > afterID in id order, at most limit.
// Revoked tokens are included only with includeRevoked.
func (s *Service) ListTokens(ctx context.Context, afterID int64, limit int, includeRevoked bool) ([]Token, error) {
	q := `SELECT ` + tokenColumns + ` FROM api_tokens WHERE id > ?`
	if !includeRevoked {
		q += ` AND revoked_at IS NULL`
	}
	rows, err := s.db.Read.QueryContext(ctx, q+` ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken revokes token id; it stops working immediately. Revoking a
// revoked token is a no-op.
func (s *Service) RevokeToken(ctx context.Context, id int64) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		t, err := s.getToken(ctx, tx, id)
		if err != nil {
			return err
		}
		if t.RevokedAt != nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE id = ?`,
			db.FormatTime(s.clock.Now()), id); err != nil {
			return fmt.Errorf("revoke token %d: %w", id, err)
		}
		return s.audit(ctx, tx, "token.revoke", "token", id, map[string]any{"name": t.Name})
	})
}

// AuthenticateToken implements api.Authenticator: the token must exist,
// not be revoked and not be expired. last_used_at is updated at most once
// a minute.
func (s *Service) AuthenticateToken(ctx context.Context, token string) (api.Principal, error) {
	if !wellFormedToken(token) {
		return api.Principal{}, fmt.Errorf("malformed token: %w", api.ErrUnauthorized)
	}
	t, err := scanToken(s.db.Read.QueryRowContext(ctx,
		`SELECT `+tokenColumns+` FROM api_tokens WHERE token_hash = ?`, sha256Hex(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Principal{}, fmt.Errorf("unknown token: %w", api.ErrUnauthorized)
	}
	if err != nil {
		return api.Principal{}, fmt.Errorf("authenticate token: %w", err)
	}
	now := s.clock.Now()
	if t.RevokedAt != nil {
		return api.Principal{}, fmt.Errorf("token revoked: %w", api.ErrUnauthorized)
	}
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return api.Principal{}, fmt.Errorf("token expired: %w", api.ErrUnauthorized)
	}
	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) >= touchInterval {
		if _, err := s.db.Write.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`,
			db.FormatTime(now), t.ID); err != nil {
			s.log.WarnContext(ctx, "auth: touch token", "token", t.ID, "err", err)
		}
	}
	return api.Principal{
		Actor: core.Actor{Type: core.ActorToken, ID: strconv.FormatInt(t.ID, 10), Name: t.Name},
		Scope: t.Scope,
	}, nil
}
