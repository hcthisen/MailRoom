package auth

import (
	"context"

	"mailroom/internal/core"
)

// cliContext marks ctx as a CLI action unless it already carries an actor.
func cliContext(ctx context.Context) context.Context {
	if core.ActorFromContext(ctx).Type == core.ActorSystem {
		return core.WithActor(ctx, core.Actor{Type: core.ActorCLI})
	}
	return ctx
}

// CreateAdmin creates a UI user from the CLI ("mailroom admin create",
// L-5). An empty password generates one, which is returned so it can be
// shown once. The audit actor is "cli" unless ctx carries another.
func (s *Service) CreateAdmin(ctx context.Context, email, password string) (User, string, error) {
	return s.CreateUser(cliContext(ctx), CreateUserInput{Email: email, Password: password})
}

// CreateCLIToken creates an API token from the CLI ("mailroom token
// create", L-5) and returns it with its secret. The audit actor and
// created_by are "cli" unless ctx carries another actor.
func (s *Service) CreateCLIToken(ctx context.Context, in TokenInput) (Token, string, error) {
	return s.CreateToken(cliContext(ctx), in)
}
