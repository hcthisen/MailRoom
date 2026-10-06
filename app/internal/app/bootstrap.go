package app

import (
	"context"

	"mailroom/internal/auth"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// CLIContext is ctx attributed to the command line in the audit log
// (actor type "cli"), for the bootstrap and offline commands.
func CLIContext(ctx context.Context) context.Context {
	return core.WithActor(ctx, core.Actor{Type: core.ActorCLI})
}

// CreateAdmin creates a UI user on d, as "mailroom admin create" does
// (L-5). An empty password generates one, which is returned once; the
// audit entry is attributed to the CLI.
func CreateAdmin(ctx context.Context, d *db.DB, email, password string) (auth.User, string, error) {
	return auth.New(auth.Deps{DB: d}).CreateAdmin(CLIContext(ctx), email, password)
}

// CreateToken creates an API token on d, as "mailroom token create" does
// (L-5), and returns it with its secret (shown only once). The token's
// created_by and the audit entry are attributed to the CLI.
func CreateToken(ctx context.Context, d *db.DB, in auth.TokenInput) (auth.Token, string, error) {
	return auth.New(auth.Deps{DB: d}).CreateCLIToken(CLIContext(ctx), in)
}
