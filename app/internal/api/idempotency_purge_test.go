package api_test

import (
	"context"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/testutil"
)

// TestPurgeStoredResponses covers the purge helper used by GDPR erase: a
// stored response containing the value loses its body, and a retry with
// the same key is answered 409 instead of being replayed or run again.
func TestPurgeStoredResponses(t *testing.T) {
	hs := newHarness(t, api.Config{})
	ctx := context.Background()
	post := func(key, body string) resp {
		return hs.do("POST", "/api/v1/things", adminToken, body, api.IdempotencyHeader, key)
	}
	if r := post("k1", `{"name":"a&b@x.io"}`); r.status != 201 {
		t.Fatalf("first = %d %s", r.status, r.body)
	}
	if r := post("k2", `{"name":"other"}`); r.status != 201 {
		t.Fatalf("second = %d", r.status)
	}

	for _, s := range []string{"", "nobody@x.io"} {
		if n, err := api.CountStoredResponses(ctx, hs.db.Read, s); err != nil || n != 0 {
			t.Fatalf("count %q = %d, %v", s, n, err)
		}
	}
	if n, err := api.CountStoredResponses(ctx, hs.db.Read, "a&b@x.io"); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	n, err := api.PurgeStoredResponses(ctx, hs.db.Write, "a&b@x.io")
	if err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if n, _ := api.CountStoredResponses(ctx, hs.db.Read, "a&b@x.io"); n != 0 {
		t.Fatalf("count after purge = %d", n)
	}
	if n := testutil.Count(t, hs.db.Read, `SELECT count(*) FROM idempotency_keys WHERE instr(CAST(response_body AS TEXT), 'x.io') > 0`); n != 0 {
		t.Fatalf("%d responses still hold the value", n)
	}

	before := hs.created.Load()
	if r := post("k1", `{"name":"a&b@x.io"}`); r.status != 409 {
		t.Fatalf("retry after purge = %d %s", r.status, r.body)
	}
	if hs.created.Load() != before {
		t.Fatal("the purged request ran again")
	}
	if r := post("k2", `{"name":"other"}`); r.status != 201 || r.header.Get(api.IdempotencyReplayHeader) != "true" {
		t.Fatalf("untouched key = %d %v", r.status, r.header)
	}
	if n, err := api.PurgeStoredResponses(ctx, hs.db.Write, ""); err != nil || n != 0 {
		t.Fatalf("empty purge = %d, %v", n, err)
	}
}
