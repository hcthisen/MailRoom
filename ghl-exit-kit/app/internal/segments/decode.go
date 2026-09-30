package segments

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"mailroom/internal/core"
)

// ParseFilter decodes and validates a filter from JSON. Unknown members are
// rejected. Every error, including a member of the wrong JSON type, is a
// *core.ValidationError whose Field is the path of the offending input
// (e.g. "rules[1].days"). The result is normalised (see Validate).
func ParseFilter(raw []byte) (SegmentFilter, error) {
	if len(raw) > MaxFilterBytes {
		return SegmentFilter{}, core.Invalid("", "filter is larger than %d bytes", MaxFilterBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return SegmentFilter{}, core.Invalid("", "filter is not valid JSON: %s", syntaxMessage(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SegmentFilter{}, core.Invalid("", "filter has trailing data after the JSON object")
	}
	f, err := decodeFilter(v)
	if err != nil {
		return SegmentFilter{}, err
	}
	return Validate(f)
}

// syntaxMessage shortens encoding/json syntax errors for users.
func syntaxMessage(err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("%s (at byte %d)", se.Error(), se.Offset)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected end of input"
	}
	return err.Error()
}

// The members a filter object and a rule object may have.
var (
	filterMembers = map[string]bool{"match": true, "rules": true}
	ruleMembers   = map[string]bool{
		"match": true, "rules": true, "field": true, "op": true, "key": true, "value": true,
		mValues: true, mDays: true, mURL: true, mBroadcastID: true, mWithinDays: true,
	}
)

// decodeFilter builds a SegmentFilter from a generic JSON value (decoded
// with UseNumber), checking member names and JSON types with indexed paths.
// A JSON null member counts as absent, as with encoding/json.
func decodeFilter(v any) (SegmentFilter, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return SegmentFilter{}, core.Invalid("", "filter must be a JSON object")
	}
	if err := knownMembers(obj, filterMembers, ""); err != nil {
		return SegmentFilter{}, err
	}
	var (
		f   SegmentFilter
		err error
	)
	if f.Match, err = stringMember(obj, "match", ""); err != nil {
		return SegmentFilter{}, err
	}
	if f.Rules, err = rulesMember(obj, ""); err != nil {
		return SegmentFilter{}, err
	}
	return f, nil
}

// decodeRule builds one rule; p is its path, e.g. "rules[2]".
func decodeRule(v any, p string) (SegmentRule, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return SegmentRule{}, core.Invalid(p, "must be a JSON object (a condition or a group)")
	}
	if err := knownMembers(obj, ruleMembers, p+"."); err != nil {
		return SegmentRule{}, err
	}
	var (
		r   SegmentRule
		err error
	)
	for _, m := range []struct {
		name string
		dst  *string
	}{{"match", &r.Match}, {"field", &r.Field}, {"op", &r.Op}, {"key", &r.Key}, {mURL, &r.URL}} {
		if *m.dst, err = stringMember(obj, m.name, p+"."); err != nil {
			return SegmentRule{}, err
		}
	}
	if r.Rules, err = rulesMember(obj, p+"."); err != nil {
		return SegmentRule{}, err
	}
	if r.Value = obj["value"]; r.Value != nil {
		if _, isObj := r.Value.(map[string]any); isObj {
			return SegmentRule{}, core.Invalid(p+".value", "must be a string, number or boolean, not an object")
		}
		if _, isArr := r.Value.([]any); isArr {
			return SegmentRule{}, core.Invalid(p+".value", "must be a single value; use values for a list")
		}
	}
	if r.Values, err = stringsMember(obj, mValues, p+"."); err != nil {
		return SegmentRule{}, err
	}
	if r.Days, err = intMember(obj, mDays, p+"."); err != nil {
		return SegmentRule{}, err
	}
	if r.WithinDays, err = intMember(obj, mWithinDays, p+"."); err != nil {
		return SegmentRule{}, err
	}
	if r.BroadcastID, err = int64Member(obj, mBroadcastID, p+"."); err != nil {
		return SegmentRule{}, err
	}
	return r, nil
}

// knownMembers rejects members not in allowed, reporting the first in a
// stable (sorted) order.
func knownMembers(obj map[string]any, allowed map[string]bool, prefix string) error {
	var unknown string
	for k := range obj {
		if !allowed[k] && (unknown == "" || k < unknown) {
			unknown = k
		}
	}
	if unknown != "" {
		return core.Invalid(prefix+unknown, "is not a known member")
	}
	return nil
}

func stringMember(obj map[string]any, name, prefix string) (string, error) {
	switch v := obj[name].(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	}
	return "", core.Invalid(prefix+name, "must be a string")
}

func rulesMember(obj map[string]any, prefix string) ([]SegmentRule, error) {
	v, ok := obj["rules"]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, core.Invalid(prefix+"rules", "must be an array of rules")
	}
	out := make([]SegmentRule, len(list))
	for i, item := range list {
		r, err := decodeRule(item, fmt.Sprintf("%srules[%d]", prefix, i))
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

func stringsMember(obj map[string]any, name, prefix string) ([]string, error) {
	v, ok := obj[name]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, core.Invalid(prefix+name, "must be an array of strings")
	}
	out := make([]string, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, core.Invalid(fmt.Sprintf("%s%s[%d]", prefix, name, i), "must be a string")
		}
		out[i] = s
	}
	return out, nil
}

func int64Member(obj map[string]any, name, prefix string) (*int64, error) {
	v, ok := obj[name]
	if !ok || v == nil {
		return nil, nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return nil, core.Invalid(prefix+name, "must be a whole number")
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil {
		return nil, core.Invalid(prefix+name, "must be a whole number")
	}
	return &i, nil
}

func intMember(obj map[string]any, name, prefix string) (*int, error) {
	i, err := int64Member(obj, name, prefix)
	if err != nil || i == nil {
		return nil, err
	}
	if int64(int(*i)) != *i {
		return nil, core.Invalid(prefix+name, "is out of range")
	}
	n := int(*i)
	return &n, nil
}
