package audit_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

var tokenActor = core.Actor{Type: core.ActorToken, ID: "3", Name: "setup-agent"}

func TestLogRecordsActorAndChanges(t *testing.T) {
	d := testutil.NewDB(t)
	clock := testutil.NewClock(time.Time{})
	audit.SetClock(clock)
	t.Cleanup(func() { audit.SetClock(nil) })
	ctx := core.WithActor(context.Background(), tokenActor)

	err := d.WithTx(ctx, func(tx *sql.Tx) error {
		return audit.Log(ctx, tx, "contact.update", "contact", "42",
			audit.Diff(nil, "first_name", "Ann", "Anna"))
	})
	if err != nil {
		t.Fatal(err)
	}
	// A rolled-back transaction leaves no audit record.
	boom := errors.New("boom")
	err = d.WithTx(ctx, func(tx *sql.Tx) error {
		if err := audit.Log(ctx, tx, "contact.delete", "contact", "42", nil); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// Without an actor in ctx, the system actor is recorded.
	if err := audit.Write(context.Background(), d.Write, audit.Entry{Operation: "retention.run", At: testutil.Epoch.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := audit.Log(ctx, d.Write, "", "", "", nil); err == nil {
		t.Fatal("empty operation accepted")
	}

	recs, err := audit.New(d).List(context.Background(), audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d", len(recs))
	}
	sys, upd := recs[0], recs[1]
	if sys.Actor.Type != core.ActorSystem || sys.Operation != "retention.run" || string(sys.Changes) != "{}" ||
		!sys.CreatedAt.Equal(testutil.Epoch.Add(time.Hour)) {
		t.Fatalf("system record = %+v", sys)
	}
	if upd.Actor != (audit.Actor{Type: "token", ID: "3", Name: "setup-agent"}) || upd.TargetType != "contact" ||
		upd.TargetID != "42" || !upd.CreatedAt.Equal(testutil.Epoch) {
		t.Fatalf("update record = %+v", upd)
	}
	var changes map[string]audit.Change
	if err := json.Unmarshal(upd.Changes, &changes); err != nil || changes["first_name"] != (audit.Change{From: "Ann", To: "Anna"}) {
		t.Fatalf("changes = %s (%v)", upd.Changes, err)
	}
	if len(audit.Diff(nil, "x", 1, 1)) != 0 {
		t.Fatal("Diff recorded an unchanged field")
	}
}

type fakeAuth struct{}

func (fakeAuth) AuthenticateToken(context.Context, string) (api.Principal, error) {
	return api.Principal{Actor: tokenActor, Scope: api.ScopeRead}, nil
}

func (fakeAuth) AuthenticateSession(context.Context, string) (api.Principal, string, error) {
	return api.Principal{}, "", api.ErrUnauthorized
}

func TestListAPI(t *testing.T) {
	d := testutil.NewDB(t)
	ctx := context.Background()
	user := core.WithActor(ctx, core.Actor{Type: core.ActorUser, ID: "1", Name: "owner@example.com"})
	tok := core.WithActor(ctx, tokenActor)
	cli := core.WithActor(ctx, core.Actor{Type: core.ActorCLI})
	entries := []struct {
		ctx context.Context
		e   audit.Entry
	}{
		{user, audit.Entry{Operation: "contact.create", TargetType: "contact", TargetID: "1"}},
		{tok, audit.Entry{Operation: "contact.update", TargetType: "contact", TargetID: "1"}},
		{tok, audit.Entry{Operation: "contact.update", TargetType: "contact", TargetID: "2"}},
		{cli, audit.Entry{Operation: "token.create", TargetType: "token", TargetID: "3"}},
		{user, audit.Entry{Operation: "settings.update", TargetType: "settings", TargetID: "app"}},
	}
	for i, e := range entries {
		e.e.At = testutil.Epoch.Add(time.Duration(i) * time.Hour)
		if err := audit.Write(e.ctx, d.Write, e.e); err != nil {
			t.Fatal(err)
		}
	}

	a, err := api.New(api.Config{DB: d, BaseURL: "https://x.example.com", Auth: fakeAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	audit.New(d).RegisterAPI(a)
	h := a.Handler()
	get := func(query string) (int, api.ListBody[audit.Record]) {
		req := httptest.NewRequest("GET", "/api/v1/audit"+query, nil)
		req.Header.Set("Authorization", "Bearer mr_x")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body api.ListBody[audit.Record]
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	ops := func(b api.ListBody[audit.Record]) []string {
		var out []string
		for _, r := range b.Items {
			out = append(out, r.Operation+"/"+r.TargetID)
		}
		return out
	}
	tests := []struct {
		query string
		want  []string
	}{
		{"", []string{"settings.update/app", "token.create/3", "contact.update/2", "contact.update/1", "contact.create/1"}},
		{"?target_type=contact&target_id=1", []string{"contact.update/1", "contact.create/1"}},
		{"?actor=token:3", []string{"contact.update/2", "contact.update/1"}},
		{"?actor=setup-agent", []string{"contact.update/2", "contact.update/1"}},
		{"?actor=owner@example.com", []string{"settings.update/app", "contact.create/1"}},
		{"?actor=cli", []string{"token.create/3"}},
		{"?operation=contact.update", []string{"contact.update/2", "contact.update/1"}},
		{"?since=2026-01-05T11:00:00Z", []string{"settings.update/app", "token.create/3", "contact.update/2"}},
		{"?since=2026-01-05T10:00:00Z&until=2026-01-05T12:00:00Z", []string{"contact.update/2", "contact.update/1"}},
	}
	for _, tt := range tests {
		code, body := get(tt.query)
		if code != 200 {
			t.Fatalf("%s: status %d", tt.query, code)
		}
		if got := ops(body); !equal(got, tt.want) {
			t.Errorf("%s = %v, want %v", tt.query, got, tt.want)
		}
	}

	// Pagination, newest first.
	_, p1 := get("?limit=2")
	_, p2 := get("?limit=2&cursor=" + p1.NextCursor)
	_, p3 := get("?limit=2&cursor=" + p2.NextCursor)
	got := append(append(ops(p1), ops(p2)...), ops(p3)...)
	if !equal(got, tests[0].want) || p3.NextCursor != "" {
		t.Fatalf("paged = %v (last cursor %q)", got, p3.NextCursor)
	}
	if code, _ := get("?since=yesterday"); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad since = %d", code)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
