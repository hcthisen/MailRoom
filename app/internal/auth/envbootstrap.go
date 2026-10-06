package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// BootstrapTokenName is the name of the API token created from
// MAILROOM_BOOTSTRAP_TOKEN.
const BootstrapTokenName = "bootstrap"

// EnvBootstrap holds the optional first-start values read from the
// environment, for platforms where nobody can run "mailroom admin create"
// or "mailroom token create" inside the container (e.g. Coolify).
type EnvBootstrap struct {
	AdminEmail    string // MAILROOM_ADMIN_EMAIL
	AdminPassword string // MAILROOM_ADMIN_PASSWORD (min. 12 characters)
	Token         string // MAILROOM_BOOTSTRAP_TOKEN ("mr_" + 52 chars a-z, 2-7)
}

// BootstrapResult says what BootstrapFromEnv created.
type BootstrapResult struct {
	AdminCreated bool
	TokenCreated bool
}

// BootstrapFromEnv creates the first admin user and/or an admin-scope API
// token named "bootstrap" from values the operator chose in advance. It is
// safe to call on every start: the admin is created only while the
// database has no users at all, and the token only while no token named
// "bootstrap" exists. The token secret itself is never stored, only its
// hash, like every other token. Operators should remove the variables once
// the setup is done and revoke the token when it is no longer needed.
func (s *Service) BootstrapFromEnv(ctx context.Context, in EnvBootstrap) (BootstrapResult, error) {
	ctx = core.WithActor(ctx, core.Actor{Type: core.ActorSystem, Name: "env-bootstrap"})
	var res BootstrapResult

	if in.AdminEmail != "" {
		if in.AdminPassword == "" {
			return res, fmt.Errorf("MAILROOM_ADMIN_EMAIL is set but MAILROOM_ADMIN_PASSWORD is empty: %w", core.ErrValidation)
		}
		var n int
		if err := s.db.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			return res, fmt.Errorf("count users: %w", err)
		}
		if n == 0 {
			if _, _, err := s.CreateUser(ctx, CreateUserInput{Email: in.AdminEmail, Password: in.AdminPassword}); err != nil {
				return res, fmt.Errorf("MAILROOM_ADMIN_EMAIL: %w", err)
			}
			res.AdminCreated = true
		}
	}

	if in.Token != "" {
		if !wellFormedToken(in.Token) {
			return res, fmt.Errorf("MAILROOM_BOOTSTRAP_TOKEN must be %q followed by 52 characters a-z and 2-7: %w", "mr_", core.ErrValidation)
		}
		created, err := s.createNamedToken(ctx, BootstrapTokenName, in.Token)
		if err != nil {
			return res, fmt.Errorf("MAILROOM_BOOTSTRAP_TOKEN: %w", err)
		}
		res.TokenCreated = created
	}
	return res, nil
}

// createNamedToken stores secret as an admin token named name unless a
// token with that name already exists (then it returns false).
func (s *Service) createNamedToken(ctx context.Context, name, secret string) (bool, error) {
	created := false
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM api_tokens WHERE name = ?`, name).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("look up token: %w", err)
		}
		err = tx.QueryRowContext(ctx,
			`INSERT INTO api_tokens (name, token_hash, prefix, scope, created_by, created_at)
			 VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
			name, sha256Hex(secret), secret[:TokenPrefixLength], string(api.ScopeAdmin),
			core.ActorFromContext(ctx).String(), db.FormatTime(s.clock.Now())).Scan(&id)
		if err != nil {
			return fmt.Errorf("create token: %w", err)
		}
		created = true
		return s.audit(ctx, tx, "token.create", "token", id, map[string]any{"name": name, "scope": "admin", "source": "env"})
	})
	return created, err
}
