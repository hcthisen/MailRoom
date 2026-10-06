package segments

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
)

// Validate checks f against the filter model and returns it normalised:
// match defaults to "all", text values are trimmed (attr eq/neq values are
// kept exactly), email values lowercased, tag lists de-duplicated
// case-insensitively, times converted to UTC and numbers to float64. A
// member that doesn't apply to a rule's field and operator is an error.
// Errors are *core.ValidationError with the path inside the filter as
// Field, e.g. "rules[2].value".
func Validate(f SegmentFilter) (SegmentFilter, error) {
	v := validator{}
	match, err := validMatch(f.Match, "match")
	if err != nil {
		return SegmentFilter{}, err
	}
	rules, err := v.rules(f.Rules, "", 1)
	if err != nil {
		return SegmentFilter{}, err
	}
	return SegmentFilter{Match: match, Rules: rules}, nil
}

// validator carries state across one Validate call.
type validator struct {
	conditions int
}

func validMatch(m, field string) (string, error) {
	switch m {
	case "":
		return MatchAll, nil
	case MatchAll, MatchAny:
		return m, nil
	}
	return "", core.Invalid(field, `must be "all" or "any"`)
}

func (v *validator) rules(in []SegmentRule, prefix string, depth int) ([]SegmentRule, error) {
	if len(in) > MaxGroupRules {
		return nil, core.Invalid(prefix+"rules", "has more than %d rules", MaxGroupRules)
	}
	out := make([]SegmentRule, 0, len(in))
	for i, r := range in {
		p := fmt.Sprintf("%srules[%d]", prefix, i)
		var (
			nr  SegmentRule
			err error
		)
		if r.IsGroup() {
			nr, err = v.group(r, p, depth+1)
		} else {
			nr, err = v.condition(r, p)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, nr)
	}
	return out, nil
}

func (v *validator) group(r SegmentRule, p string, depth int) (SegmentRule, error) {
	if depth > MaxDepth {
		return SegmentRule{}, core.Invalid(p, "groups nest at most %d levels deep", MaxDepth)
	}
	if err := onlyGroupMembers(r, p); err != nil {
		return SegmentRule{}, err
	}
	match, err := validMatch(r.Match, p+".match")
	if err != nil {
		return SegmentRule{}, err
	}
	if len(r.Rules) == 0 {
		return SegmentRule{}, core.Invalid(p+".rules", "a group needs at least one rule")
	}
	rules, err := v.rules(r.Rules, p+".", depth)
	if err != nil {
		return SegmentRule{}, err
	}
	return SegmentRule{Match: match, Rules: rules}, nil
}

// onlyGroupMembers rejects condition members on a group.
func onlyGroupMembers(r SegmentRule, p string) error {
	switch {
	case r.Op != "":
		return core.Invalid(p+".op", "a group has no op; add field to make it a condition")
	case r.Key != "":
		return core.Invalid(p+".key", "is not allowed on a group")
	case r.Value != nil:
		return core.Invalid(p+".value", "is not allowed on a group")
	case r.Values != nil:
		return core.Invalid(p+".values", "is not allowed on a group")
	case r.Days != nil:
		return core.Invalid(p+".days", "is not allowed on a group")
	case r.URL != "":
		return core.Invalid(p+".url", "is not allowed on a group")
	case r.BroadcastID != nil:
		return core.Invalid(p+".broadcast_id", "is not allowed on a group")
	case r.WithinDays != nil:
		return core.Invalid(p+".within_days", "is not allowed on a group")
	}
	return nil
}

// member names of a condition, used to reject irrelevant ones.
const (
	mKey         = "key"
	mValue       = "value"
	mValues      = "values"
	mDays        = "days"
	mURL         = "url"
	mBroadcastID = "broadcast_id"
	mWithinDays  = "within_days"
)

// fieldOps lists the operators each field accepts.
var fieldOps = map[string][]string{
	FieldTag:       {OpHas, OpNotHas},
	FieldTags:      {OpHasAll, OpHasAny, OpHasNone},
	FieldStatus:    {OpIs, OpIsNot},
	FieldEmail:     {OpEq, OpContains, OpEndsWith},
	FieldFirstName: {OpEq, OpContains, OpIsEmpty, OpNotEmpty},
	FieldLastName:  {OpEq, OpContains, OpIsEmpty, OpNotEmpty},
	FieldCreatedAt: {OpBefore, OpAfter, OpWithinDays},
	FieldAttr:      {OpEq, OpNeq, OpContains, OpExists, OpNotExists},
	FieldClicked:   {OpAnyLink, OpLink, OpNoClick},
}

// fieldOrder lists the fields for error messages.
var fieldOrder = []string{FieldTag, FieldTags, FieldStatus, FieldEmail, FieldFirstName, FieldLastName, FieldCreatedAt, FieldAttr, FieldClicked}

func (v *validator) condition(r SegmentRule, p string) (SegmentRule, error) {
	if r.Field == "" {
		return SegmentRule{}, core.Invalid(p+".field", "is required: a rule is a condition with a field, or a group with rules")
	}
	ops, ok := fieldOps[r.Field]
	if !ok {
		return SegmentRule{}, core.Invalid(p+".field", "must be one of %s", strings.Join(fieldOrder, ", "))
	}
	if !contains(ops, r.Op) {
		return SegmentRule{}, core.Invalid(p+".op", "for field %s must be one of %s", r.Field, strings.Join(ops, ", "))
	}
	if r.Match != "" {
		return SegmentRule{}, core.Invalid(p+".match", "is only allowed on a group (a rule without field)")
	}
	if r.Rules != nil {
		return SegmentRule{}, core.Invalid(p+".rules", "is only allowed on a group (a rule without field)")
	}
	v.conditions++
	if v.conditions > MaxConditions {
		return SegmentRule{}, core.Invalid(p, "a filter has at most %d conditions", MaxConditions)
	}
	out := SegmentRule{Field: r.Field, Op: r.Op}
	var (
		allowed []string
		err     error
	)
	switch r.Field {
	case FieldTag:
		allowed = []string{mValue}
		out.Value, err = tagValue(r.Value, p+".value")
	case FieldTags:
		allowed = []string{mValues}
		out.Values, err = tagValues(r.Values, p+".values")
	case FieldStatus:
		allowed = []string{mValue}
		out.Value, err = statusValue(r.Value, p+".value")
	case FieldEmail:
		allowed = []string{mValue}
		var s string
		s, err = textValue(r.Value, p+".value")
		out.Value = strings.ToLower(s)
	case FieldFirstName, FieldLastName:
		if r.Op == OpEq || r.Op == OpContains {
			allowed = []string{mValue}
			out.Value, err = textValue(r.Value, p+".value")
		}
	case FieldCreatedAt:
		if r.Op == OpWithinDays {
			allowed = []string{mDays}
			out.Days, err = days(r.Days, p+".days", true)
		} else {
			allowed = []string{mValue}
			out.Value, err = timeValue(r.Value, p+".value")
		}
	case FieldAttr:
		out.Key, out.Value, allowed, err = attrMembers(r, p)
	case FieldClicked:
		out, allowed, err = clickMembers(r, p)
	}
	if err != nil {
		return SegmentRule{}, err
	}
	if err := onlyMembers(r, p, allowed); err != nil {
		return SegmentRule{}, err
	}
	return out, nil
}

// onlyMembers rejects condition members that are set but not in allowed.
func onlyMembers(r SegmentRule, p string, allowed []string) error {
	set := map[string]bool{
		mKey:         r.Key != "",
		mValue:       r.Value != nil,
		mValues:      r.Values != nil,
		mDays:        r.Days != nil,
		mURL:         r.URL != "",
		mBroadcastID: r.BroadcastID != nil,
		mWithinDays:  r.WithinDays != nil,
	}
	for _, m := range []string{mKey, mValue, mValues, mDays, mURL, mBroadcastID, mWithinDays} {
		if set[m] && !contains(allowed, m) {
			return core.Invalid(p+"."+m, "is not allowed for field %s with op %s", r.Field, r.Op)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// textValue returns a required, trimmed text value of at most
// MaxValueLength characters without control characters.
func textValue(v any, field string) (string, error) {
	if v == nil {
		return "", core.Invalid(field, "is required")
	}
	s, ok := v.(string)
	if !ok {
		return "", core.Invalid(field, "must be a string")
	}
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", core.Invalid(field, "must not be empty")
	case !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0:
		return "", core.Invalid(field, "contains invalid characters")
	case utf8.RuneCountInString(s) > MaxValueLength:
		return "", core.Invalid(field, "is longer than %d characters", MaxValueLength)
	}
	return s, nil
}

func tagValue(v any, field string) (string, error) {
	if v == nil {
		return "", core.Invalid(field, "is required")
	}
	s, ok := v.(string)
	if !ok {
		return "", core.Invalid(field, "must be a tag name (string)")
	}
	n, err := contactstore.NormalizeTag(s)
	if err != nil {
		return "", relabel(err, field)
	}
	return n, nil
}

func tagValues(vs []string, field string) ([]string, error) {
	switch {
	case len(vs) == 0:
		return nil, core.Invalid(field, "needs at least one tag name")
	case len(vs) > MaxValues:
		return nil, core.Invalid(field, "has more than %d tag names", MaxValues)
	}
	for i, s := range vs {
		if _, err := contactstore.NormalizeTag(s); err != nil {
			return nil, relabel(err, fmt.Sprintf("%s[%d]", field, i))
		}
	}
	out, err := contactstore.NormalizeTags(vs)
	if err != nil {
		return nil, relabel(err, field)
	}
	return out, nil
}

// relabel moves a validation error to another field, keeping its message.
func relabel(err error, field string) error {
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		return &core.ValidationError{Field: field, Message: ve.Message}
	}
	return err
}

func statusValue(v any, field string) (string, error) {
	s, ok := v.(string)
	if v == nil || !ok || !core.ContactStatus(s).Valid() {
		names := make([]string, len(core.ContactStatuses))
		for i, st := range core.ContactStatuses {
			names[i] = string(st)
		}
		return "", core.Invalid(field, "must be one of %s", strings.Join(names, ", "))
	}
	return s, nil
}

func timeValue(v any, field string) (string, error) {
	s, ok := v.(string)
	if v == nil || !ok {
		return "", core.Invalid(field, "must be an RFC 3339 time, e.g. 2026-01-31T00:00:00Z")
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return "", core.Invalid(field, "must be an RFC 3339 time, e.g. 2026-01-31T00:00:00Z")
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

// days validates an optional or required day count.
func days(d *int, field string, required bool) (*int, error) {
	if d == nil {
		if required {
			return nil, core.Invalid(field, "is required")
		}
		return nil, nil
	}
	if *d < 1 || *d > MaxDays {
		return nil, core.Invalid(field, "must be between 1 and %d", MaxDays)
	}
	n := *d
	return &n, nil
}

// validAttrKey reports whether k is 1-64 of [A-Za-z0-9_], the attribute key
// rule of contactstore.ValidateAttributes. Only such keys ever reach SQL.
func validAttrKey(k string) bool {
	if k == "" || len(k) > contactstore.MaxAttributeKey {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func attrMembers(r SegmentRule, p string) (key string, value any, allowed []string, err error) {
	if r.Key == "" {
		return "", nil, nil, core.Invalid(p+".key", "is required")
	}
	if !validAttrKey(r.Key) {
		return "", nil, nil, core.Invalid(p+".key", "must be 1-%d letters, digits or underscores", contactstore.MaxAttributeKey)
	}
	switch r.Op {
	case OpExists, OpNotExists:
		return r.Key, nil, []string{mKey}, nil
	case OpContains:
		s, err := textValue(r.Value, p+".value")
		return r.Key, s, []string{mKey, mValue}, err
	}
	value, err = scalarValue(r.Value, p+".value")
	return r.Key, value, []string{mKey, mValue}, err
}

// scalarValue accepts a string (kept as is, but at most MaxValueLength
// characters), a finite number (as float64) or a boolean.
func scalarValue(v any, field string) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, core.Invalid(field, "is required")
	case string:
		if !utf8.ValidString(x) || utf8.RuneCountInString(x) > MaxValueLength {
			return nil, core.Invalid(field, "must be valid text of at most %d characters", MaxValueLength)
		}
		return x, nil
	case bool:
		return x, nil
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return nil, core.Invalid(field, "is not a valid number")
		}
		return finite(f, field)
	case float64:
		return finite(x, field)
	case float32:
		return finite(float64(x), field)
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case int32:
		return float64(x), nil
	}
	return nil, core.Invalid(field, "must be a string, number or boolean")
}

func finite(f float64, field string) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, core.Invalid(field, "must be a finite number")
	}
	return f, nil
}

func clickMembers(r SegmentRule, p string) (SegmentRule, []string, error) {
	out := SegmentRule{Field: r.Field, Op: r.Op}
	allowed := []string{mBroadcastID, mWithinDays}
	switch r.Op {
	case OpLink:
		allowed = append(allowed, mURL)
		if r.URL == "" {
			return out, nil, core.Invalid(p+".url", "is required for op link")
		}
	case OpNoClick:
		allowed = append(allowed, mURL)
	}
	if r.URL != "" && contains(allowed, mURL) {
		u := strings.TrimSpace(r.URL)
		switch {
		case u == "":
			return out, nil, core.Invalid(p+".url", "must not be empty")
		case len(u) > MaxURLLength:
			return out, nil, core.Invalid(p+".url", "is longer than %d characters", MaxURLLength)
		case !utf8.ValidString(u) || strings.IndexFunc(u, unicode.IsControl) >= 0:
			return out, nil, core.Invalid(p+".url", "contains invalid characters")
		}
		out.URL = u
	}
	if r.BroadcastID != nil {
		if *r.BroadcastID < 1 {
			return out, nil, core.Invalid(p+".broadcast_id", "must be a broadcast ID (1 or more)")
		}
		id := *r.BroadcastID
		out.BroadcastID = &id
	}
	var err error
	if out.WithinDays, err = days(r.WithinDays, p+".within_days", false); err != nil {
		return out, nil, err
	}
	return out, allowed, nil
}
