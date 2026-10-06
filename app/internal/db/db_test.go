package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d
}

func TestOpenDirectory(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(dir): %v", err)
	}
	defer d.Close()
	if want := filepath.Join(dir, DefaultFileName); d.Path != want {
		t.Fatalf("Path = %q, want %q", d.Path, want)
	}
}

func TestPragmas(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tests := []struct {
		pool   *sql.DB
		pragma string
		want   string
	}{
		{d.Write, "journal_mode", "wal"},
		{d.Write, "foreign_keys", "1"},
		{d.Write, "busy_timeout", "10000"},
		{d.Write, "synchronous", "1"}, // NORMAL
		{d.Read, "journal_mode", "wal"},
		{d.Read, "foreign_keys", "1"},
		{d.Read, "query_only", "1"},
	}
	for _, tt := range tests {
		var got string
		if err := tt.pool.QueryRowContext(ctx, "PRAGMA "+tt.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", tt.pragma, err)
		}
		if got != tt.want {
			t.Errorf("PRAGMA %s = %q, want %q", tt.pragma, got, tt.want)
		}
	}
	if _, err := d.Read.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('x', '1', 'now')`); err == nil {
		t.Error("read pool must reject writes")
	}
}

func TestMigrate(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	for _, table := range []string{"contacts", "tags", "messages", "suppressions", "schema_migrations", "enrollments"} {
		var n int
		if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d, err=%v)", table, n, err)
		}
	}
	v, err := d.SchemaVersion(ctx)
	if want := latestVersion(t); err != nil || v != want {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, want)
	}
	// Idempotent: a second run applies nothing.
	applied, err := d.Migrate(ctx)
	if err != nil || len(applied) != 0 {
		t.Fatalf("second Migrate = %v, %v; want nothing applied", applied, err)
	}
	pending, err := d.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("Pending = %v, %v", pending, err)
	}
	var name, appliedAt string
	if err := d.Read.QueryRowContext(ctx, `SELECT name, applied_at FROM schema_migrations WHERE version=1`).Scan(&name, &appliedAt); err != nil {
		t.Fatal(err)
	}
	if name != "0001_init.sql" {
		t.Errorf("name = %q", name)
	}
	if _, err := ParseTime(appliedAt); err != nil {
		t.Errorf("applied_at %q: %v", appliedAt, err)
	}
}

func TestMigrateFreshDBReportsApplied(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	pending, err := d.Pending(ctx)
	if err != nil || len(pending) == 0 {
		t.Fatalf("fresh DB Pending = %d, %v", len(pending), err)
	}
	applied, err := d.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != len(pending) || applied[0] != "0001_init.sql" {
		t.Fatalf("applied = %v", applied)
	}
}

func TestUnknownMigrationRejected(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.Write.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (9999, '9999_future.sql', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "newer binary") {
		t.Fatalf("want newer-binary error, got %v", err)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	d := openTestDB(t)
	_, err := d.Write.ExecContext(context.Background(),
		`INSERT INTO contact_tags (contact_id, tag_id, added_at) VALUES (999, 999, 'x')`)
	if !IsForeignKeyViolation(err) {
		t.Fatalf("want FK violation, got %v", err)
	}
	if !IsConstraintViolation(err) {
		t.Fatal("FK violation is a constraint violation")
	}
}

func insertSetting(ctx context.Context, q Querier, key string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, '1', ?)`, key, FormatTime(time.Unix(0, 0)))
	return err
}

func countSettings(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	if err := d.Read.QueryRowContext(context.Background(), `SELECT count(*) FROM settings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWithTx(t *testing.T) {
	ctx := context.Background()
	errBoom := errors.New("boom")
	tests := []struct {
		name      string
		fn        func(tx *sql.Tx) error
		wantErr   error
		wantPanic bool
		wantRows  int
	}{
		{name: "commit", fn: func(tx *sql.Tx) error { return insertSetting(ctx, tx, "a") }, wantRows: 1},
		{name: "rollback on error", fn: func(tx *sql.Tx) error {
			if err := insertSetting(ctx, tx, "a"); err != nil {
				return err
			}
			return errBoom
		}, wantErr: errBoom},
		{name: "rollback on panic", fn: func(tx *sql.Tx) error {
			if err := insertSetting(ctx, tx, "a"); err != nil {
				return err
			}
			panic("kaboom")
		}, wantPanic: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openTestDB(t)
			var err error
			func() {
				defer func() {
					if p := recover(); (p != nil) != tt.wantPanic {
						t.Fatalf("panic = %v, wantPanic %v", p, tt.wantPanic)
					}
				}()
				err = d.WithTx(ctx, tt.fn)
			}()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := countSettings(t, d); got != tt.wantRows {
				t.Fatalf("rows = %d, want %d", got, tt.wantRows)
			}
			// The single write connection must be usable afterwards.
			if err := d.WithTx(ctx, func(tx *sql.Tx) error { return insertSetting(ctx, tx, "after") }); err != nil {
				t.Fatalf("write pool unusable after %s: %v", tt.name, err)
			}
		})
	}
}

func TestConcurrentWrites(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				err := d.WithTx(ctx, func(tx *sql.Tx) error {
					// Read-modify-write inside the transaction: lost updates
					// or SQLITE_BUSY would show up here.
					var n int
					if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM settings`).Scan(&n); err != nil {
						return err
					}
					return insertSetting(ctx, tx, fmt.Sprintf("k-%d-%d-%d", w, i, n))
				})
				if err != nil {
					errs <- err
				}
				// Concurrent reads on the read pool while writing.
				var n int
				if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM settings`).Scan(&n); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}
	if got := countSettings(t, d); got != workers*perWorker {
		t.Fatalf("rows = %d, want %d", got, workers*perWorker)
	}
}

func TestTimeFormat(t *testing.T) {
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	times := []time.Time{
		base,
		base.Add(time.Microsecond),
		base.Add(999 * time.Millisecond),
		base.Add(time.Second),
		base.Add(-time.Hour).In(time.FixedZone("CEST", 2*3600)),
		time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2100, 12, 31, 23, 59, 59, 999999000, time.UTC),
	}
	var strs []string
	for _, tm := range times {
		s := FormatTime(tm)
		if len(s) != len(TimeFormat) {
			t.Fatalf("FormatTime(%v) = %q is not fixed width", tm, s)
		}
		got, err := ParseTime(s)
		if err != nil {
			t.Fatalf("ParseTime(%q): %v", s, err)
		}
		if !got.Equal(tm.Truncate(time.Microsecond)) || got.Location() != time.UTC {
			t.Fatalf("round trip %v -> %q -> %v", tm, s, got)
		}
		strs = append(strs, s)
	}
	// Lexicographic order must equal chronological order.
	sorted := append([]string(nil), strs...)
	sort.Strings(sorted)
	chrono := append([]time.Time(nil), times...)
	sort.Slice(chrono, func(i, j int) bool { return chrono[i].Before(chrono[j]) })
	for i := range sorted {
		if sorted[i] != FormatTime(chrono[i]) {
			t.Fatalf("order mismatch at %d: %s vs %s", i, sorted[i], FormatTime(chrono[i]))
		}
	}
	if FormatTime(base) != "2026-09-29T12:00:00.000000Z" {
		t.Fatalf("FormatTime = %q", FormatTime(base))
	}
	if _, err := ParseTime("yesterday"); err == nil {
		t.Fatal("ParseTime must reject garbage")
	}
	if got, err := ParseTime("2026-09-29T14:00:00+02:00"); err != nil || !got.Equal(base) || got.Location() != time.UTC {
		t.Fatalf("RFC 3339 fallback = %v, %v", got, err)
	}
}

func TestNullHelpers(t *testing.T) {
	if NullID(0) != nil || NullID(5) != int64(5) {
		t.Error("NullID")
	}
	if NullString("") != nil || NullString("x") != "x" {
		t.Error("NullString")
	}
	if NullTime(nil) != nil {
		t.Error("NullTime(nil)")
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if NullTime(&now) != "2026-01-02T03:04:05.000000Z" {
		t.Error("NullTime")
	}
	if p, err := ParseNullTime(sql.NullString{}); p != nil || err != nil {
		t.Error("ParseNullTime(NULL)")
	}
	if p, err := ParseNullTime(sql.NullString{Valid: true, String: FormatTime(now)}); err != nil || !p.Equal(now) {
		t.Error("ParseNullTime")
	}
}

func TestJSONHelpers(t *testing.T) {
	var nilMap map[string]any
	var nilSlice []int64
	tests := []struct {
		in   any
		want string
	}{
		{nil, "{}"},
		{nilMap, "{}"},
		{nilSlice, "[]"},
		{map[string]any{"a": 1}, `{"a":1}`},
		{[]string{"x"}, `["x"]`},
	}
	for _, tt := range tests {
		got, err := MarshalJSON(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("MarshalJSON(%#v) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	var m map[string]any
	if err := UnmarshalJSON(`{"a":"b"}`, &m); err != nil || m["a"] != "b" {
		t.Fatalf("UnmarshalJSON = %v, %v", m, err)
	}
	if err := UnmarshalJSON("", &m); err != nil {
		t.Fatal(err)
	}
	if err := UnmarshalJSON("{", &m); err == nil {
		t.Fatal("want error for invalid JSON")
	}
	if _, err := MarshalJSON(func() {}); err == nil {
		t.Fatal("want error for unmarshalable value")
	}
}

func TestUniqueViolation(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := insertSetting(ctx, d.Write, "dup"); err != nil {
		t.Fatal(err)
	}
	err := insertSetting(ctx, d.Write, "dup")
	if !IsUniqueViolation(err) {
		t.Fatalf("want unique violation, got %v", err)
	}
	if IsUniqueViolation(errors.New("other")) || IsUniqueViolation(nil) {
		t.Fatal("false positive")
	}
}

func TestBackup(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := insertSetting(ctx, d.Write, "backed-up"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "nested", "backup.db")
	if err := d.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := d.Backup(ctx, dest); err == nil {
		t.Fatal("Backup must refuse to overwrite")
	}
	b, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var n int
	if err := b.Read.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key='backed-up'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup content: n=%d err=%v", n, err)
	}
	if v, err := b.SchemaVersion(ctx); err != nil || v != latestVersion(t) {
		t.Fatalf("backup schema version = %d, %v", v, err)
	}
}

// latestVersion is the highest embedded migration version.
func latestVersion(t *testing.T) int {
	t.Helper()
	all, err := Migrations()
	if err != nil || len(all) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	return all[len(all)-1].Version
}
