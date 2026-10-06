package api_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
	"mailroom/internal/testutil"
)

type ledgerInput struct {
	Body struct {
		Name string `json:"name"`
	}
}

type ledgerOutput struct {
	Body struct {
		Name string `json:"name"`
	}
}

// registerLedger adds POST /ledger (and a sensitive POST /ledger-secret)
// that write a settings row in a transaction. Name "fail" writes and then
// fails (rolled back); "boom" commits and then fails.
func registerLedger(hs *harness, runs *atomic.Int64) {
	handler := func(ctx context.Context, in *ledgerInput) (*ledgerOutput, error) {
		runs.Add(1)
		err := hs.db.WithTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, '1', 'x')`,
				in.Body.Name+strconv.FormatInt(runs.Load(), 10)); err != nil {
				return err
			}
			if in.Body.Name == "fail" {
				return errors.New("failed after writing")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if in.Body.Name == "boom" {
			return nil, errors.New("failed after the commit")
		}
		out := &ledgerOutput{}
		out.Body.Name = in.Body.Name
		return out, nil
	}
	api.Register(hs.api, huma.Operation{
		OperationID: "create-ledger", Method: http.MethodPost, Path: "/ledger", DefaultStatus: http.StatusCreated,
		Summary: "Ledger", Description: "Writes a row.", Tags: []string{"Test"},
	}, handler)
	api.Register(hs.api, huma.Operation{
		OperationID: "create-ledger-secret", Method: http.MethodPost, Path: "/ledger-secret", DefaultStatus: http.StatusCreated,
		Summary: "Ledger secret", Description: "Writes a row and returns a secret.", Tags: []string{"Test"},
		Metadata: map[string]any{api.MetaSensitive: true},
	}, handler)
}

func keyStatus(t *testing.T, hs *harness, key string) int {
	t.Helper()
	return testutil.Count(t, hs.db.Read, `SELECT status_code FROM idempotency_keys WHERE key = ?`, key)
}

// TestIdempotencyKeyCommitsWithChanges: the key is written in the same
// transaction as the request's changes, so a request that committed can
// never run twice, even when its response isn't stored.
func TestIdempotencyKeyCommitsWithChanges(t *testing.T) {
	hs := newHarness(t, api.Config{})
	var runs atomic.Int64
	registerLedger(hs, &runs)
	post := func(path, key, name string) resp {
		return hs.do("POST", path, adminToken, `{"name":"`+name+`"}`, api.IdempotencyHeader, key)
	}
	tests := []struct {
		name, path, key, body string
		first, retry          int
		retryReplayed         bool
		runs                  int64 // handler executions after both calls
		stored                int   // idempotency_keys.status_code afterwards
	}{
		{"success is replayed", "/api/v1/ledger", "a", "ok", 201, 201, true, 1, 201},
		{"rolled back runs again", "/api/v1/ledger", "b", "fail", 500, 500, false, 2, -1},
		{"5xx after commit is not re-run", "/api/v1/ledger", "c", "boom", 500, 409, false, 1, 0},
		{"sensitive commit is not re-run", "/api/v1/ledger-secret", "d", "secret", 201, 409, false, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs.Store(0)
			if r := post(tt.path, tt.key, tt.body); r.status != tt.first {
				t.Fatalf("first = %d %s", r.status, r.body)
			}
			r := post(tt.path, tt.key, tt.body)
			if r.status != tt.retry || (r.header.Get(api.IdempotencyReplayHeader) == "true") != tt.retryReplayed {
				t.Fatalf("retry = %d %v %s", r.status, r.header, r.body)
			}
			if runs.Load() != tt.runs {
				t.Fatalf("handler ran %d times, want %d", runs.Load(), tt.runs)
			}
			if got := keyStatus(t, hs, tt.key); got != tt.stored {
				t.Fatalf("stored status = %d, want %d", got, tt.stored)
			}
		})
	}
	if n := testutil.Count(t, hs.db.Read, `SELECT count(*) FROM settings WHERE key LIKE 'fail%'`); n != 0 {
		t.Fatal("rolled-back writes were kept")
	}
}

func TestClientIPTrustedProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:ffff::/48")}
	tests := []struct {
		name    string
		remote  string
		xff     []string
		trusted []netip.Prefix
		want    string
	}{
		{"no trust configured", "10.0.0.5:1", []string{"1.2.3.4"}, nil, "10.0.0.5"},
		{"untrusted peer", "203.0.113.1:1", []string{"1.2.3.4"}, trusted, "203.0.113.1"},
		{"trusted peer without header", "10.0.0.5:1", nil, trusted, "10.0.0.5"},
		{"trusted peer", "10.0.0.5:1", []string{"1.2.3.4"}, trusted, "1.2.3.4"},
		{"spoofed entries on the left are ignored", "10.0.0.5:1", []string{"6.6.6.6, 1.2.3.4"}, trusted, "1.2.3.4"},
		{"trusted hops are skipped", "10.0.0.5:1", []string{"6.6.6.6, 1.2.3.4, 10.0.0.9"}, trusted, "1.2.3.4"},
		{"several headers", "10.0.0.5:1", []string{"6.6.6.6", "1.2.3.4"}, trusted, "1.2.3.4"},
		{"garbage stops the walk", "10.0.0.5:1", []string{"1.2.3.4, nonsense, 10.0.0.9"}, trusted, "10.0.0.9"},
		{"ipv6 proxy", "[2001:db8:ffff::1]:1", []string{"2001:db8:1::7"}, trusted, "2001:db8:1::7"},
		{"mapped ipv4 peer", "[::ffff:10.0.0.5]:1", []string{"1.2.3.4"}, trusted, "1.2.3.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := api.ClientIP(r, tt.trusted); got != tt.want {
				t.Fatalf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAnonymousLimitIgnoresSpoofedForwardedFor: without trusted proxies a
// client can't get a fresh bucket per request by inventing X-Forwarded-For.
func TestAnonymousLimitIgnoresSpoofedForwardedFor(t *testing.T) {
	hs := newHarness(t, api.Config{Rate: 1, Burst: 2})
	codes := []int{}
	for i := range 4 {
		codes = append(codes, hs.do("GET", "/api/v1/openapi.json", "", "", "X-Forwarded-For", "198.51.100."+strconv.Itoa(i)).status)
	}
	if codes[2] != 429 || codes[3] != 429 {
		t.Fatalf("statuses = %v", codes)
	}
}

func TestIPKey(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.3.4":                  "1.2.3.4",
		"::ffff:1.2.3.4":           "1.2.3.4",
		"2001:db8:1:2:3:4:5:6":     "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1":     "2001:db8:1:2::/64",
		"not an ip":                "not an ip",
		"2001:db8:1:3::1":          "2001:db8:1:3::/64",
		"fe80::1":                  "fe80::/64",
		"2001:0db8:0001:0002::0cd": "2001:db8:1:2::/64",
	} {
		if got := api.IPKey(in); got != want {
			t.Errorf("IPKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimiterBoundsKeys(t *testing.T) {
	clock := testutil.NewClock(time.Time{})
	l := api.NewLimiter(1, 1, clock)
	l.SetMaxKeys(3)
	for _, k := range []string{"a", "b", "c"} {
		if !l.Allow(k).Allowed {
			t.Fatalf("key %s refused", k)
		}
	}
	// Beyond the limit, new keys share one bucket.
	if !l.Allow("d").Allowed || l.Allow("e").Allowed {
		t.Fatal("overflow keys don't share a bucket")
	}
	for i := range 1000 {
		l.Allow("spoofed-" + strconv.Itoa(i))
	}
	if n := l.Len(); n > 4 {
		t.Fatalf("limiter holds %d buckets", n)
	}
	// Known keys keep their own bucket, and idle buckets are swept later.
	clock.Advance(time.Minute)
	if !l.Allow("f").Allowed || l.Len() > 2 {
		t.Fatalf("after sweep: len %d", l.Len())
	}
}

func TestErrorCodesAreClosed(t *testing.T) {
	closed := map[string]bool{
		api.CodeNotFound: true, api.CodeConflict: true, api.CodeValidation: true, api.CodePreconditionFailed: true,
		api.CodeForbidden: true, api.CodeUnauthorized: true, api.CodeRateLimited: true, api.CodeInternal: true,
	}
	if len(closed) != 8 {
		t.Fatal("codes are not distinct")
	}
	for status := 400; status < 600; status++ {
		if c := api.CodeForStatus(status); !closed[c] {
			t.Errorf("CodeForStatus(%d) = %q, outside the contract", status, c)
		}
	}
	hs := newHarness(t, api.Config{})
	if e := hs.do("DELETE", "/api/v1/things", adminToken, "").errBody(t); !closed[e.Code] {
		t.Fatalf("405 code = %q", e.Code)
	}
}
