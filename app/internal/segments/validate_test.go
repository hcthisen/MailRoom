package segments_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/segments"
)

// isField reports whether err is a *core.ValidationError for field.
func isField(err error, field string) bool {
	var ve *core.ValidationError
	return errors.As(err, &ve) && ve.Field == field && errors.Is(err, core.ErrValidation)
}

func TestValidateErrors(t *testing.T) {
	cond := func(rule string) string { return `{"rules":[` + rule + `]}` }
	group := func(inner string) string { return `{"match":"any","rules":[` + inner + `]}` }
	tag := `{"field":"tag","op":"has","value":"a"}`
	many := func(n int, rule string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = rule
		}
		return strings.Join(parts, ",")
	}

	tests := []struct {
		name, filter, field string
	}{
		{"bad match", `{"match":"some","rules":[]}`, "match"},
		{"unknown field", cond(`{"field":"nope","op":"has"}`), "rules[0].field"},
		{"empty rule", cond(`{}`), "rules[0].field"},
		{"op not valid for field", cond(`{"field":"tag","op":"eq","value":"x"}`), "rules[0].op"},
		{"missing op", cond(`{"field":"status","value":"active"}`), "rules[0].op"},
		{"tag missing value", cond(`{"field":"tag","op":"has"}`), "rules[0].value"},
		{"tag number value", cond(`{"field":"tag","op":"has","value":3}`), "rules[0].value"},
		{"tag empty value", cond(`{"field":"tag","op":"not_has","value":"  "}`), "rules[0].value"},
		{"tag too long", cond(`{"field":"tag","op":"has","value":"` + strings.Repeat("x", 65) + `"}`), "rules[0].value"},
		{"tag with values", cond(`{"field":"tag","op":"has","value":"a","values":["b"]}`), "rules[0].values"},
		{"tags without values", cond(`{"field":"tags","op":"has_any"}`), "rules[0].values"},
		{"tags empty values", cond(`{"field":"tags","op":"has_all","values":[]}`), "rules[0].values"},
		{"tags bad name", cond(`{"field":"tags","op":"has_none","values":["a",""]}`), "rules[0].values[1]"},
		{"tags too many", cond(`{"field":"tags","op":"has_any","values":[` + strings.TrimSuffix(strings.Repeat(`"a",`, 101), ",") + `]}`), "rules[0].values"},
		{"tags with value", cond(`{"field":"tags","op":"has_any","values":["a"],"value":"b"}`), "rules[0].value"},
		{"bad status", cond(`{"field":"status","op":"is","value":"deleted"}`), "rules[0].value"},
		{"status missing", cond(`{"field":"status","op":"is_not"}`), "rules[0].value"},
		{"email empty", cond(`{"field":"email","op":"eq","value":" "}`), "rules[0].value"},
		{"email bool", cond(`{"field":"email","op":"contains","value":true}`), "rules[0].value"},
		{"email with key", cond(`{"field":"email","op":"eq","value":"a","key":"k"}`), "rules[0].key"},
		{"email control char", cond(`{"field":"email","op":"contains","value":"a\u0000b"}`), "rules[0].value"},
		{"email too long", cond(`{"field":"email","op":"contains","value":"` + strings.Repeat("x", 257) + `"}`), "rules[0].value"},
		{"name is_empty with value", cond(`{"field":"first_name","op":"is_empty","value":"x"}`), "rules[0].value"},
		{"name eq missing value", cond(`{"field":"last_name","op":"eq"}`), "rules[0].value"},
		{"created_at bad time", cond(`{"field":"created_at","op":"before","value":"yesterday"}`), "rules[0].value"},
		{"created_at date only", cond(`{"field":"created_at","op":"after","value":"2026-01-01"}`), "rules[0].value"},
		{"created_at with days", cond(`{"field":"created_at","op":"before","value":"2026-01-01T00:00:00Z","days":3}`), "rules[0].days"},
		{"within_days missing", cond(`{"field":"created_at","op":"within_days"}`), "rules[0].days"},
		{"within_days zero", cond(`{"field":"created_at","op":"within_days","days":0}`), "rules[0].days"},
		{"within_days too many", cond(`{"field":"created_at","op":"within_days","days":3651}`), "rules[0].days"},
		{"within_days with value", cond(`{"field":"created_at","op":"within_days","days":3,"value":"x"}`), "rules[0].value"},
		{"attr missing key", cond(`{"field":"attr","op":"exists"}`), "rules[0].key"},
		{"attr bad key", cond(`{"field":"attr","key":"a-b","op":"exists"}`), "rules[0].key"},
		{"attr long key", cond(`{"field":"attr","key":"` + strings.Repeat("k", 65) + `","op":"exists"}`), "rules[0].key"},
		{"attr object value", cond(`{"field":"attr","key":"k","op":"eq","value":{"a":1}}`), "rules[0].value"},
		{"attr array value", cond(`{"field":"attr","key":"k","op":"neq","value":[1]}`), "rules[0].value"},
		{"attr null value", cond(`{"field":"attr","key":"k","op":"eq","value":null}`), "rules[0].value"},
		{"attr contains number", cond(`{"field":"attr","key":"k","op":"contains","value":5}`), "rules[0].value"},
		{"attr long string", cond(`{"field":"attr","key":"k","op":"eq","value":"` + strings.Repeat("x", 257) + `"}`), "rules[0].value"},
		{"attr exists with value", cond(`{"field":"attr","key":"k","op":"exists","value":"x"}`), "rules[0].value"},
		{"attr with url", cond(`{"field":"attr","key":"k","op":"exists","url":"x"}`), "rules[0].url"},
		{"clicked link no url", cond(`{"field":"clicked","op":"link"}`), "rules[0].url"},
		{"clicked link blank url", cond(`{"field":"clicked","op":"link","url":"  "}`), "rules[0].url"},
		{"clicked long url", cond(`{"field":"clicked","op":"link","url":"https://x/` + strings.Repeat("a", 2048) + `"}`), "rules[0].url"},
		{"clicked any_link with url", cond(`{"field":"clicked","op":"any_link","url":"https://x"}`), "rules[0].url"},
		{"clicked bad broadcast", cond(`{"field":"clicked","op":"any_link","broadcast_id":0}`), "rules[0].broadcast_id"},
		{"clicked bad within_days", cond(`{"field":"clicked","op":"no_click","within_days":0}`), "rules[0].within_days"},
		{"clicked with value", cond(`{"field":"clicked","op":"any_link","value":"x"}`), "rules[0].value"},
		{"clicked with days", cond(`{"field":"clicked","op":"any_link","days":3}`), "rules[0].days"},
		{"condition with match", cond(`{"field":"tag","op":"has","value":"a","match":"all"}`), "rules[0].match"},
		{"condition with rules", cond(`{"field":"tag","op":"has","value":"a","rules":[]}`), "rules[0].rules"},
		{"group without rules", cond(`{"match":"all"}`), "rules[0].rules"},
		{"group with empty rules", cond(`{"rules":[]}`), "rules[0].rules"},
		{"group bad match", cond(`{"match":"none","rules":[` + tag + `]}`), "rules[0].match"},
		{"group with op", cond(`{"op":"has","rules":[` + tag + `]}`), "rules[0].op"},
		{"group with value", cond(`{"value":"x","rules":[` + tag + `]}`), "rules[0].value"},
		{"nested path", cond(group(tag + `,` + group(`{"field":"tag","op":"has"}`))), "rules[0].rules[1].rules[0].value"},
		{"too deep", cond(group(group(group(tag)))), "rules[0].rules[0].rules[0]"},
		{"too many rules", cond(many(51, tag)), "rules"},
		{"too many rules in group", cond(group(many(51, tag))), "rules[0].rules"},
		{"too many conditions", `{"rules":[` + many(5, group(many(41, tag))) + `]}`, "rules[4].rules[36]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := segments.ParseFilter([]byte(tt.filter))
			if !isField(err, tt.field) {
				t.Fatalf("err = %v, want a validation error for %q", err, tt.field)
			}
		})
	}
}

func TestParseFilterSyntax(t *testing.T) {
	for _, raw := range []string{
		``,
		`[]`,
		`{"rules":[{"field":"tag","op":"has","value":"a","extra":1}]}`,
		`{"rules":[],"unknown":true}`,
		`{"rules":[]} {"rules":[]}`,
		`{"rules":"x"}`,
		`{"rules":[{"field":"tags","op":"has_any","values":[1]}]}`,
		`{"rules":[{"field":"created_at","op":"within_days","days":1.5}]}`,
		`{"rules":[` + strings.Repeat(" ", segments.MaxFilterBytes) + `]}`,
	} {
		name := raw
		if len(name) > 40 {
			name = name[:40]
		}
		t.Run(name, func(t *testing.T) {
			if _, err := segments.ParseFilter([]byte(raw)); !errors.Is(err, core.ErrValidation) {
				t.Fatalf("err = %v, want a validation error", err)
			}
		})
	}
}

func TestValidateNormalises(t *testing.T) {
	f := parse(t, `{"rules":[
		{"field":"tag","op":"has","value":"  VIP "},
		{"field":"tags","op":"has_any","values":["a","A"," b "]},
		{"field":"email","op":"ends_with","value":" @Example.COM "},
		{"field":"first_name","op":"contains","value":" an "},
		{"field":"created_at","op":"after","value":"2026-01-02T10:00:00+01:00"},
		{"field":"attr","key":"plan","op":"eq","value":" Pro "},
		{"field":"attr","key":"seats","op":"neq","value":5},
		{"field":"clicked","op":"link","url":" https://example.com/x ","within_days":3},
		{"rules":[{"field":"status","op":"is","value":"active"}]}]}`)
	got, err := segments.MarshalFilter(f)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"match":"all","rules":[` +
		`{"field":"tag","op":"has","value":"VIP"},` +
		`{"field":"tags","op":"has_any","values":["a","b"]},` +
		`{"field":"email","op":"ends_with","value":"@example.com"},` +
		`{"field":"first_name","op":"contains","value":"an"},` +
		`{"field":"created_at","op":"after","value":"2026-01-02T09:00:00Z"},` +
		`{"field":"attr","op":"eq","key":"plan","value":" Pro "},` +
		`{"field":"attr","op":"neq","key":"seats","value":5},` +
		`{"field":"clicked","op":"link","url":"https://example.com/x","within_days":3},` +
		`{"match":"all","rules":[{"field":"status","op":"is","value":"active"}]}]}`
	if got != want {
		t.Fatalf("canonical =\n%s\nwant\n%s", got, want)
	}
	// The canonical form parses to itself.
	again, err := segments.MarshalFilter(parse(t, got))
	if err != nil || again != got {
		t.Fatalf("round trip = %s, %v", again, err)
	}
	// An empty filter is stored with an empty rules array.
	if got, _ := segments.MarshalFilter(parse(t, `{}`)); got != `{"match":"all","rules":[]}` {
		t.Fatalf("empty = %s", got)
	}
}

func TestValidateGoValues(t *testing.T) {
	seven := 7
	for _, v := range []any{3, int64(3), int32(3), float32(3), 3.0} {
		f, err := segments.Validate(segments.SegmentFilter{Rules: []segments.SegmentRule{{Field: "attr", Key: "n", Op: "eq", Value: v}}})
		if err != nil || f.Rules[0].Value != 3.0 {
			t.Fatalf("Validate(%T) = %+v, %v", v, f, err)
		}
	}
	if _, err := segments.Validate(segments.SegmentFilter{Rules: []segments.SegmentRule{{Field: "attr", Key: "n", Op: "eq", Value: struct{}{}}}}); !isField(err, "rules[0].value") {
		t.Fatalf("struct value: %v", err)
	}
	f, err := segments.Validate(segments.SegmentFilter{Rules: []segments.SegmentRule{{Field: "created_at", Op: "within_days", Days: &seven}}})
	if err != nil || *f.Rules[0].Days != 7 || f.Rules[0].Days == &seven {
		t.Fatalf("Validate copies days: %+v %v", f, err)
	}
}

func TestCompileSQL(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	expr, args, err := segments.Compile(parse(t, `{"match":"any","rules":[
		{"field":"attr","key":"plan","op":"eq","value":"pro"},
		{"field":"created_at","op":"within_days","days":2},
		{"match":"all","rules":[{"field":"tags","op":"has_all","values":["a","b"]},{"field":"clicked","op":"no_click","broadcast_id":4,"within_days":1}]}]}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(expr, "plan") || strings.Contains(expr, "pro") {
		t.Fatalf("values in SQL: %s", expr)
	}
	if !strings.HasPrefix(expr, "(") || !strings.HasSuffix(expr, ")") || strings.Contains(expr, "status") {
		t.Fatalf("expr = %s", expr)
	}
	want := []any{`$."plan"`, `$."plan"`, "pro", "2026-02-27T12:00:00.000000Z", "a", "b", int64(4), "2026-02-28T12:00:00.000000Z"}
	if fmt.Sprint(args) != fmt.Sprint(want) {
		t.Fatalf("args = %#v\nwant %#v", args, want)
	}
	if strings.Count(expr, "?") != len(args) {
		t.Fatalf("placeholders %d != args %d: %s", strings.Count(expr, "?"), len(args), expr)
	}
	if expr, args, err := segments.Compile(segments.SegmentFilter{}, now); err != nil || expr != "(1)" || len(args) != 0 {
		t.Fatalf("empty = %q %v %v", expr, args, err)
	}
	if _, _, err := segments.Compile(parse(t, `{}`), now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := segments.Compile(segments.SegmentFilter{Match: "x"}, now); !isField(err, "match") {
		t.Fatalf("invalid filter: %v", err)
	}
}

// TestParseFilterMemberTypes checks that members of the wrong JSON type are
// reported with the indexed path of the offending input.
func TestParseFilterMemberTypes(t *testing.T) {
	const tag = `{"field":"tag","op":"has","value":"a"}`
	tests := []struct {
		name, filter, field string
	}{
		{"not an object", `[]`, ""},
		{"match type", `{"match":1,"rules":[]}`, "match"},
		{"rules type", `{"rules":"x"}`, "rules"},
		{"unknown top member", `{"rules":[],"unknown":true}`, "unknown"},
		{"rule not an object", `{"rules":[` + tag + `,1]}`, "rules[1]"},
		{"days string", `{"rules":[` + tag + `,{"field":"created_at","op":"within_days","days":"7"}]}`, "rules[1].days"},
		{"days fraction", `{"rules":[{"field":"created_at","op":"within_days","days":1.5}]}`, "rules[0].days"},
		{"days huge", `{"rules":[{"field":"created_at","op":"within_days","days":1e30}]}`, "rules[0].days"},
		{"within_days bool", `{"rules":[{"field":"clicked","op":"any_link","within_days":true}]}`, "rules[0].within_days"},
		{"broadcast_id string", `{"rules":[{"field":"clicked","op":"any_link","broadcast_id":"3"}]}`, "rules[0].broadcast_id"},
		{"values element", `{"rules":[{"field":"tags","op":"has_any","values":["a",1]}]}`, "rules[0].values[1]"},
		{"values not array", `{"rules":[{"field":"tags","op":"has_any","values":"a"}]}`, "rules[0].values"},
		{"field type", `{"rules":[{"field":5,"op":"has","value":"a"}]}`, "rules[0].field"},
		{"op type", `{"rules":[{"field":"tag","op":[],"value":"a"}]}`, "rules[0].op"},
		{"key type", `{"rules":[{"field":"attr","key":5,"op":"exists"}]}`, "rules[0].key"},
		{"url type", `{"rules":[{"field":"clicked","op":"link","url":{}}]}`, "rules[0].url"},
		{"value object", `{"rules":[{"field":"attr","key":"k","op":"eq","value":{"a":1}}]}`, "rules[0].value"},
		{"value array", `{"rules":[{"field":"tag","op":"has","value":["a"]}]}`, "rules[0].value"},
		{"value overflow", `{"rules":[{"field":"attr","key":"k","op":"eq","value":1e400}]}`, "rules[0].value"},
		{"unknown rule member", `{"rules":[{"field":"tag","op":"has","value":"a","extra":1}]}`, "rules[0].extra"},
		{"nested type", `{"rules":[` + tag + `,{"rules":[` + tag + `,{"rules":[{"field":"tag","op":"has","value":"a","days":"x"}]}]}]}`, "rules[1].rules[1].rules[0].days"},
		{"nested rules type", `{"rules":[{"match":"any","rules":{}}]}`, "rules[0].rules"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := segments.ParseFilter([]byte(tt.filter))
			var ve *core.ValidationError
			if !errors.As(err, &ve) || ve.Field != tt.field {
				t.Fatalf("err = %#v, want a validation error for field %q", err, tt.field)
			}
		})
	}
}

// TestParseFilterNulls checks that JSON null members count as absent and
// numbers keep their value.
func TestParseFilterNulls(t *testing.T) {
	f, err := segments.ParseFilter([]byte(`{"match":null,"rules":[
		{"field":"tag","op":"has","value":"a","days":null,"values":null,"url":null},
		{"field":"attr","key":"seats","op":"eq","value":5},
		{"field":"clicked","op":"any_link","broadcast_id":7,"within_days":30}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Match != segments.MatchAll || f.Rules[0].Days != nil || f.Rules[0].Values != nil {
		t.Fatalf("nulls not treated as absent: %+v", f)
	}
	if v, ok := f.Rules[1].Value.(float64); !ok || v != 5 {
		t.Fatalf("attr value = %#v, want float64 5", f.Rules[1].Value)
	}
	if r := f.Rules[2]; *r.BroadcastID != 7 || *r.WithinDays != 30 {
		t.Fatalf("clicked = %+v", r)
	}
	if _, err := segments.ParseFilter([]byte(`{"rules":[`)); !isField(err, "") {
		t.Fatalf("truncated JSON: %v", err)
	}
}
