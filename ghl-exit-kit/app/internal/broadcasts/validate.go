package broadcasts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/core"
)

// validName trims and checks a broadcast name.
func validName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if err := singleLine("name", n, MaxNameLength); err != nil {
		return n, err
	}
	if n == "" {
		return n, core.Invalid("name", "is required")
	}
	return n, nil
}

// validFromName trims and checks a From display name ("" = default).
func validFromName(n string) (string, error) {
	n = strings.TrimSpace(n)
	return n, singleLine("from_name", n, MaxFromNameLength)
}

// singleLine checks a one-line text value: valid UTF-8, at most maxLen
// characters and no control characters (it may end up in a header).
func singleLine(field, v string, maxLen int) error {
	switch {
	case !utf8.ValidString(v):
		return core.Invalid(field, "is not valid UTF-8")
	case utf8.RuneCountInString(v) > maxLen:
		return core.Invalid(field, "is longer than %d characters", maxLen)
	case strings.IndexFunc(v, unicode.IsControl) >= 0:
		return core.Invalid(field, "must be a single line without control characters")
	}
	return nil
}

// validReplyTo normalises a Reply-To address, which must be on the sending
// domain (D-7). "" means no Reply-To header.
func (s *Service) validReplyTo(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	norm, err := core.NormalizeEmail(v)
	if err != nil {
		return v, core.Invalid("reply_to", "is not a valid email address")
	}
	if d := s.domain(); d != "" && !strings.HasSuffix(norm, "@"+d) {
		return norm, core.Invalid("reply_to", "must be an address on the sending domain %s", d)
	}
	return norm, nil
}

func (s *Service) domain() string {
	if s.settings == nil {
		return ""
	}
	return strings.ToLower(s.settings.Domain())
}

// resolveAudience validates an audience and fills in the segment name.
func (s *Service) resolveAudience(ctx context.Context, a BroadcastAudience) (BroadcastAudience, error) {
	switch strings.TrimSpace(a.Type) {
	case "", AudienceAll:
		if a.SegmentID != 0 {
			return a, core.Invalid("audience.segment_id", "must be omitted for audience type all")
		}
		return BroadcastAudience{Type: AudienceAll}, nil
	case AudienceSegment:
	default:
		return a, core.Invalid("audience.type", "must be all or segment")
	}
	if a.SegmentID <= 0 {
		return a, core.Invalid("audience.segment_id", "is required for audience type segment")
	}
	if s.segments == nil {
		return a, core.Invalid("audience.type", "segment audiences are not available")
	}
	if _, _, err := s.segments.WhereSQL(ctx, a.SegmentID); err != nil {
		var ve *core.ValidationError
		switch {
		case errors.Is(err, core.ErrNotFound):
			return a, core.Invalid("audience.segment_id", "segment %d does not exist", a.SegmentID)
		case errors.As(err, &ve):
			return a, core.Invalid("audience.segment_id", "segment %d has an invalid filter (%s: %s)", a.SegmentID, ve.Field, ve.Message)
		}
		return a, err
	}
	var name string
	if err := s.db.Read.QueryRowContext(ctx, `SELECT name FROM segments WHERE id = ?`, a.SegmentID).Scan(&name); err != nil {
		return a, fmt.Errorf("broadcasts: segment %d name: %w", a.SegmentID, err)
	}
	return BroadcastAudience{Type: AudienceSegment, SegmentID: a.SegmentID, SegmentName: name}, nil
}

// emailName is the library name of a broadcast's email.
func emailName(broadcastName string) string {
	n := "Broadcast: " + broadcastName
	if utf8.RuneCountInString(n) <= MaxNameLength {
		return n
	}
	return string([]rune(n)[:MaxNameLength])
}

// requireSender checks that the settings have a From address, so a started
// broadcast doesn't fail every message, and a company footer (E-1), so its
// messages don't all wait for one.
func (s *Service) requireSender(ctx context.Context) error {
	if s.settings == nil {
		return nil
	}
	st, err := s.settings.Get(ctx)
	if err != nil {
		return err
	}
	if st.Sender.FromEmail == "" {
		return core.Invalid("sender.from_email", "is not set in the settings: set it before sending or scheduling a broadcast")
	}
	if strings.TrimSpace(st.CompanyFooter) == "" {
		return core.Invalid("company_footer", "is not set in the settings: every marketing email must show the sender's name and postal address; set it before sending or scheduling a broadcast")
	}
	return nil
}
