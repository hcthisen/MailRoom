// Package segments implements saved contact segments (PRD T-2, T-3): the
// filter model, its compiler to SQL, live counts and member previews, and
// the /api/v1/segments operations. Segments are evaluated whenever they are
// used and never stored as membership. *Service implements
// core.SegmentResolver.
//
// # Filter model
//
// A filter is a JSON object with a match mode and a list of rules. Each rule
// is either a condition (it has a "field") or a nested group (it has
// "rules" and an optional "match"):
//
//	{"match": "all" | "any", "rules": [ <condition> | <group>, ... ]}
//
// "match" defaults to "all". A filter without rules matches every contact;
// a nested group must have at least one rule. Groups nest up to depth 3: the
// filter itself is depth 1, a group inside it depth 2, a group inside that
// depth 3. A filter holds at most MaxConditions conditions in total and a
// group at most MaxGroupRules rules.
//
// Conditions:
//
//	{"field": "tag",  "op": "has" | "not_has", "value": "customer"}
//	{"field": "tags", "op": "has_all" | "has_any" | "has_none", "values": ["a", "b"]}
//	{"field": "status", "op": "is" | "is_not", "value": "active"}
//	{"field": "email", "op": "eq" | "contains" | "ends_with", "value": "@example.com"}
//	{"field": "first_name" | "last_name", "op": "eq" | "contains", "value": "Ann"}
//	{"field": "first_name" | "last_name", "op": "is_empty" | "not_empty"}
//	{"field": "created_at", "op": "before" | "after", "value": "2026-01-01T00:00:00Z"}
//	{"field": "created_at", "op": "within_days", "days": 30}
//	{"field": "attr", "key": "plan", "op": "eq" | "neq", "value": "pro" | 3 | true}
//	{"field": "attr", "key": "plan", "op": "contains", "value": "pro"}
//	{"field": "attr", "key": "plan", "op": "exists" | "not_exists"}
//	{"field": "clicked", "op": "any_link" | "link" | "no_click",
//	 "url": "https://example.com/offer", "broadcast_id": 12, "within_days": 30}
//
// Semantics:
//
//   - Tag names and text comparisons (eq, contains, ends_with) ignore ASCII
//     case, like the tags table's NOCASE collation. "contains" and
//     "ends_with" match the value literally: % and _ are not wildcards.
//   - "tags" with has_all needs every listed tag, has_any at least one, and
//     has_none none of them. A tag that doesn't exist yet is simply not held.
//   - Saved segments refer to tags by name, not ID. When a tag is renamed or
//     merged, the tags package must call Service.ReplaceTagName in the same
//     transaction so saved segments follow it; otherwise a has rule on the
//     old name matches nobody and a not_has/has_none rule matches everybody
//     (a broadcast could reach contacts the segment meant to exclude). A
//     deleted tag is held by nobody, with the same effect on negations;
//     Service.SegmentsUsingTag finds the segments that still refer to one.
//     Counts and previews list every referenced name that no tag has in
//     missing_tags, so a stale reference is visible before sending.
//   - "status" is an explicit condition only: no filter ever adds a status
//     condition by itself. Callers that send add c.status = 'active'.
//   - "created_at" values are RFC 3339 times; before and after are strict.
//     within_days means created in the last N days (1-3650), counted from
//     the moment the filter is evaluated.
//   - "attr" compares contacts.attributes[key]; key is 1-64 letters, digits
//     or underscores. The value's JSON type must match the stored type: the
//     string "5" doesn't equal the number 5, and booleans only match JSON
//     true/false. neq and not_exists also match contacts without the key.
//     exists treats a JSON null like a missing key. contains needs a string.
//   - "clicked" uses per-contact click tracking (K-1). any_link: clicked any
//     tracked link; link: clicked the link with exactly this url; no_click:
//     no click (on url, when given). broadcast_id limits the clicks to that
//     broadcast's messages and within_days (1-3650) to the last N days.
//
// Validation errors are *core.ValidationError values whose Field is the
// path of the offending input inside the filter, e.g. "rules[2].value" or
// "rules[0].rules[1].op"; the API prefixes it with "filter.".
//
// # SQL
//
// Compile turns a validated filter into a boolean SQL expression over the
// contacts alias "c" plus positional arguments. The expression is built only
// from a closed set of constant fragments; every user-supplied value,
// including attribute paths, is passed as an argument. Every fragment
// evaluates to 0 or 1, never NULL.
package segments
