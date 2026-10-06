package ui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/segments"
	"mailroom/internal/testutil"
)

func TestAudienceAttributesRoundTrip(t *testing.T) {
	in := map[string]any{
		"company": "Acme", "seats": 5.0, "ratio": -0.25, "beta": true, "off": false, "none": nil,
		"zip": "01234", "looks_number": "42", "looks_bool": "true", "quoted": `"hi"`, "spaced": " pad ",
		"multi": "a\nb", "obj": map[string]any{"a": 1.0}, "list": []any{"x", 2.0}, "empty": "",
	}
	text := audFormatAttributes(in)
	out, err := audParseAttributes(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip:\n%q\n in  %#v\n out %#v", text, in, out)
	}
	for _, bad := range []string{"no colon", ": value", "a: 1\na: 2", "j: {broken"} {
		if _, err := audParseAttributes(bad); err == nil {
			t.Errorf("%q: no error", bad)
		} else if ve := (*core.ValidationError)(nil); !asValidation(err, &ve) || ve.Field != "attributes" {
			t.Errorf("%q: error %v, want a validation error on attributes", bad, err)
		}
	}
	if m, err := audParseAttributes("\n  \nk:v\r\n"); err != nil || m["k"] != "v" {
		t.Fatalf("blank lines and CRLF: %v %v", m, err)
	}
}

func asValidation(err error, ve **core.ValidationError) bool {
	v, ok := err.(*core.ValidationError)
	if ok {
		*ve = v
	}
	return ok
}

// A saved filter read into the builder builds the same filter again.
func TestAudienceBuilderRoundTrip(t *testing.T) {
	u := &UI{clock: testutil.NewClock(testutil.Epoch)}
	raw := `{"match":"any","rules":[
		{"field":"tag","op":"has","value":"customer"},
		{"match":"all","rules":[
			{"field":"tags","op":"has_none","values":["vip","staff"]},
			{"match":"any","rules":[{"field":"status","op":"is","value":"active"},{"field":"first_name","op":"is_empty"}]}]},
		{"field":"email","op":"ends_with","value":"@example.com"},
		{"field":"created_at","op":"within_days","days":30},
		{"field":"created_at","op":"before","value":"2026-01-02T03:04:00Z"},
		{"field":"attr","key":"plan","op":"eq","value":"pro"},
		{"field":"attr","key":"seats","op":"neq","value":5},
		{"field":"attr","key":"beta","op":"eq","value":true},
		{"field":"attr","key":"x","op":"not_exists"},
		{"field":"clicked","op":"link","url":"https://example.com/a","broadcast_id":12,"within_days":7}]}`
	want, err := segments.ParseFilter([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	root := u.audNodeFromFilter(want)
	got, err := u.audBuildFilter(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err = segments.Validate(got)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("round trip:\nwant %s\ngot  %s", a, b)
	}
	if d := audDescribe(want); !strings.Contains(d, "has tag customer or (has none of tags vip, staff and (status is active or first name is empty))") {
		t.Errorf("describe = %q", d)
	}
}

func TestAudienceBuilderEdits(t *testing.T) {
	root := &audNode{Path: "filter", Depth: 1, Group: true, Match: segments.MatchAll}
	audApplyEdit(root, "add-group:filter")
	audApplyEdit(root, "add-group:filter.rules[0]")
	audApplyEdit(root, "add-group:filter.rules[0].rules[1]") // depth 3 can't nest further
	audApplyEdit(root, "add-condition:filter.rules[0].rules[1]")
	audApplyEdit(root, "add-condition:filter.rules[9]") // unknown: nothing
	audApplyEdit(root, "remove:filter.rules[0].rules[0]")
	audApplyEdit(root, "bogus")
	g := root.Rules[0]
	if len(root.Rules) != 1 || !g.Group || len(g.Rules) != 1 {
		t.Fatalf("tree = %+v", g)
	}
	inner := g.Rules[0]
	if inner.Path != "filter.rules[0].rules[0]" || inner.Depth != 3 || inner.CanNest() || len(inner.Rules) != 2 {
		t.Fatalf("inner = %+v", inner)
	}
	if inner.Rules[1].Path != "filter.rules[0].rules[0].rules[1]" || inner.Rules[1].Number() != "1.1.2" {
		t.Fatalf("renumbered = %s %s", inner.Rules[1].Path, inner.Rules[1].Number())
	}
	for i := 0; i < segments.MaxGroupRules+5; i++ {
		audApplyEdit(root, "add-condition:filter")
	}
	if len(root.Rules) != segments.MaxGroupRules {
		t.Fatalf("rules = %d", len(root.Rules))
	}
}

func TestAudienceHumanError(t *testing.T) {
	for field, want := range map[string]string{
		"":                               "bad",
		"filter":                         "bad",
		"filter.rules[1]":                "Rule 2: bad",
		"filter.rules[1].rules[0].value": "Rule 2.1, value: bad",
		"filter.rules[0].within_days":    "Rule 1, within days: bad",
		"sample_limit":                   "sample_limit: bad",
	} {
		if got := audHumanError(field, "bad"); got != want {
			t.Errorf("%q = %q, want %q", field, got, want)
		}
	}
}

func TestAudienceBuilderInputErrors(t *testing.T) {
	u := &UI{clock: testutil.NewClock(testutil.Epoch)}
	cases := map[string]*audNode{
		"filter.rules[0].days":         {Field: "created_at", Op: "within_days", Days: "soon"},
		"filter.rules[0].value":        {Field: "attr", Key: "n", Op: "eq", ValueType: "number", Value: "five"},
		"filter.rules[0].broadcast_id": {Field: "clicked", Op: "any_link", BroadcastID: "x"},
	}
	for field, n := range cases {
		n.Path = "filter.rules[0]"
		root := &audNode{Path: "filter", Depth: 1, Group: true, Match: "all", Rules: []*audNode{n}}
		_, err := u.audBuildFilter(root)
		ve, ok := err.(*core.ValidationError)
		if !ok || ve.Field != field {
			t.Errorf("%s: %v", field, err)
		}
	}
	// Dates are read in the settings time zone (UTC without settings).
	n := &audNode{Path: "filter.rules[0]", Field: "created_at", Op: "after", Value: "2026-03-01T10:30"}
	r, err := u.audBuildCondition(n)
	if err != nil || r.Value != time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC).Format(time.RFC3339) {
		t.Fatalf("date = %v %v", r.Value, err)
	}
}
