package segments_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"mailroom/internal/segments"
	"mailroom/internal/testutil"
)

// TestEvaluate runs every field and operator (and nesting) against the
// fixture contacts and checks the members and counts.
func TestEvaluate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cond := func(rule string) string { return `{"rules":[` + rule + `]}` }
	all := []string{"ann", "bob", "cat", "dan", "eve", "f_y"}
	offer := `"https://example.com/offer"`

	tests := []struct {
		name   string
		filter string
		want   []string
	}{
		{"empty filter matches everyone", `{"match":"all","rules":[]}`, all},
		{"empty any filter matches everyone", `{"match":"any","rules":[]}`, all},

		{"tag has", cond(`{"field":"tag","op":"has","value":"customer"}`), []string{"ann", "bob"}},
		{"tag has ignores case", cond(`{"field":"tag","op":"has","value":" CUSTOMER "}`), []string{"ann", "bob"}},
		{"tag has unknown tag", cond(`{"field":"tag","op":"has","value":"nobody"}`), nil},
		{"tag not_has", cond(`{"field":"tag","op":"not_has","value":"customer"}`), []string{"cat", "dan", "eve", "f_y"}},
		{"tags has_all", cond(`{"field":"tags","op":"has_all","values":["customer","vip"]}`), []string{"ann"}},
		{"tags has_all with unknown tag", cond(`{"field":"tags","op":"has_all","values":["customer","nobody"]}`), nil},
		{"tags has_any", cond(`{"field":"tags","op":"has_any","values":["vip","newsletter"]}`), []string{"ann", "cat", "eve"}},
		{"tags has_none", cond(`{"field":"tags","op":"has_none","values":["customer","vip"]}`), []string{"dan", "eve", "f_y"}},

		{"status is", cond(`{"field":"status","op":"is","value":"active"}`), []string{"ann", "bob", "f_y"}},
		{"status is_not", cond(`{"field":"status","op":"is_not","value":"active"}`), []string{"cat", "dan", "eve"}},

		{"email eq ignores case", cond(`{"field":"email","op":"eq","value":"ANN@example.com"}`), []string{"ann"}},
		{"email contains", cond(`{"field":"email","op":"contains","value":"example"}`), []string{"ann", "bob", "dan", "eve", "f_y"}},
		{"email ends_with", cond(`{"field":"email","op":"ends_with","value":"@example.com"}`), []string{"ann", "dan"}},
		{"email contains _ literally", cond(`{"field":"email","op":"contains","value":"_"}`), []string{"f_y"}},
		{"email contains % literally", cond(`{"field":"email","op":"contains","value":"%"}`), nil},
		{"email contains backslash literally", cond(`{"field":"email","op":"contains","value":"\\"}`), nil},

		{"first_name eq ignores case", cond(`{"field":"first_name","op":"eq","value":"ann"}`), []string{"ann"}},
		{"first_name contains", cond(`{"field":"first_name","op":"contains","value":"A"}`), []string{"ann", "dan", "f_y"}},
		{"first_name is_empty", cond(`{"field":"first_name","op":"is_empty"}`), []string{"cat"}},
		{"first_name not_empty", cond(`{"field":"first_name","op":"not_empty"}`), []string{"ann", "bob", "dan", "eve", "f_y"}},
		{"last_name eq with quote", cond(`{"field":"last_name","op":"eq","value":"o'neil"}`), []string{"dan"}},
		{"last_name contains quote", cond(`{"field":"last_name","op":"contains","value":"'"}`), []string{"dan"}},
		{"last_name is_empty", cond(`{"field":"last_name","op":"is_empty"}`), []string{"bob", "cat", "f_y"}},
		{"last_name not_empty", cond(`{"field":"last_name","op":"not_empty"}`), []string{"ann", "dan", "eve"}},

		{"created_at before", cond(`{"field":"created_at","op":"before","value":"2025-11-26T09:00:00Z"}`), []string{"ann", "dan"}},
		{"created_at before is strict", cond(`{"field":"created_at","op":"before","value":"2026-01-02T09:00:00Z"}`), []string{"ann", "bob", "dan"}},
		{"created_at after is strict", cond(`{"field":"created_at","op":"after","value":"2026-01-02T09:00:00Z"}`), []string{"cat", "eve"}},
		{"created_at after with offset", cond(`{"field":"created_at","op":"after","value":"2026-01-02T10:00:00+01:00"}`), []string{"cat", "eve"}},
		{"created_at within_days", cond(`{"field":"created_at","op":"within_days","days":7}`), []string{"cat", "eve", "f_y"}},
		{"created_at within_days 1", cond(`{"field":"created_at","op":"within_days","days":1}`), []string{"cat", "eve"}},

		{"attr eq string ignores case", cond(`{"field":"attr","key":"plan","op":"eq","value":"pro"}`), []string{"ann"}},
		{"attr eq string 5", cond(`{"field":"attr","key":"plan","op":"eq","value":"5"}`), []string{"eve"}},
		{"attr eq number does not match string", cond(`{"field":"attr","key":"plan","op":"eq","value":5}`), nil},
		{"attr eq integer", cond(`{"field":"attr","key":"seats","op":"eq","value":5}`), []string{"ann"}},
		{"attr eq real", cond(`{"field":"attr","key":"seats","op":"eq","value":1.5}`), []string{"bob"}},
		{"attr eq true", cond(`{"field":"attr","key":"trial","op":"eq","value":true}`), []string{"bob"}},
		{"attr eq false", cond(`{"field":"attr","key":"trial","op":"eq","value":false}`), []string{"ann"}},
		{"attr eq number does not match bool", cond(`{"field":"attr","key":"trial","op":"eq","value":1}`), nil},
		{"attr neq includes missing and null", cond(`{"field":"attr","key":"plan","op":"neq","value":"PRO"}`), []string{"bob", "cat", "dan", "eve", "f_y"}},
		{"attr neq bool", cond(`{"field":"attr","key":"trial","op":"neq","value":true}`), []string{"ann", "cat", "dan", "eve", "f_y"}},
		{"attr contains", cond(`{"field":"attr","key":"plan","op":"contains","value":"AS"}`), []string{"bob"}},
		{"attr contains wildcards literally", cond(`{"field":"attr","key":"note","op":"contains","value":"0%_"}`), []string{"cat"}},
		{"attr contains % is not a wildcard", cond(`{"field":"attr","key":"note","op":"contains","value":"5%f"}`), nil},
		{"attr contains skips numbers", cond(`{"field":"attr","key":"seats","op":"contains","value":"5"}`), nil},
		{"attr exists ignores null", cond(`{"field":"attr","key":"plan","op":"exists"}`), []string{"ann", "bob", "eve"}},
		{"attr not_exists", cond(`{"field":"attr","key":"plan","op":"not_exists"}`), []string{"cat", "dan", "f_y"}},
		{"attr key is case-sensitive", cond(`{"field":"attr","key":"Plan","op":"exists"}`), nil},

		{"clicked any_link", cond(`{"field":"clicked","op":"any_link"}`), []string{"ann", "bob", "cat"}},
		{"clicked link", cond(`{"field":"clicked","op":"link","url":` + offer + `}`), []string{"ann", "cat"}},
		{"clicked link unknown", cond(`{"field":"clicked","op":"link","url":"https://example.com/none"}`), nil},
		{"clicked link in broadcast", cond(fmt.Sprintf(`{"field":"clicked","op":"link","url":%s,"broadcast_id":%d}`, offer, f.b1)), []string{"ann"}},
		{"clicked any_link in broadcast", cond(fmt.Sprintf(`{"field":"clicked","op":"any_link","broadcast_id":%d}`, f.b2)), []string{"bob"}},
		{"clicked any_link within_days", cond(`{"field":"clicked","op":"any_link","within_days":30}`), []string{"ann", "cat"}},
		{"clicked link within_days", cond(`{"field":"clicked","op":"link","url":` + offer + `,"within_days":3}`), []string{"ann"}},
		{"clicked no_click", cond(`{"field":"clicked","op":"no_click"}`), []string{"dan", "eve", "f_y"}},
		{"clicked no_click on url", cond(`{"field":"clicked","op":"no_click","url":` + offer + `}`), []string{"bob", "dan", "eve", "f_y"}},
		{"clicked no_click in broadcast", cond(fmt.Sprintf(`{"field":"clicked","op":"no_click","broadcast_id":%d}`, f.b1)), []string{"bob", "cat", "dan", "eve", "f_y"}},
		{"clicked no_click within_days", cond(`{"field":"clicked","op":"no_click","within_days":30}`), []string{"bob", "dan", "eve", "f_y"}},

		{"match all", `{"match":"all","rules":[{"field":"tag","op":"has","value":"customer"},{"field":"attr","key":"trial","op":"eq","value":true}]}`, []string{"bob"}},
		{"match defaults to all", `{"rules":[{"field":"tag","op":"has","value":"vip"},{"field":"status","op":"is","value":"active"}]}`, []string{"ann"}},
		{"match any", `{"match":"any","rules":[{"field":"email","op":"eq","value":"ann@example.com"},{"field":"email","op":"eq","value":"bob@example.org"}]}`, []string{"ann", "bob"}},
		{"nested group", `{"match":"all","rules":[
			{"field":"status","op":"is","value":"active"},
			{"match":"any","rules":[{"field":"tag","op":"has","value":"vip"},{"field":"attr","key":"trial","op":"eq","value":true}]}]}`,
			[]string{"ann", "bob"}},
		{"depth 3", `{"match":"any","rules":[
			{"match":"all","rules":[
				{"field":"tag","op":"has","value":"vip"},
				{"match":"any","rules":[{"field":"status","op":"is","value":"unsubscribed"},{"field":"email","op":"ends_with","value":"@test.io"}]}]},
			{"field":"first_name","op":"eq","value":"dan"}]}`,
			[]string{"cat", "dan"}},
		{"any of negations", `{"match":"any","rules":[{"field":"tag","op":"not_has","value":"customer"},{"field":"clicked","op":"no_click"}]}`,
			[]string{"cat", "dan", "eve", "f_y"}},
		{"all with empty result", `{"match":"all","rules":[{"field":"status","op":"is","value":"active"},{"field":"status","op":"is","value":"bounced"}]}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := parse(t, tt.filter)
			ms, err := f.svc.Members(ctx, filter, segments.MemberQuery{Limit: 100})
			if err != nil {
				t.Fatalf("Members: %v", err)
			}
			got := memberNames(ms)
			want := tt.want
			if want == nil {
				want = []string{}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("members = %v, want %v", got, want)
			}
			counts, err := f.svc.Count(ctx, filter)
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			active := 0
			for _, n := range want {
				if n == "ann" || n == "bob" || n == "f_y" {
					active++
				}
			}
			if counts.Total != int64(len(want)) || counts.Active != int64(active) {
				t.Fatalf("counts = %+v, want total %d active %d", counts, len(want), active)
			}
		})
	}
}

// TestInjection checks that hostile values are data, never SQL.
func TestInjection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	hostile := []string{
		`x'; DROP TABLE contacts; --`,
		`' OR 1=1 --`,
		`" OR ""="`,
		`') OR ('1'='1`,
		`%' OR c.status LIKE '%`,
		`\'; DELETE FROM contacts; --`,
		`$.plan') OR 1=1 --`,
	}
	for _, h := range hostile {
		for _, rule := range []string{
			`{"field":"tag","op":"has","value":%s}`,
			`{"field":"tags","op":"has_any","values":[%s]}`,
			`{"field":"email","op":"eq","value":%s}`,
			`{"field":"email","op":"contains","value":%s}`,
			`{"field":"first_name","op":"eq","value":%s}`,
			`{"field":"last_name","op":"contains","value":%s}`,
			`{"field":"attr","key":"plan","op":"eq","value":%s}`,
			`{"field":"attr","key":"plan","op":"contains","value":%s}`,
			`{"field":"clicked","op":"link","url":%s}`,
		} {
			raw := `{"rules":[` + fmt.Sprintf(rule, mustJSON(t, h)) + `]}`
			filter := parse(t, raw)
			expr, args, err := f.svc.Compile(filter)
			if err != nil {
				t.Fatalf("Compile(%s): %v", raw, err)
			}
			if strings.Contains(expr, h) || strings.Contains(expr, "DROP") || strings.Contains(expr, "DELETE") {
				t.Fatalf("value leaked into SQL: %s", expr)
			}
			if len(args) == 0 {
				t.Fatalf("no args for %s", raw)
			}
			c, err := f.svc.Count(ctx, filter)
			if err != nil || c.Total != 0 {
				t.Fatalf("Count(%s) = %+v, %v", raw, c, err)
			}
		}
	}
	// Hostile attribute keys never reach SQL: they are rejected.
	for _, key := range []string{`plan"; DROP TABLE contacts; --`, `plan')`, `a.b`, `$`, `plan"`, `ünïcode`, strings.Repeat("k", 65)} {
		raw := fmt.Sprintf(`{"rules":[{"field":"attr","key":%s,"op":"exists"}]}`, mustJSON(t, key))
		if _, err := segments.ParseFilter([]byte(raw)); !isField(err, "rules[0].key") {
			t.Fatalf("key %q: err = %v", key, err)
		}
	}
	if n := testutil.Count(t, f.db.Read, `SELECT count(*) FROM contacts`); n != len(fixtureContacts) {
		t.Fatalf("contacts = %d", n)
	}
}
