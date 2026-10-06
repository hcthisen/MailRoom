package sequences

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
)

// SequenceInput is the configuration of a new sequence. Zero values take
// the defaults: status active, reentry never, no trigger, no exit tags, no
// send window of its own.
type SequenceInput struct {
	Name       string
	Status     string
	TriggerTag *string
	ExitTags   []string
	ExitAddTag *string
	Reentry    string
	SendWindow *settings.SendWindow
}

// SequencePatch changes the set fields of a sequence. TriggerTag,
// ExitAddTag and SendWindow tell "absent" from null (which clears them);
// ExitTags nil leaves them unchanged, an empty slice clears them.
type SequencePatch struct {
	Name       *string
	Status     *string
	TriggerTag OptionalString
	ExitTags   *[]string
	ExitAddTag OptionalString
	Reentry    *string
	SendWindow settings.OptionalSendWindow
}

// OptionalString tells "absent" from "null" from a value in a PATCH body:
// Set is true when the field was present; Value is nil for null.
type OptionalString struct {
	Set   bool
	Value *string
}

// UnmarshalJSON records that the field was present.
func (o *OptionalString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.Value = nil
		return nil
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// MarshalJSON encodes the value (null when unset or null).
func (o OptionalString) MarshalJSON() ([]byte, error) {
	if o.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*o.Value)
}

// Schema documents the field as a nullable tag name.
func (OptionalString) Schema(huma.Registry) *huma.Schema {
	maxLen := contactstore.MaxTagLength
	return &huma.Schema{Type: huma.TypeString, Nullable: true, MaxLength: &maxLen}
}

// normalized is a validated sequence configuration.
type normalized struct {
	Name       string
	Status     string
	TriggerTag string // "" = none
	ExitTags   []string
	ExitAddTag string // "" = none
	Reentry    string
	Window     *settings.SendWindow
}

// normalize validates in and returns its normalised form.
func normalize(in SequenceInput) (normalized, error) {
	var n normalized
	var err error
	if n.Name, err = validateName(in.Name); err != nil {
		return n, err
	}
	n.Status = strings.TrimSpace(in.Status)
	if n.Status == "" {
		n.Status = StatusActive
	}
	if n.Status != StatusActive && n.Status != StatusPaused {
		return n, core.Invalid("status", "must be active or paused")
	}
	n.Reentry = strings.TrimSpace(in.Reentry)
	if n.Reentry == "" {
		n.Reentry = ReentryNever
	}
	switch n.Reentry {
	case ReentryNever, ReentryAfterCompletion, ReentryAlways:
	default:
		return n, core.Invalid("reentry", "must be never, after_completion or always")
	}
	if n.TriggerTag, err = optionalTag("trigger_tag", in.TriggerTag); err != nil {
		return n, err
	}
	if n.ExitAddTag, err = optionalTag("exit_add_tag", in.ExitAddTag); err != nil {
		return n, err
	}
	if len(in.ExitTags) > MaxExitTags {
		return n, core.Invalid("exit_tags", "at most %d exit tags are allowed", MaxExitTags)
	}
	if n.ExitTags, err = contactstore.NormalizeTags(in.ExitTags); err != nil {
		return n, retarget(err, "exit_tags")
	}
	if n.TriggerTag != "" && slices.ContainsFunc(n.ExitTags, func(t string) bool { return sameTag(t, n.TriggerTag) }) {
		return n, core.Invalid("exit_tags", "must not contain the trigger tag %q: every contact would leave before the first step", n.TriggerTag)
	}
	if n.ExitAddTag != "" && n.TriggerTag != "" && sameTag(n.ExitAddTag, n.TriggerTag) {
		return n, core.Invalid("exit_add_tag", "must differ from the trigger tag")
	}
	if in.SendWindow != nil {
		if err := in.SendWindow.Validate("send_window"); err != nil {
			return n, err
		}
		w := *in.SendWindow
		w.Days = slices.Clone(w.Days)
		slices.Sort(w.Days)
		n.Window = &w
	}
	return n, nil
}

// validateName checks a sequence name.
func validateName(name string) (string, error) {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return "", core.Invalid("name", "is required")
	case utf8.RuneCountInString(n) > MaxNameLength:
		return "", core.Invalid("name", "is longer than %d characters", MaxNameLength)
	case !utf8.ValidString(n) || strings.IndexFunc(n, unicode.IsControl) >= 0:
		return "", core.Invalid("name", "must be a single line without control characters")
	}
	return n, nil
}

// optionalTag normalises an optional tag name ("" = none).
func optionalTag(field string, v *string) (string, error) {
	if v == nil {
		return "", nil
	}
	t, err := contactstore.NormalizeTag(*v)
	if err != nil {
		return "", retarget(err, field)
	}
	return t, nil
}

// retarget moves a validation error to another field.
func retarget(err error, field string) error {
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		return core.Invalid(field, "%s", ve.Message)
	}
	return err
}

// validationField returns the field of a validation error.
func validationField(err error) (string, bool) {
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		return ve.Field, true
	}
	return "", false
}

// sameTag compares tag names like the tags table (ASCII case-insensitive).
func sameTag(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// input returns the configuration of a stored sequence as a SequenceInput.
func (v Sequence) input() SequenceInput {
	return SequenceInput{
		Name: v.Name, Status: v.Status, TriggerTag: v.TriggerTag, ExitTags: v.ExitTags,
		ExitAddTag: v.ExitAddTag, Reentry: v.Reentry, SendWindow: v.SendWindow,
	}
}

// apply returns in with p applied.
func (p SequencePatch) apply(in SequenceInput) SequenceInput {
	if p.Name != nil {
		in.Name = *p.Name
	}
	if p.Status != nil {
		in.Status = *p.Status
	}
	if p.TriggerTag.Set {
		in.TriggerTag = p.TriggerTag.Value
	}
	if p.ExitTags != nil {
		in.ExitTags = *p.ExitTags
	}
	if p.ExitAddTag.Set {
		in.ExitAddTag = p.ExitAddTag.Value
	}
	if p.Reentry != nil {
		in.Reentry = *p.Reentry
	}
	if p.SendWindow.Set {
		in.SendWindow = p.SendWindow.Value
	}
	return in
}

// tagRefs are the tag IDs of a normalised configuration (0 = none).
type tagRefs struct {
	Trigger int64
	Exits   []int64
	ExitAdd int64
}

// ensureTags creates the configuration's tags as needed, rewrites their
// names in n to the stored spelling and returns their IDs.
func (s *Service) ensureTags(ctx context.Context, tx *sql.Tx, n *normalized) (tagRefs, error) {
	now := db.FormatTime(s.now())
	ensure := func(name *string) (int64, error) {
		if *name == "" {
			return 0, nil
		}
		id, stored, err := contactstore.EnsureTag(ctx, tx, *name, now)
		if err != nil {
			return 0, err
		}
		*name = stored
		return id, nil
	}
	var r tagRefs
	var err error
	if r.Trigger, err = ensure(&n.TriggerTag); err != nil {
		return r, err
	}
	if r.ExitAdd, err = ensure(&n.ExitAddTag); err != nil {
		return r, err
	}
	r.Exits = make([]int64, 0, len(n.ExitTags))
	for i := range n.ExitTags {
		id, err := ensure(&n.ExitTags[i])
		if err != nil {
			return r, err
		}
		r.Exits = append(r.Exits, id)
	}
	return r, nil
}

// windowJSON encodes a send window for the send_window column.
func windowJSON(w *settings.SendWindow) (any, error) {
	if w == nil {
		return nil, nil
	}
	return db.MarshalJSON(w)
}
