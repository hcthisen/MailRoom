package segments

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/db"
)

// SQL fragments. The compiler assembles expressions only from these
// constants, the fixed column names in nameColumns and the connectives
// " AND ", " OR ", "NOT " and parentheses. Every value is a "?" argument.
const (
	sqlTrue = `1`

	// Tag lookups resolve the tag IDs once (uncorrelated subquery), then use
	// the contact_tags primary key. tags.name is COLLATE NOCASE.
	sqlTagHas = `EXISTS (SELECT 1 FROM contact_tags ct WHERE ct.contact_id = c.id AND ct.tag_id IN (SELECT t.id FROM tags t WHERE t.name = ?))`
	sqlTagAny = `EXISTS (SELECT 1 FROM contact_tags ct WHERE ct.contact_id = c.id AND ct.tag_id IN (SELECT t.id FROM tags t WHERE t.name IN (SELECT value FROM json_each(?))))`

	sqlStatusIs    = `c.status = ?`
	sqlStatusIsNot = `c.status <> ?`

	sqlEmailEq   = `c.email = ?`
	sqlEmailLike = `c.email LIKE ? ESCAPE '\'`

	sqlCreatedBefore = `c.created_at < ?`
	sqlCreatedAfter  = `c.created_at > ?`
	sqlCreatedSince  = `c.created_at >= ?`

	// Attribute fragments take the JSON path ($."key") as an argument.
	sqlAttrEqText   = `IFNULL(json_type(c.attributes, ?) = 'text' AND json_extract(c.attributes, ?) = ? COLLATE NOCASE, 0)`
	sqlAttrEqNumber = `IFNULL(json_type(c.attributes, ?) IN ('integer', 'real') AND json_extract(c.attributes, ?) = ?, 0)`
	sqlAttrEqBool   = `IFNULL(json_type(c.attributes, ?) = ?, 0)`
	sqlAttrContains = `IFNULL(json_type(c.attributes, ?) = 'text' AND json_extract(c.attributes, ?) LIKE ? ESCAPE '\', 0)`
	sqlAttrExists   = `IFNULL(json_type(c.attributes, ?) <> 'null', 0)`

	sqlClickOpen      = `EXISTS (SELECT 1 FROM clicks k WHERE k.contact_id = c.id`
	sqlClickLink      = ` AND k.link_id IN (SELECT l.id FROM links l WHERE l.url = ?)`
	sqlClickBroadcast = ` AND k.message_id IN (SELECT m.id FROM messages m WHERE m.broadcast_id = ?)`
	sqlClickSince     = ` AND k.created_at >= ?`
	sqlClickClose     = `)`
)

// nameColumns maps the name fields to their (fixed) column fragments.
var nameColumns = map[string]struct{ eq, like, empty, notEmpty string }{
	FieldFirstName: {`c.first_name = ? COLLATE NOCASE`, `c.first_name LIKE ? ESCAPE '\'`, `c.first_name = ''`, `c.first_name <> ''`},
	FieldLastName:  {`c.last_name = ? COLLATE NOCASE`, `c.last_name LIKE ? ESCAPE '\'`, `c.last_name = ''`, `c.last_name <> ''`},
}

// Compile validates f and compiles it to a parenthesised SQL boolean
// expression over the contacts alias "c", with its positional arguments.
// Relative conditions (within_days) are resolved against now. The
// expression never filters on status unless f has a status condition.
func Compile(f SegmentFilter, now time.Time) (string, []any, error) {
	vf, err := Validate(f)
	if err != nil {
		return "", nil, err
	}
	c := compiler{now: now.UTC()}
	expr, err := c.group(vf.Match, vf.Rules, true)
	if err != nil {
		return "", nil, err
	}
	return "(" + expr + ")", c.args, nil
}

type compiler struct {
	now  time.Time
	args []any
}

func (c *compiler) arg(vs ...any) { c.args = append(c.args, vs...) }

// group compiles rules joined by match. An empty top-level filter matches
// everyone; Validate rejects empty nested groups.
func (c *compiler) group(match string, rules []SegmentRule, top bool) (string, error) {
	if len(rules) == 0 {
		if top {
			return sqlTrue, nil
		}
		return "", fmt.Errorf("segments: empty group")
	}
	joiner := " AND "
	if match == MatchAny {
		joiner = " OR "
	}
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		var (
			p   string
			err error
		)
		if r.IsGroup() {
			p, err = c.group(r.Match, r.Rules, false)
		} else {
			p, err = c.condition(r)
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, "("+p+")")
	}
	return strings.Join(parts, joiner), nil
}

func (c *compiler) condition(r SegmentRule) (string, error) {
	switch r.Field {
	case FieldTag:
		c.arg(r.Value)
		if r.Op == OpNotHas {
			return "NOT " + sqlTagHas, nil
		}
		return sqlTagHas, nil
	case FieldTags:
		return c.tags(r)
	case FieldStatus:
		c.arg(r.Value)
		if r.Op == OpIsNot {
			return sqlStatusIsNot, nil
		}
		return sqlStatusIs, nil
	case FieldEmail:
		s, _ := r.Value.(string)
		switch r.Op {
		case OpEq:
			c.arg(s)
			return sqlEmailEq, nil
		case OpContains:
			c.arg("%" + escapeLike(s) + "%")
		default: // ends_with
			c.arg("%" + escapeLike(s))
		}
		return sqlEmailLike, nil
	case FieldFirstName, FieldLastName:
		cols := nameColumns[r.Field]
		s, _ := r.Value.(string)
		switch r.Op {
		case OpEq:
			c.arg(s)
			return cols.eq, nil
		case OpContains:
			c.arg("%" + escapeLike(s) + "%")
			return cols.like, nil
		case OpIsEmpty:
			return cols.empty, nil
		}
		return cols.notEmpty, nil
	case FieldCreatedAt:
		return c.createdAt(r)
	case FieldAttr:
		return c.attr(r)
	case FieldClicked:
		return c.clicked(r), nil
	}
	return "", fmt.Errorf("segments: unknown field %q", r.Field)
}

func (c *compiler) tags(r SegmentRule) (string, error) {
	if r.Op == OpHasAll {
		parts := make([]string, len(r.Values))
		for i, v := range r.Values {
			c.arg(v)
			parts[i] = sqlTagHas
		}
		return strings.Join(parts, " AND "), nil
	}
	list, err := json.Marshal(r.Values)
	if err != nil {
		return "", fmt.Errorf("segments: encode tags: %w", err)
	}
	c.arg(string(list))
	if r.Op == OpHasNone {
		return "NOT " + sqlTagAny, nil
	}
	return sqlTagAny, nil
}

func (c *compiler) createdAt(r SegmentRule) (string, error) {
	if r.Op == OpWithinDays {
		c.arg(db.FormatTime(c.since(*r.Days)))
		return sqlCreatedSince, nil
	}
	s, _ := r.Value.(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", fmt.Errorf("segments: created_at value %q: %w", s, err)
	}
	c.arg(db.FormatTime(t))
	if r.Op == OpBefore {
		return sqlCreatedBefore, nil
	}
	return sqlCreatedAfter, nil
}

func (c *compiler) since(days int) time.Time {
	return c.now.Add(-time.Duration(days) * 24 * time.Hour)
}

// attrPath returns the JSON path of a validated attribute key. It is passed
// as an argument, never spliced into the SQL.
func attrPath(key string) string { return `$."` + key + `"` }

func (c *compiler) attr(r SegmentRule) (string, error) {
	path := attrPath(r.Key)
	switch r.Op {
	case OpExists:
		c.arg(path)
		return sqlAttrExists, nil
	case OpNotExists:
		c.arg(path)
		return "NOT " + sqlAttrExists, nil
	case OpContains:
		s, _ := r.Value.(string)
		c.arg(path, path, "%"+escapeLike(s)+"%")
		return sqlAttrContains, nil
	}
	var eq string
	switch v := r.Value.(type) {
	case string:
		c.arg(path, path, v)
		eq = sqlAttrEqText
	case float64:
		c.arg(path, path, v)
		eq = sqlAttrEqNumber
	case bool:
		jt := "false"
		if v {
			jt = "true"
		}
		c.arg(path, jt)
		eq = sqlAttrEqBool
	default:
		return "", fmt.Errorf("segments: attr value of type %T", r.Value)
	}
	if r.Op == OpNeq {
		return "NOT " + eq, nil
	}
	return eq, nil
}

func (c *compiler) clicked(r SegmentRule) string {
	var b strings.Builder
	b.WriteString(sqlClickOpen)
	if r.URL != "" {
		b.WriteString(sqlClickLink)
		c.arg(r.URL)
	}
	if r.BroadcastID != nil {
		b.WriteString(sqlClickBroadcast)
		c.arg(*r.BroadcastID)
	}
	if r.WithinDays != nil {
		b.WriteString(sqlClickSince)
		c.arg(db.FormatTime(c.since(*r.WithinDays)))
	}
	b.WriteString(sqlClickClose)
	if r.Op == OpNoClick {
		return "NOT " + b.String()
	}
	return b.String()
}

// escapeLike escapes the LIKE wildcards % and _ and the escape character
// itself, for use with ESCAPE '\'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
