package ui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/segments"
)

// The segment builder edits a segments.SegmentFilter as a form. Every
// input is named by its path in the filter, the same path the segments
// service puts in its validation errors ("filter.rules[1].rules[0].value"),
// so an error lands next to its input. Structural edits (add a condition
// or group, remove a rule) post the whole form with an "edit" value and
// get the re-rendered builder back (htmx, or a full page without
// JavaScript); nothing is saved until the form is submitted.

// audNode is the filter itself or a nested group (Group), or a condition.
// Condition members are kept as the text of their inputs until the
// filter is built, so a half-typed value survives a re-render.
type audNode struct {
	Path  string // "filter", "filter.rules[0]", "filter.rules[0].rules[2]", ...
	Depth int    // groups: 1 for the filter, 2 and 3 for nested groups
	Group bool
	Match string
	Rules []*audNode

	Field, Op, Key, Value, ValueType, Values, Days, URL, BroadcastID, WithinDays string

	errs map[string]string
}

// Name is the form name of one of the node's members.
func (n *audNode) Name(member string) string { return n.Path + "." + member }

// ID is the element id of one of the node's inputs.
func (n *audNode) ID(member string) string {
	return "f-" + strings.NewReplacer("[", "-", "]", "", ".", "-").Replace(n.Path+"."+member)
}

// Err is the validation error of one of the node's members.
func (n *audNode) Err(member string) string { return n.errs[n.Path+"."+member] }

// RuleErr is a validation error of the node as a whole (e.g. a group
// without rules).
func (n *audNode) RuleErr() string {
	if m := n.errs[n.Path]; m != "" {
		return m
	}
	return n.errs[n.Path+".rules"]
}

// Number is the node's human number ("2.1": the first rule of the second
// rule's group).
func (n *audNode) Number() string { return audRuleNumber(n.Path) }

// CanNest reports whether a group may hold another group.
func (n *audNode) CanNest() bool { return n.Group && n.Depth < segments.MaxDepth }

// Ops are the operators of the condition's field.
func (n *audNode) Ops() []Option { return audOps[n.Field] }

// Shows reports whether the condition has input member for its field and
// operator.
func (n *audNode) Shows(member string) bool {
	f, op := n.Field, n.Op
	switch member {
	case "value":
		switch f {
		case segments.FieldTag, segments.FieldStatus, segments.FieldEmail:
			return true
		case segments.FieldFirstName, segments.FieldLastName:
			return op == segments.OpEq || op == segments.OpContains
		case segments.FieldCreatedAt:
			return op == segments.OpBefore || op == segments.OpAfter
		case segments.FieldAttr:
			return op == segments.OpEq || op == segments.OpNeq || op == segments.OpContains
		}
	case "value_type":
		return f == segments.FieldAttr && (op == segments.OpEq || op == segments.OpNeq)
	case "values":
		return f == segments.FieldTags
	case "key":
		return f == segments.FieldAttr
	case "days":
		return f == segments.FieldCreatedAt && op == segments.OpWithinDays
	case "url":
		return f == segments.FieldClicked && (op == segments.OpLink || op == segments.OpNoClick)
	case "broadcast_id", "within_days":
		return f == segments.FieldClicked
	}
	return false
}

var audRuleIndex = regexp.MustCompile(`rules\[(\d+)\]`)

// audRuleNumber turns "filter.rules[1].rules[0]" into "2.1".
func audRuleNumber(path string) string {
	var parts []string
	for _, m := range audRuleIndex.FindAllStringSubmatch(path, -1) {
		i, _ := strconv.Atoi(m[1])
		parts = append(parts, strconv.Itoa(i+1))
	}
	return strings.Join(parts, ".")
}

// audHumanError is a filter validation error for the preview panel:
// "Rule 2.1, value: is required".
func audHumanError(field, msg string) string {
	if field == "" || field == "filter" {
		return msg
	}
	rest := field
	if i := strings.LastIndex(field, "]"); i >= 0 {
		rest = strings.TrimPrefix(field[i+1:], ".")
	}
	num := audRuleNumber(field)
	switch {
	case num == "":
		return field + ": " + msg
	case rest == "":
		return "Rule " + num + ": " + msg
	}
	return "Rule " + num + ", " + strings.ReplaceAll(rest, "_", " ") + ": " + msg
}

// ---------------------------------------------------------------- choices

var audFields = []Option{
	{segments.FieldTag, "Tag"},
	{segments.FieldTags, "Tags (several)"},
	{segments.FieldStatus, "Status"},
	{segments.FieldEmail, "Email"},
	{segments.FieldFirstName, "First name"},
	{segments.FieldLastName, "Last name"},
	{segments.FieldCreatedAt, "Created"},
	{segments.FieldAttr, "Attribute"},
	{segments.FieldClicked, "Clicks"},
}

var audOps = map[string][]Option{
	segments.FieldTag:       {{segments.OpHas, "has"}, {segments.OpNotHas, "doesn't have"}},
	segments.FieldTags:      {{segments.OpHasAny, "has any of"}, {segments.OpHasAll, "has all of"}, {segments.OpHasNone, "has none of"}},
	segments.FieldStatus:    {{segments.OpIs, "is"}, {segments.OpIsNot, "is not"}},
	segments.FieldEmail:     {{segments.OpContains, "contains"}, {segments.OpEndsWith, "ends with"}, {segments.OpEq, "is"}},
	segments.FieldFirstName: {{segments.OpEq, "is"}, {segments.OpContains, "contains"}, {segments.OpIsEmpty, "is empty"}, {segments.OpNotEmpty, "is not empty"}},
	segments.FieldLastName:  {{segments.OpEq, "is"}, {segments.OpContains, "contains"}, {segments.OpIsEmpty, "is empty"}, {segments.OpNotEmpty, "is not empty"}},
	segments.FieldCreatedAt: {{segments.OpWithinDays, "in the last … days"}, {segments.OpBefore, "before"}, {segments.OpAfter, "after"}},
	segments.FieldAttr:      {{segments.OpEq, "equals"}, {segments.OpNeq, "doesn't equal"}, {segments.OpContains, "contains"}, {segments.OpExists, "is set"}, {segments.OpNotExists, "is not set"}},
	segments.FieldClicked:   {{segments.OpAnyLink, "clicked any link"}, {segments.OpLink, "clicked the link"}, {segments.OpNoClick, "didn't click"}},
}

var audValueTypes = []Option{{"text", "text"}, {"number", "number"}, {"boolean", "yes/no"}}

var audMatches = []Option{{segments.MatchAll, "all"}, {segments.MatchAny, "any"}}

func audHasOption(opts []Option, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

func audLabel(opts []Option, v string) string {
	for _, o := range opts {
		if o.Value == v {
			return o.Label
		}
	}
	return v
}

// normalize makes a condition's field, operator and value type valid
// choices (after the field changed, its old operator may not apply).
func (n *audNode) normalize() {
	if !audHasOption(audFields, n.Field) {
		n.Field = segments.FieldTag
	}
	if !audHasOption(audOps[n.Field], n.Op) {
		n.Op = audOps[n.Field][0].Value
	}
	if !audHasOption(audValueTypes, n.ValueType) {
		n.ValueType = "text"
	}
}

// ---------------------------------------------------------------- form <-> tree

// audParseBuilder reads the builder's inputs from f.
func audParseBuilder(f *Form) *audNode {
	root := &audNode{Path: "filter", Depth: 1, Group: true}
	audParseGroup(f, root)
	return root
}

func audParseGroup(f *Form, g *audNode) {
	g.Match = f.Get(g.Name("match"))
	if !audHasOption(audMatches, g.Match) {
		g.Match = segments.MatchAll
	}
	for i := 0; i < segments.MaxGroupRules; i++ {
		p := fmt.Sprintf("%s.rules[%d]", g.Path, i)
		kind := f.Get(p + ".kind")
		if kind == "" {
			break
		}
		n := &audNode{Path: p}
		if kind == "group" && g.Depth < segments.MaxDepth {
			n.Group, n.Depth = true, g.Depth+1
			audParseGroup(f, n)
		} else {
			get := func(m string) string { return f.Get(p + "." + m) }
			n.Field, n.Op, n.Key, n.Value, n.ValueType = get("field"), get("op"), get("key"), get("value"), get("value_type")
			n.Values, n.Days, n.URL, n.BroadcastID, n.WithinDays = get("values"), get("days"), get("url"), get("broadcast_id"), get("within_days")
			n.normalize()
		}
		g.Rules = append(g.Rules, n)
	}
}

// audNodeFromFilter builds the tree of a saved filter.
func (u *UI) audNodeFromFilter(sf segments.SegmentFilter) *audNode {
	root := &audNode{Path: "filter", Depth: 1, Group: true, Match: sf.Match}
	if root.Match == "" {
		root.Match = segments.MatchAll
	}
	root.Rules = u.audNodesFromRules(sf.Rules, root)
	return root
}

func (u *UI) audNodesFromRules(rules []segments.SegmentRule, g *audNode) []*audNode {
	var out []*audNode
	for i, r := range rules {
		n := &audNode{Path: fmt.Sprintf("%s.rules[%d]", g.Path, i)}
		if r.IsGroup() {
			n.Group, n.Depth, n.Match = true, g.Depth+1, r.Match
			if n.Match == "" {
				n.Match = segments.MatchAll
			}
			n.Rules = u.audNodesFromRules(r.Rules, n)
		} else {
			n.Field, n.Op, n.Key, n.URL = r.Field, r.Op, r.Key, r.URL
			n.Values = strings.Join(r.Values, ", ")
			if r.Days != nil {
				n.Days = strconv.Itoa(*r.Days)
			}
			if r.WithinDays != nil {
				n.WithinDays = strconv.Itoa(*r.WithinDays)
			}
			if r.BroadcastID != nil {
				n.BroadcastID = strconv.FormatInt(*r.BroadcastID, 10)
			}
			n.ValueType = "text"
			switch v := r.Value.(type) {
			case nil:
			case string:
				n.Value = v
				if r.Field == segments.FieldCreatedAt {
					if t, err := time.Parse(time.RFC3339, v); err == nil {
						n.Value = u.fmtInputTime(t)
					}
				}
			case bool:
				n.Value, n.ValueType = strconv.FormatBool(v), "boolean"
			case float64:
				n.Value, n.ValueType = strconv.FormatFloat(v, 'f', -1, 64), "number"
			case json.Number:
				n.Value, n.ValueType = v.String(), "number"
			default:
				n.Value = fmt.Sprint(v)
			}
			n.normalize()
		}
		out = append(out, n)
	}
	return out
}

// audBuildFilter turns the tree into the filter the service validates.
// Inputs that can't even be read (a number that isn't one) are validation
// errors on their input; everything else is left to the service.
func (u *UI) audBuildFilter(root *audNode) (segments.SegmentFilter, error) {
	rules, err := u.audBuildRules(root)
	if err != nil {
		return segments.SegmentFilter{}, err
	}
	return segments.SegmentFilter{Match: root.Match, Rules: rules}, nil
}

func (u *UI) audBuildRules(g *audNode) ([]segments.SegmentRule, error) {
	out := []segments.SegmentRule{}
	for _, n := range g.Rules {
		if n.Group {
			rules, err := u.audBuildRules(n)
			if err != nil {
				return nil, err
			}
			out = append(out, segments.SegmentRule{Match: n.Match, Rules: rules})
			continue
		}
		r, err := u.audBuildCondition(n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func audInt(n *audNode, member, s string, required bool) (*int, error) {
	if s == "" {
		if required {
			return nil, core.Invalid(n.Name(member), "is required")
		}
		return nil, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return nil, core.Invalid(n.Name(member), "must be a whole number")
	}
	return &v, nil
}

func (u *UI) audBuildCondition(n *audNode) (segments.SegmentRule, error) {
	r := segments.SegmentRule{Field: n.Field, Op: n.Op}
	var err error
	switch n.Field {
	case segments.FieldTags:
		r.Values = ParseTags(n.Values)
		if r.Values == nil {
			r.Values = []string{}
		}
	case segments.FieldCreatedAt:
		if n.Op == segments.OpWithinDays {
			r.Days, err = audInt(n, "days", n.Days, true)
			break
		}
		t, perr := u.ParseInputTime(n.Name("value"), n.Value)
		switch {
		case perr != nil:
			err = perr
		case t.IsZero():
			err = core.Invalid(n.Name("value"), "is required")
		default:
			r.Value = t.Format(time.RFC3339)
		}
	case segments.FieldAttr:
		r.Key = n.Key
		if !n.Shows("value") {
			break
		}
		r.Value = n.Value
		if n.Shows("value_type") {
			switch n.ValueType {
			case "number":
				f, perr := strconv.ParseFloat(n.Value, 64)
				if perr != nil {
					err = core.Invalid(n.Name("value"), "is not a number")
				}
				r.Value = f
			case "boolean":
				switch strings.ToLower(n.Value) {
				case "true", "yes":
					r.Value = true
				case "false", "no":
					r.Value = false
				default:
					err = core.Invalid(n.Name("value"), "must be true or false")
				}
			}
		}
	case segments.FieldClicked:
		if n.Shows("url") {
			r.URL = n.URL
		}
		if n.BroadcastID != "" {
			id, perr := strconv.ParseInt(n.BroadcastID, 10, 64)
			if perr != nil {
				return r, core.Invalid(n.Name("broadcast_id"), "must be a broadcast ID (a number)")
			}
			r.BroadcastID = &id
		}
		r.WithinDays, err = audInt(n, "within_days", n.WithinDays, false)
	default:
		if n.Shows("value") {
			r.Value = n.Value
		}
	}
	return r, err
}

// ---------------------------------------------------------------- edits

// audApplyEdit applies a structural edit ("add-condition:<group path>",
// "add-group:<group path>", "remove:<rule path>") and renumbers the tree.
// Unknown edits and paths change nothing.
func audApplyEdit(root *audNode, edit string) {
	op, path, _ := strings.Cut(edit, ":")
	switch op {
	case "add-condition", "add-group":
		g := audFind(root, path)
		if g == nil || !g.Group || len(g.Rules) >= segments.MaxGroupRules {
			break
		}
		cond := &audNode{Field: segments.FieldTag}
		cond.normalize()
		if op == "add-condition" {
			g.Rules = append(g.Rules, cond)
		} else if g.CanNest() {
			g.Rules = append(g.Rules, &audNode{Group: true, Match: segments.MatchAll, Rules: []*audNode{cond}})
		}
	case "remove":
		audRemove(root, path)
	}
	audRenumber(root)
}

func audFind(g *audNode, path string) *audNode {
	if g.Path == path {
		return g
	}
	for _, n := range g.Rules {
		if n.Path == path || strings.HasPrefix(path, n.Path+".") {
			return audFind(n, path)
		}
	}
	return nil
}

func audRemove(g *audNode, path string) bool {
	for i, n := range g.Rules {
		if n.Path == path {
			g.Rules = append(g.Rules[:i:i], g.Rules[i+1:]...)
			return true
		}
		if n.Group && strings.HasPrefix(path, n.Path+".") {
			return audRemove(n, path)
		}
	}
	return false
}

// audRenumber recomputes every path and depth from the tree's shape.
func audRenumber(g *audNode) {
	for i, n := range g.Rules {
		n.Path = fmt.Sprintf("%s.rules[%d]", g.Path, i)
		if n.Group {
			n.Depth = g.Depth + 1
			audRenumber(n)
		}
	}
}

// audSetErrors hands the form's errors to every node of the tree.
func audSetErrors(g *audNode, errs map[string]string) {
	g.errs = errs
	for _, n := range g.Rules {
		audSetErrors(n, errs)
	}
}

// ---------------------------------------------------------------- description

// audDescribe is a one-line reading of a filter ("all of: has tag
// customer, status is active").
func audDescribe(sf segments.SegmentFilter) string {
	if len(sf.Rules) == 0 {
		return "Every contact"
	}
	return audDescribeRules(sf.Match, sf.Rules)
}

func audDescribeRules(match string, rules []segments.SegmentRule) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		if r.IsGroup() {
			parts = append(parts, "("+audDescribeRules(r.Match, r.Rules)+")")
			continue
		}
		parts = append(parts, audDescribeCondition(r))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	joiner := " and "
	if match == segments.MatchAny {
		joiner = " or "
	}
	return strings.Join(parts, joiner)
}

func audDescribeCondition(r segments.SegmentRule) string {
	field := strings.ToLower(audLabel(audFields, r.Field))
	op := audLabel(audOps[r.Field], r.Op)
	val := ""
	switch v := r.Value.(type) {
	case nil:
	case string:
		val = v
		if r.Field == segments.FieldCreatedAt {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				val = t.Format("2 Jan 2006")
			}
		}
	default:
		val = fmt.Sprint(v)
	}
	switch r.Field {
	case segments.FieldTag:
		return op + " tag " + val
	case segments.FieldTags:
		return op + " tags " + strings.Join(r.Values, ", ")
	case segments.FieldCreatedAt:
		if r.Days != nil {
			return fmt.Sprintf("created in the last %d days", *r.Days)
		}
		return "created " + op + " " + val
	case segments.FieldAttr:
		return strings.TrimSpace(r.Key + " " + op + " " + val)
	case segments.FieldClicked:
		s := op
		if r.URL != "" {
			s += " " + r.URL
		}
		if r.BroadcastID != nil {
			s += fmt.Sprintf(" in broadcast %d", *r.BroadcastID)
		}
		if r.WithinDays != nil {
			s += fmt.Sprintf(" in the last %d days", *r.WithinDays)
		}
		return s
	}
	return strings.TrimSpace(field + " " + op + " " + val)
}
