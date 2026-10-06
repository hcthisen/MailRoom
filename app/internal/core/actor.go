package core

import "context"

// Actor types, matching audit_log.actor_type.
const (
	ActorUser   = "user"
	ActorToken  = "token"
	ActorSystem = "system"
	ActorPublic = "public"
	ActorCLI    = "cli"
)

// Actor identifies who performs an operation, for audit and attribution.
type Actor struct {
	Type string // one of ActorUser, ActorToken, ActorSystem, ActorPublic, ActorCLI
	ID   string // user or token ID; empty for system/public/cli
	Name string // display name: user email or token name
}

// String returns the compact actor form used in created_by/actor columns,
// e.g. "user:1", "token:7" or "system".
func (a Actor) String() string {
	if a.ID == "" {
		return a.Type
	}
	return a.Type + ":" + a.ID
}

type actorKey struct{}

// WithActor returns a copy of ctx carrying the actor.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFromContext returns the actor stored in ctx, or the system actor
// when none was set (background workers).
func ActorFromContext(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorKey{}).(Actor); ok {
		return a
	}
	return Actor{Type: ActorSystem}
}
