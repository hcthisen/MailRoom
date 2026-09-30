// Package testutil provides test helpers shared by every package: a temp
// SQLite database with migrations applied, a settable clock, a fake email
// provider and row fixtures. It is imported only from _test.go files.
package testutil

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/tokens"
)

// Epoch is the default start time of NewClock and the timestamp used by
// fixtures: Monday 2026-01-05 09:00 UTC.
var Epoch = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

// Secret is a fixed HMAC secret for tests.
var Secret = []byte("mailroom-test-secret-0123456789abcdef")

// NewDB returns a fresh database file in t.TempDir() with all migrations
// applied. It is closed when the test ends.
func NewDB(t testing.TB) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), db.DefaultFileName))
	if err != nil {
		t.Fatalf("testutil.NewDB: open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("testutil.NewDB: migrate: %v", err)
	}
	return d
}

// NewSigner returns a token signer using Secret and clock.
func NewSigner(t testing.TB, clock core.Clock) *tokens.Signer {
	t.Helper()
	s, err := tokens.NewSigner(Secret, clock)
	if err != nil {
		t.Fatalf("testutil.NewSigner: %v", err)
	}
	return s
}
