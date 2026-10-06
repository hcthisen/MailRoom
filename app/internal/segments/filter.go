package segments

import (
	"encoding/json"
	"fmt"
)

// Match modes of a filter or group.
const (
	MatchAll = "all"
	MatchAny = "any"
)

// Condition fields.
const (
	FieldTag       = "tag"
	FieldTags      = "tags"
	FieldStatus    = "status"
	FieldEmail     = "email"
	FieldFirstName = "first_name"
	FieldLastName  = "last_name"
	FieldCreatedAt = "created_at"
	FieldAttr      = "attr"
	FieldClicked   = "clicked"
)

// Operators.
const (
	OpHas        = "has"
	OpNotHas     = "not_has"
	OpHasAll     = "has_all"
	OpHasAny     = "has_any"
	OpHasNone    = "has_none"
	OpIs         = "is"
	OpIsNot      = "is_not"
	OpEq         = "eq"
	OpNeq        = "neq"
	OpContains   = "contains"
	OpEndsWith   = "ends_with"
	OpIsEmpty    = "is_empty"
	OpNotEmpty   = "not_empty"
	OpBefore     = "before"
	OpAfter      = "after"
	OpWithinDays = "within_days"
	OpExists     = "exists"
	OpNotExists  = "not_exists"
	OpAnyLink    = "any_link"
	OpLink       = "link"
	OpNoClick    = "no_click"
)

// Limits of the filter model.
const (
	MaxDepth       = 3    // the filter is depth 1; groups nest down to depth MaxDepth
	MaxGroupRules  = 50   // rules in one filter or group
	MaxConditions  = 200  // conditions in a whole filter
	MaxValues      = 100  // values of a "tags" condition
	MaxValueLength = 256  // characters of a text value
	MaxURLLength   = 2048 // characters of a clicked url
	MaxDays        = 3650 // days of within_days
	MaxFilterBytes = 64 << 10
)

// SegmentFilter is a segment's filter: a match mode and its rules. See the
// package documentation for the model.
type SegmentFilter struct {
	Match string        `json:"match,omitempty" enum:"all,any" doc:"all: every rule must match (default); any: at least one" example:"all"`
	Rules []SegmentRule `json:"rules" maxItems:"50" doc:"Conditions and nested groups. No rules matches every contact."`
}

// SegmentRule is one entry of a filter: a condition (Field set) or a nested
// group (Rules set, Field empty). Only the members relevant to the field and
// operator may be set.
type SegmentRule struct {
	Match string        `json:"match,omitempty" enum:"all,any" doc:"Group only: all (default) or any"`
	Rules []SegmentRule `json:"rules,omitempty" maxItems:"50" doc:"Group only: the group's conditions and nested groups (at least one; groups nest up to depth 3)"`

	Field       string   `json:"field,omitempty" enum:"tag,tags,status,email,first_name,last_name,created_at,attr,clicked" doc:"Condition only: what the condition tests" example:"tag"`
	Op          string   `json:"op,omitempty" enum:"has,not_has,has_all,has_any,has_none,is,is_not,eq,neq,contains,ends_with,is_empty,not_empty,before,after,within_days,exists,not_exists,any_link,link,no_click" doc:"Condition operator; which ones are allowed depends on field" example:"has"`
	Key         string   `json:"key,omitempty" maxLength:"64" doc:"attr only: the attribute key (letters, digits, underscores)" example:"plan"`
	Value       any      `json:"value,omitempty" doc:"The value to compare with: a tag name, status, text, RFC 3339 time, or (attr) a string, number or boolean" example:"\"customer\""`
	Values      []string `json:"values,omitempty" maxItems:"100" doc:"tags only: the tag names" example:"[\"customer\",\"vip\"]"`
	Days        *int     `json:"days,omitempty" nullable:"false" minimum:"1" maximum:"3650" doc:"created_at within_days only: number of days" example:"30"`
	URL         string   `json:"url,omitempty" maxLength:"2048" doc:"clicked only: the exact link URL (required for link, optional for no_click)" example:"https://example.com/offer"`
	BroadcastID *int64   `json:"broadcast_id,omitempty" nullable:"false" minimum:"1" doc:"clicked only: count only clicks in this broadcast" example:"12"`
	WithinDays  *int     `json:"within_days,omitempty" nullable:"false" minimum:"1" maximum:"3650" doc:"clicked only: count only clicks in the last N days" example:"30"`
}

// IsGroup reports whether r is a nested group rather than a condition.
func (r SegmentRule) IsGroup() bool { return r.Field == "" && (r.Rules != nil || r.Match != "") }

// MarshalFilter returns the canonical JSON of a validated filter, as stored
// in segments.filter.
func MarshalFilter(f SegmentFilter) (string, error) {
	if f.Rules == nil {
		f.Rules = []SegmentRule{}
	}
	b, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("segments: encode filter: %w", err)
	}
	return string(b), nil
}
