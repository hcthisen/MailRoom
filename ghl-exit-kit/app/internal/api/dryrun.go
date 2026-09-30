package api

import (
	"context"
	"database/sql"
	"errors"

	"mailroom/internal/db"
)

// DryRunParam is embedded in Huma inputs of operations that support
// ?dry_run=true (G-6): imports, bulk operations, erase and broadcast send.
type DryRunParam struct {
	DryRun bool `query:"dry_run" doc:"Run the operation in a transaction that is rolled back, and return what would happen without changing anything."`
}

// errDryRun forces the rollback of a dry-run transaction.
var errDryRun = errors.New("api: dry run rollback")

// Tx runs fn in a write transaction. With dryRun the transaction is always
// rolled back, even when fn succeeds, so fn can do the real work and report
// its outcome without changing anything. fn's own error is returned
// unchanged either way.
func Tx(ctx context.Context, d *db.DB, dryRun bool, fn func(tx *sql.Tx) error) error {
	err := d.WithTx(ctx, func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		return nil
	}
	return err
}
