package segments_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/segments"
	"mailroom/internal/testutil"
)

func TestCRUD(t *testing.T) {
	f := newFixture(t)
	ctx := core.WithActor(context.Background(), core.Actor{Type: core.ActorUser, ID: "1", Name: "owner@example.com"})
	vip := parse(t, `{"rules":[{"field":"tag","op":"has","value":"vip"}]}`)

	sg, err := f.svc.Create(ctx, segments.CreateInput{Name: "  VIPs ", Filter: vip}, false)
	if err != nil {
		t.Fatal(err)
	}
	if sg.ID == 0 || sg.Name != "VIPs" || sg.Version != 1 || !sg.CreatedAt.Equal(testutil.Epoch) {
		t.Fatalf("created = %+v", sg)
	}
	got, err := f.svc.Get(ctx, sg.ID)
	if err != nil || mustJSON(t, got) != mustJSON(t, sg) {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	// Names are unique ignoring case.
	if _, err := f.svc.Create(ctx, segments.CreateInput{Name: "vips", Filter: vip}, false); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	for _, tt := range []struct {
		name  string
		in    segments.CreateInput
		field string
	}{
		{"empty name", segments.CreateInput{Name: " "}, "name"},
		{"long name", segments.CreateInput{Name: string(make([]rune, 101))}, "name"},
		{"control char", segments.CreateInput{Name: "a\nb"}, "name"},
		{"bad filter", segments.CreateInput{Name: "x", Filter: segments.SegmentFilter{Rules: []segments.SegmentRule{{Field: "tag", Op: "has"}}}}, "filter.rules[0].value"},
		{"bad match", segments.CreateInput{Name: "x", Filter: segments.SegmentFilter{Match: "?"}}, "filter.match"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.svc.Create(ctx, tt.in, false); !isField(err, tt.field) {
				t.Fatalf("err = %v, want field %s", err, tt.field)
			}
		})
	}

	// Dry run: validated and returned, but not stored.
	dry, err := f.svc.Create(ctx, segments.CreateInput{Name: "Dry", Filter: vip}, true)
	if err != nil || dry.Name != "Dry" {
		t.Fatalf("dry create = %+v, %v", dry, err)
	}
	if n := testutil.Count(t, f.db.Read, `SELECT count(*) FROM segments`); n != 1 {
		t.Fatalf("segments after dry run = %d", n)
	}

	// Update.
	f.clock.Advance(time.Hour)
	newName := "VIP customers"
	both := parse(t, `{"rules":[{"field":"tags","op":"has_all","values":["vip","customer"]}]}`)
	up, err := f.svc.Update(ctx, sg.ID, segments.UpdateInput{Name: &newName, Filter: &both}, 1, false)
	if err != nil || up.Version != 2 || up.Name != newName || !up.UpdatedAt.Equal(testutil.Epoch.Add(time.Hour)) ||
		mustJSON(t, up.Filter) != mustJSON(t, both) {
		t.Fatalf("update = %+v, %v", up, err)
	}
	if _, err := f.svc.Update(ctx, sg.ID, segments.UpdateInput{Name: &newName}, 1, false); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale version: %v", err)
	}
	same := "VIP customers"
	noop, err := f.svc.Update(ctx, sg.ID, segments.UpdateInput{Name: &same, Filter: &both}, 2, false)
	if err != nil || noop.Version != 2 {
		t.Fatalf("no-op update = %+v, %v", noop, err)
	}
	dryName := "Would be"
	dryUp, err := f.svc.Update(ctx, sg.ID, segments.UpdateInput{Name: &dryName}, 0, true)
	if err != nil || dryUp.Name != dryName || dryUp.Version != 3 {
		t.Fatalf("dry update = %+v, %v", dryUp, err)
	}
	if cur, _ := f.svc.Get(ctx, sg.ID); cur.Name != newName || cur.Version != 2 {
		t.Fatalf("after dry update = %+v", cur)
	}
	other, err := f.svc.Create(ctx, segments.CreateInput{Name: "Other"}, false)
	if err != nil {
		t.Fatal(err)
	}
	clash := "vip CUSTOMERS"
	if _, err := f.svc.Update(ctx, other.ID, segments.UpdateInput{Name: &clash}, 0, false); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("rename clash: %v", err)
	}
	// Renaming a segment to a different case of its own name is fine.
	recased := "VIP Customers"
	if up, err := f.svc.Update(ctx, sg.ID, segments.UpdateInput{Name: &recased}, 0, false); err != nil || up.Name != recased {
		t.Fatalf("recase = %+v, %v", up, err)
	}
	bad := segments.SegmentFilter{Rules: []segments.SegmentRule{{Field: "status", Op: "is", Value: "gone"}}}
	if _, err := f.svc.Update(ctx, sg.ID, segments.UpdateInput{Filter: &bad}, 0, false); !isField(err, "filter.rules[0].value") {
		t.Fatalf("bad filter update: %v", err)
	}
	if _, err := f.svc.Update(ctx, 999, segments.UpdateInput{Name: &newName}, 0, false); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("update unknown: %v", err)
	}

	// List pages by ID.
	page, err := f.svc.List(ctx, 0, 1)
	if err != nil || len(page) != 1 || page[0].ID != sg.ID {
		t.Fatalf("list page 1 = %+v, %v", page, err)
	}
	page, err = f.svc.List(ctx, sg.ID, 10)
	if err != nil || len(page) != 1 || page[0].ID != other.ID {
		t.Fatalf("list page 2 = %+v, %v", page, err)
	}

	// Delete.
	if err := f.svc.Delete(ctx, other.ID, 7, false); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := f.svc.Delete(ctx, other.ID, 0, true); err != nil {
		t.Fatalf("dry delete: %v", err)
	}
	if _, err := f.svc.Get(ctx, other.ID); err != nil {
		t.Fatalf("dry delete removed the segment: %v", err)
	}
	if err := f.svc.Delete(ctx, other.ID, 1, false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := f.svc.Get(ctx, other.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
	if err := f.svc.Delete(ctx, other.ID, 0, false); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}

	// Audit: one row per real write, with the actor.
	recs, err := audit.New(f.db).List(ctx, audit.Filter{TargetType: "segment"})
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, r := range recs {
		ops = append(ops, r.Operation)
		if r.Actor.Name != "owner@example.com" {
			t.Fatalf("audit actor = %+v", r.Actor)
		}
	}
	slices.Sort(ops)
	want := []string{"segment.create", "segment.create", "segment.delete", "segment.update", "segment.update"}
	if !slices.Equal(ops, want) {
		t.Fatalf("audit ops = %v, want %v", ops, want)
	}
}

func TestDeleteReferencedByBroadcast(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sg, err := f.svc.Create(ctx, segments.CreateInput{Name: "Target"}, false)
	if err != nil {
		t.Fatal(err)
	}
	audience := fmt.Sprintf(`{"type":"segment","segment_id":%d,"segment_name":"Target"}`, sg.ID)
	for _, status := range []string{"draft", "scheduled", "sending", "paused"} {
		t.Run(status, func(t *testing.T) {
			testutil.Exec(t, f.db, `UPDATE broadcasts SET audience = ?, status = ? WHERE id = ?`, audience, status, f.b1)
			if err := f.svc.Delete(ctx, sg.ID, 0, false); !errors.Is(err, core.ErrConflict) {
				t.Fatalf("delete while %s: %v", status, err)
			}
			if err := f.svc.Delete(ctx, sg.ID, 0, true); !errors.Is(err, core.ErrConflict) {
				t.Fatalf("dry delete while %s: %v", status, err)
			}
		})
	}
	// Other segments and finished broadcasts don't block.
	testutil.Exec(t, f.db, `UPDATE broadcasts SET audience = ?, status = 'draft' WHERE id = ?`,
		fmt.Sprintf(`{"type":"segment","segment_id":%d}`, sg.ID+100), f.b2)
	for _, status := range []string{"sent", "cancelled"} {
		testutil.Exec(t, f.db, `UPDATE broadcasts SET status = ? WHERE id = ?`, status, f.b1)
	}
	if err := f.svc.Delete(ctx, sg.ID, 0, false); err != nil {
		t.Fatalf("delete after broadcast finished: %v", err)
	}
}

func TestWhereSQL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var resolver core.SegmentResolver = f.svc
	sg, err := f.svc.Create(ctx, segments.CreateInput{Name: "Customers or VIPs",
		Filter: parse(t, `{"match":"any","rules":[{"field":"tag","op":"has","value":"customer"},{"field":"tag","op":"has","value":"vip"}]}`)}, false)
	if err != nil {
		t.Fatal(err)
	}
	expr, args, err := resolver.WhereSQL(ctx, sg.ID)
	if err != nil {
		t.Fatal(err)
	}
	// No implicit status filter: cat (unsubscribed) matches.
	all := testutil.Count(t, f.db.Read, `SELECT count(*) FROM contacts c WHERE `+expr, args...)
	if all != 3 {
		t.Fatalf("members = %d, want 3", all)
	}
	// The way a sender uses it.
	rows, err := f.db.Read.QueryContext(ctx, `SELECT c.email FROM contacts c WHERE c.status = 'active' AND `+expr+` ORDER BY c.id`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var emails []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		emails = append(emails, e)
	}
	if !slices.Equal(emails, []string{"ann@example.com", "bob@example.org"}) {
		t.Fatalf("active members = %v", emails)
	}

	if _, _, err := resolver.WhereSQL(ctx, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown segment: %v", err)
	}

	// A stored filter that no longer validates can still be read, but not evaluated.
	testutil.Exec(t, f.db, `INSERT INTO segments (name, filter, created_at, updated_at) VALUES ('Broken', '{"rules":[{"field":"tag","op":"has"}]}', ?, ?)`,
		"2026-01-05T09:00:00.000000Z", "2026-01-05T09:00:00.000000Z")
	var brokenID int64
	if err := f.db.Read.QueryRow(`SELECT id FROM segments WHERE name = 'Broken'`).Scan(&brokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(ctx, brokenID); err != nil {
		t.Fatalf("get broken: %v", err)
	}
	if _, _, err := resolver.WhereSQL(ctx, brokenID); !isField(err, "filter.rules[0].value") {
		t.Fatalf("where broken: %v", err)
	}
	if _, err := f.svc.CountSegment(ctx, brokenID); !isField(err, "filter.rules[0].value") {
		t.Fatalf("count broken: %v", err)
	}
}

func TestWithinDaysUsesClock(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sg, err := f.svc.Create(ctx, segments.CreateInput{Name: "New", Filter: parse(t, `{"rules":[{"field":"created_at","op":"within_days","days":2}]}`)}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		advance time.Duration
		want    int64
	}{{0, 2}, {day, 2}, {2 * day, 1}, {3 * day, 0}} {
		f.clock.Set(testutil.Epoch.Add(tt.advance))
		c, err := f.svc.CountSegment(ctx, sg.ID)
		if err != nil || c.Total != tt.want {
			t.Fatalf("at +%s: %+v, %v; want %d", tt.advance, c, err, tt.want)
		}
	}
}

func TestMembersAndPreview(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	everyone := segments.SegmentFilter{}
	sg, err := f.svc.Create(ctx, segments.CreateInput{Name: "Everyone"}, false)
	if err != nil {
		t.Fatal(err)
	}

	// Pages by ID.
	var seen []string
	var after int64
	for {
		page, err := f.svc.SegmentMembers(ctx, sg.ID, segments.MemberQuery{AfterID: after, Limit: 4})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			seen = append(seen, local(m.Email))
			after = m.ID
		}
	}
	if len(seen) != len(fixtureContacts) || seen[0] != "ann" || seen[5] != "f_y" {
		t.Fatalf("members = %v", seen)
	}

	active, err := f.svc.Members(ctx, everyone, segments.MemberQuery{Limit: 10, Status: core.StatusActive})
	if err != nil || !slices.Equal(memberNames(active), []string{"ann", "bob", "f_y"}) {
		t.Fatalf("active members = %v, %v", memberNames(active), err)
	}
	if _, err := f.svc.Members(ctx, everyone, segments.MemberQuery{Limit: 10, Status: "gone"}); !isField(err, "status") {
		t.Fatalf("bad status: %v", err)
	}
	if none, err := f.svc.Members(ctx, everyone, segments.MemberQuery{}); err != nil || len(none) != 0 {
		t.Fatalf("limit 0 = %v, %v", none, err)
	}

	// Members carry tags (sorted) and attributes.
	ann := active[0]
	if ann.FirstName != "Ann" || ann.LastName != "Berg" || ann.Status != "active" || !slices.Equal(ann.Tags, []string{"customer", "VIP"}) ||
		ann.Attributes["plan"] != "Pro" || !ann.CreatedAt.Equal(testutil.Epoch.Add(-100*day)) {
		t.Fatalf("ann = %+v", ann)
	}
	if active[2].Tags == nil || len(active[2].Tags) != 0 {
		t.Fatalf("untagged member tags = %#v", active[2].Tags)
	}

	// Preview: counts plus the newest matching contacts.
	p, err := f.svc.Preview(ctx, parse(t, `{"rules":[{"field":"email","op":"contains","value":"example"}]}`), 2)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 5 || p.Active != 3 || len(p.Sample) != 2 || local(p.Sample[0].Email) != "f_y" || local(p.Sample[1].Email) != "eve" {
		t.Fatalf("preview = %+v", p)
	}
	if p, err := f.svc.Preview(ctx, everyone, 0); err != nil || len(p.Sample) != len(fixtureContacts) {
		t.Fatalf("default sample = %d, %v", len(p.Sample), err)
	}
	for _, n := range []int{-1, segments.MaxSampleLimit + 1} {
		if _, err := f.svc.Preview(ctx, everyone, n); !isField(err, "sample_limit") {
			t.Fatalf("sample_limit %d: %v", n, err)
		}
	}
	if _, err := f.svc.Preview(ctx, segments.SegmentFilter{Match: "?"}, 1); !isField(err, "filter.match") {
		t.Fatalf("bad filter preview: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, p)), &raw); err != nil || raw["total"] == nil || raw["sample"] == nil {
		t.Fatalf("preview JSON = %s", mustJSON(t, p))
	}
}
