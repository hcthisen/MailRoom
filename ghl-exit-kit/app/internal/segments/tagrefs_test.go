package segments_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"mailroom/internal/audit"
	"mailroom/internal/segments"
)

func TestTagNames(t *testing.T) {
	f := parse(t, `{"rules":[
		{"field":"tag","op":"has","value":"VIP"},
		{"field":"email","op":"contains","value":"x"},
		{"match":"any","rules":[
			{"field":"tags","op":"has_none","values":["vip","customer"]},
			{"rules":[{"field":"tag","op":"not_has","value":"Customer"},{"field":"tag","op":"has","value":"new"}]}
		]}
	]}`)
	if got, want := segments.TagNames(f), []string{"VIP", "customer", "new"}; !slices.Equal(got, want) {
		t.Fatalf("TagNames = %v, want %v", got, want)
	}
	if got := segments.TagNames(segments.SegmentFilter{}); got == nil || len(got) != 0 {
		t.Fatalf("TagNames(empty) = %#v, want []", got)
	}
}

// TestTagRenameAndMerge shows what a rename does to name-based segments
// and that ReplaceTagName, called in the rename's transaction, keeps them
// selecting the same contacts.
func TestTagRenameAndMerge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	create := func(name, filter string) segments.Segment {
		t.Helper()
		sg, err := f.svc.Create(ctx, segments.CreateInput{Name: name, Filter: parse(t, filter)}, false)
		if err != nil {
			t.Fatal(err)
		}
		return sg
	}
	exclude := create("No customers", `{"rules":[{"field":"tags","op":"has_none","values":["customer"]}]}`)
	include := create("Customers", `{"rules":[{"field":"tag","op":"has","value":"Customer"}]}`)
	nested := create("VIP customers", `{"rules":[{"field":"email","op":"contains","value":"@"},{"match":"any","rules":[{"field":"tags","op":"has_all","values":["customer","vip"]}]}]}`)
	other := create("Example", `{"rules":[{"field":"email","op":"ends_with","value":"example.com"}]}`)

	counts := func(sg segments.Segment) segments.SegmentCounts {
		t.Helper()
		c, err := f.svc.CountSegment(ctx, sg.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := counts(exclude); c.Total != 4 || len(c.MissingTags) != 0 {
		t.Fatalf("before rename: %+v, want 4 members and no missing tags", c)
	}

	// Without ReplaceTagName, a rename silently turns the exclusion into
	// "everyone" and the inclusion into "no one"; missing_tags shows it.
	renameTag(t, f, "customer", "client", false)
	if c := counts(exclude); c.Total != len64(fixtureContacts) || !slices.Equal(c.MissingTags, []string{"customer"}) {
		t.Fatalf("stale exclusion: %+v", c)
	}
	if c := counts(include); c.Total != 0 || !slices.Equal(c.MissingTags, []string{"Customer"}) {
		t.Fatalf("stale inclusion: %+v", c)
	}
	renameTag(t, f, "client", "customer", false)

	// With ReplaceTagName in the same transaction the segments follow.
	if n := renameTag(t, f, "customer", "client", true); n != 3 {
		t.Fatalf("ReplaceTagName changed %d segments, want 3", n)
	}
	if c := counts(exclude); c.Total != 4 || len(c.MissingTags) != 0 {
		t.Fatalf("after rename, exclusion: %+v", c)
	}
	if c := counts(include); c.Total != 2 || len(c.MissingTags) != 0 {
		t.Fatalf("after rename, inclusion: %+v", c)
	}
	got := get(t, f, nested.ID)
	if vs := got.Filter.Rules[1].Rules[0].Values; !slices.Equal(vs, []string{"client", "vip"}) || got.Version != 2 {
		t.Fatalf("nested after rename: values %v, version %d", vs, got.Version)
	}
	if v := get(t, f, include.ID).Filter.Rules[0].Value; v != "client" {
		t.Fatalf("tag value after rename = %v", v)
	}
	if v := get(t, f, other.ID).Version; v != 1 {
		t.Fatalf("unrelated segment version = %d, want 1", v)
	}

	// A merge of VIP into client: has_all [client, vip] becomes [client].
	err := f.db.WithTx(ctx, func(tx *sql.Tx) error {
		n, err := f.svc.ReplaceTagName(ctx, tx, " VIP ", "client")
		if n != 1 {
			t.Errorf("merge changed %d segments, want 1", n)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if vs := get(t, f, nested.ID).Filter.Rules[1].Rules[0].Values; !slices.Equal(vs, []string{"client"}) {
		t.Fatalf("nested after merge: %v", vs)
	}

	// SegmentsUsingTag ignores case; unknown names and bad targets.
	err = f.db.WithTx(ctx, func(tx *sql.Tx) error {
		ids, err := f.svc.SegmentsUsingTag(ctx, tx, "CLIENT")
		if err != nil {
			return err
		}
		if want := []int64{exclude.ID, include.ID, nested.ID}; !slices.Equal(ids, want) {
			t.Errorf("SegmentsUsingTag = %v, want %v", ids, want)
		}
		if ids, err := f.svc.SegmentsUsingTag(ctx, tx, "nobody"); err != nil || len(ids) != 0 {
			t.Errorf("SegmentsUsingTag(nobody) = %v, %v", ids, err)
		}
		if n, err := f.svc.ReplaceTagName(ctx, tx, "nobody", "x"); n != 0 || err != nil {
			t.Errorf("ReplaceTagName(unused) = %d, %v", n, err)
		}
		if _, err := f.svc.ReplaceTagName(ctx, tx, "client", "  "); !isField(err, "name") {
			t.Errorf("ReplaceTagName(empty target) = %v, want a name validation error", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Every rewrite is audited with the tag change.
	recs, err := audit.New(f.db).List(ctx, audit.Filter{TargetType: "segment"})
	if err != nil {
		t.Fatal(err)
	}
	var tagged int
	for _, r := range recs {
		var changes struct {
			Tag *struct{ From, To string } `json:"tag"`
		}
		if err := json.Unmarshal(r.Changes, &changes); err != nil {
			t.Fatal(err)
		}
		if r.Operation == "segment.update" && changes.Tag != nil && changes.Tag.From != "" && changes.Tag.To != "" {
			tagged++
		}
	}
	if tagged != 4 {
		t.Fatalf("audited tag rewrites = %d, want 4 (3 rename + 1 merge)", tagged)
	}
}

// TestPreviewMissingTags checks missing_tags for an unsaved filter.
func TestPreviewMissingTags(t *testing.T) {
	f := newFixture(t)
	p, err := f.svc.Preview(context.Background(), parse(t,
		`{"match":"any","rules":[{"field":"tag","op":"has","value":"vip"},{"field":"tags","op":"has_any","values":["Ghost","customer","later"]}]}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Ghost", "later"}; !slices.Equal(p.MissingTags, want) {
		t.Fatalf("missing_tags = %v, want %v", p.MissingTags, want)
	}
	c, err := f.svc.Count(context.Background(), segments.SegmentFilter{})
	if err != nil || c.MissingTags == nil || len(c.MissingTags) != 0 {
		t.Fatalf("Count(everyone) = %+v, %v; want empty missing_tags", c, err)
	}
}

// renameTag renames a tag like the tags package does, optionally calling
// ReplaceTagName in the same transaction; it returns the segments changed.
func renameTag(t *testing.T, f *fixture, from, to string, replace bool) int {
	t.Helper()
	var n int
	err := f.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE tags SET name = ? WHERE name = ?`, to, from); err != nil {
			return err
		}
		if !replace {
			return nil
		}
		var err error
		n, err = f.svc.ReplaceTagName(context.Background(), tx, from, to)
		return err
	})
	if err != nil {
		t.Fatalf("rename %s -> %s: %v", from, to, err)
	}
	return n
}

func get(t *testing.T, f *fixture, id int64) segments.Segment {
	t.Helper()
	sg, err := f.svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

func len64[T any](s []T) int64 { return int64(len(s)) }
