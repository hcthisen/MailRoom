package segments

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"mailroom/internal/audit"
	"mailroom/internal/contactstore"
	"mailroom/internal/db"
)

// Segments refer to tags by name (see the package documentation). The
// functions here keep those references in step with tag renames and merges
// and report references that no longer resolve.

// TagNames returns the tag names f refers to in its tag and tags
// conditions, trimmed and de-duplicated ignoring ASCII case (as the tags
// table's NOCASE collation does), in order of first appearance.
func TagNames(f SegmentFilter) []string {
	var (
		out  []string
		seen = map[string]bool{}
	)
	add := func(name string) {
		n := strings.TrimSpace(name)
		if k := foldASCII(n); n != "" && !seen[k] {
			seen[k] = true
			out = append(out, n)
		}
	}
	var walk func([]SegmentRule)
	walk = func(rules []SegmentRule) {
		for _, r := range rules {
			switch {
			case r.IsGroup():
				walk(r.Rules)
			case r.Field == FieldTag:
				if s, ok := r.Value.(string); ok {
					add(s)
				}
			case r.Field == FieldTags:
				for _, s := range r.Values {
					add(s)
				}
			}
		}
	}
	walk(f.Rules)
	if out == nil {
		out = []string{}
	}
	return out
}

// ReplaceTagName rewrites every saved segment whose filter refers to the
// tag name from (ignoring ASCII case) to refer to to instead, inside the
// caller's transaction. The tags package calls it when a tag is renamed
// (old name -> new name) or merged (source name -> target name), so saved
// segments keep selecting the same contacts; a has_none or not_has rule
// would otherwise silently start matching everyone. Each changed segment
// gets a new version and a "segment.update" audit record. It returns the
// number of segments changed.
func (s *Service) ReplaceTagName(ctx context.Context, tx *sql.Tx, from, to string) (int, error) {
	from = strings.TrimSpace(from)
	to, err := contactstore.NormalizeTag(to)
	if err != nil {
		return 0, relabel(err, "name")
	}
	if from == "" || from == to {
		return 0, nil
	}
	list, err := referringSegments(ctx, tx, from)
	if err != nil {
		return 0, err
	}
	now := s.clock.Now()
	for _, cur := range list {
		next := SegmentFilter{Match: cur.Filter.Match, Rules: replaceTag(cur.Filter.Rules, from, to)}
		// A merge can turn ["a","b"] into ["b","b"]; Validate de-duplicates.
		// A stored filter that doesn't validate any more is rewritten as is.
		if vf, err := Validate(next); err == nil {
			next = vf
		}
		raw, err := MarshalFilter(next)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE segments SET filter = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			raw, db.FormatTime(now), cur.ID); err != nil {
			return 0, fmt.Errorf("update segment %d: %w", cur.ID, err)
		}
		if err := s.audit(ctx, tx, "segment.update", cur.ID, map[string]any{
			"filter": audit.Change{From: cur.Filter, To: next},
			"tag":    audit.Change{From: from, To: to},
		}); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

// SegmentsUsingTag returns the IDs of the saved segments whose filter
// refers to the tag name (ignoring ASCII case), in ID order. The tags
// package can use it to warn about, or refuse, deleting a tag that
// segments still select on.
func (s *Service) SegmentsUsingTag(ctx context.Context, tx *sql.Tx, name string) ([]int64, error) {
	list, err := referringSegments(ctx, tx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(list))
	for i, sg := range list {
		ids[i] = sg.ID
	}
	return ids, nil
}

// referringSegments returns the segments whose filter refers to tag name.
// Segments are few, so they are decoded and inspected in Go rather than
// matched in SQL, which would have to reproduce JSON string escaping.
func referringSegments(ctx context.Context, q db.Querier, name string) ([]Segment, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+segmentColumns+` FROM segments ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		sg, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		for _, n := range TagNames(sg.Filter) {
			if sameTag(n, name) {
				out = append(out, sg)
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}
	return out, nil
}

// replaceTag returns a copy of rules with every tag reference to from
// replaced by to.
func replaceTag(rules []SegmentRule, from, to string) []SegmentRule {
	if rules == nil {
		return nil
	}
	out := make([]SegmentRule, len(rules))
	for i, r := range rules {
		switch {
		case r.IsGroup():
			r.Rules = replaceTag(r.Rules, from, to)
		case r.Field == FieldTag:
			if s, ok := r.Value.(string); ok && sameTag(s, from) {
				r.Value = to
			}
		case r.Field == FieldTags:
			vs := make([]string, len(r.Values))
			for j, v := range r.Values {
				if sameTag(v, from) {
					v = to
				}
				vs[j] = v
			}
			r.Values = vs
		}
		out[i] = r
	}
	return out
}

// missingTags returns the names among names that no tag has, in order.
func missingTags(ctx context.Context, q db.Querier, names []string) ([]string, error) {
	out := []string{}
	if len(names) == 0 {
		return out, nil
	}
	list, err := json.Marshal(names)
	if err != nil {
		return nil, fmt.Errorf("encode tag names: %w", err)
	}
	// t.name is COLLATE NOCASE, so the comparison ignores ASCII case.
	rows, err := q.QueryContext(ctx, `
		SELECT je.value FROM json_each(?) je
		WHERE NOT EXISTS (SELECT 1 FROM tags t WHERE t.name = je.value)
		ORDER BY je.key`, string(list))
	if err != nil {
		return nil, fmt.Errorf("find missing tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan missing tag: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find missing tags: %w", err)
	}
	return out, nil
}

// sameTag reports whether two tag names are equal after trimming, ignoring
// ASCII case like SQLite's NOCASE collation.
func sameTag(a, b string) bool {
	return foldASCII(strings.TrimSpace(a)) == foldASCII(strings.TrimSpace(b))
}

// foldASCII lowercases ASCII letters only, like SQLite's NOCASE collation.
func foldASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
