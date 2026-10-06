package db

import (
	"context"
	"database/sql"
)

// CommitHook observes the transactions that WithTx runs with a context
// carrying it. The API's Idempotency-Key middleware uses it to record a key
// in the same transaction as the request's side effects, so a crash between
// COMMIT and storing the response can't lead to a second execution.
type CommitHook struct {
	// Before runs inside the transaction right before COMMIT. An error rolls
	// the transaction back and is returned by WithTx.
	Before func(ctx context.Context, tx *sql.Tx) error
	// After runs after a successful COMMIT.
	After func()
}

type commitHookKey struct{}

// WithCommitHook returns a copy of ctx carrying h. Every WithTx called with
// the returned context (or one derived from it) runs h. A later
// WithCommitHook replaces an earlier one.
func WithCommitHook(ctx context.Context, h *CommitHook) context.Context {
	return context.WithValue(ctx, commitHookKey{}, h)
}

func commitHookFrom(ctx context.Context) *CommitHook {
	h, _ := ctx.Value(commitHookKey{}).(*CommitHook)
	return h
}
