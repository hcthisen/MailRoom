package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/outbox"
)

// dataDraft is the message data key that holds the content of a test of
// unsaved editor content (SendTestDraft).
const dataDraft = "draft"

// SendTestDraft queues a test (kind test) of content that may not be saved
// yet: the "Send test" button of the editor, which works before the first
// save. It behaves like SendTestAs, but the message carries the content
// itself in its data ({"test": true, "draft": {...}}) instead of an email
// ID, and Render renders it from there. The content is validated like on
// save first.
func (s *Service) SendTestDraft(ctx context.Context, tx *sql.Tx, d Draft, to []string, contactID int64, sender SenderOverride) ([]int64, []string, error) {
	in, err := ValidateEmail(EmailInput{Subject: d.Subject, Preheader: d.Preheader, Format: d.Format, Body: d.Body})
	if err != nil {
		return nil, nil, err
	}
	addrs, err := s.testRecipients(ctx, to)
	if err != nil {
		return nil, nil, err
	}
	data := map[string]any{"test": true, dataDraft: map[string]any{
		"subject": in.Subject, "preheader": in.Preheader, "format": in.Format, "body": in.Body,
	}}
	if contactID > 0 {
		if _, err := s.contacts.Get(ctx, tx, contactID); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return nil, nil, invalid("contact_id", "contact %d does not exist", contactID)
			}
			return nil, nil, err
		}
		data["contact_id"] = contactID
	}
	if sender.FromName != nil {
		data[core.DataFromName] = *sender.FromName
	}
	if sender.ReplyTo != nil {
		data[core.DataReplyTo] = *sender.ReplyTo
	}
	ids := make([]int64, 0, len(addrs))
	for _, a := range addrs {
		id, _, err := outbox.Enqueue(ctx, tx, s.clock, outbox.MessageSpec{Kind: core.KindTest, ToEmail: a, Data: data})
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	changes := map[string]any{"recipients": len(addrs), "message_ids": ids, "subject": in.Subject}
	if contactID > 0 {
		changes["contact_id"] = contactID
	}
	if err := audit.Write(ctx, tx, audit.Entry{
		Operation: "email.test_send_draft", TargetType: "email", TargetID: "draft",
		Changes: changes, At: s.clock.Now(),
	}); err != nil {
		return nil, nil, err
	}
	return ids, addrs, nil
}

// draftFromData reads the content SendTestDraft stored in message data.
func draftFromData(data map[string]any) (Draft, bool) {
	m, ok := data[dataDraft].(map[string]any)
	if !ok {
		return Draft{}, false
	}
	str := func(k string) string { v, _ := m[k].(string); return v }
	d := Draft{Subject: str("subject"), Preheader: str("preheader"), Format: str("format"), Body: str("body")}
	if d.Subject == "" || d.Body == "" {
		return Draft{}, false
	}
	if d.Format == "" {
		d.Format = FormatMarkdown
	}
	return d, true
}

// MergeField is one merge field the editor offers.
type MergeField struct {
	Tag      string `json:"tag" doc:"What to insert, e.g. {{first_name}}" example:"{{first_name}}"`
	Name     string `json:"name" doc:"The field name" example:"first_name"`
	Label    string `json:"label" doc:"A human label" example:"First name"`
	Contacts int64  `json:"contacts" doc:"How many contacts have a value for it (0 for unsubscribe_url)" example:"812"`
	Example  string `json:"example,omitempty" doc:"A value from one contact, to show what it looks like" example:"Anna"`
}

// maxAttributeFields bounds the attribute keys MergeFields lists.
const maxAttributeFields = 200

// MergeFields lists the merge fields an email may use: the contact
// fields, every attribute key found on contacts (most used first) and the
// unsubscribe link, each with how many contacts have a value.
func (s *Service) MergeFields(ctx context.Context) ([]MergeField, error) {
	var total, first, last int64
	err := s.db.Read.QueryRowContext(ctx, `SELECT count(*), IFNULL(sum(first_name <> ''), 0), IFNULL(sum(last_name <> ''), 0) FROM contacts`).
		Scan(&total, &first, &last)
	if err != nil {
		return nil, fmt.Errorf("content: count merge fields: %w", err)
	}
	example := func(col string) string {
		var v string
		_ = s.db.Read.QueryRowContext(ctx, `SELECT `+col+` FROM contacts WHERE `+col+` <> '' AND status = 'active' ORDER BY id LIMIT 1`).Scan(&v)
		return v
	}
	out := []MergeField{
		{Tag: "{{first_name}}", Name: fieldFirstName, Label: "First name", Contacts: first, Example: example("first_name")},
		{Tag: "{{last_name}}", Name: fieldLastName, Label: "Last name", Contacts: last, Example: example("last_name")},
		{Tag: "{{email}}", Name: fieldEmail, Label: "Email address", Contacts: total, Example: example("email")},
	}
	rows, err := s.db.Read.QueryContext(ctx, `SELECT j.key, count(*), max(CAST(j.value AS TEXT))
		FROM contacts c, json_each(c.attributes) j
		WHERE j.type NOT IN ('null', 'object', 'array') AND CAST(j.value AS TEXT) <> ''
		GROUP BY j.key ORDER BY count(*) DESC, j.key LIMIT ?`, maxAttributeFields)
	if err != nil {
		return nil, fmt.Errorf("content: list attribute keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			key string
			n   int64
			ex  sql.NullString
		)
		if err := rows.Scan(&key, &n, &ex); err != nil {
			return nil, fmt.Errorf("content: scan attribute key: %w", err)
		}
		if !validAttrKey(key) {
			continue
		}
		out = append(out, MergeField{Tag: "{{" + attrPrefix + key + "}}", Name: attrPrefix + key, Label: attrLabel(key), Contacts: n, Example: ex.String})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("content: list attribute keys: %w", err)
	}
	out = append(out, MergeField{Tag: "{{unsubscribe_url}}", Name: fieldUnsubscribe, Label: "Unsubscribe link (URL)"})
	return out, nil
}

// attrLabel turns an attribute key into a label: "company_name" → "Company name".
func attrLabel(key string) string {
	b := []byte(key)
	for i, c := range b {
		if c == '_' {
			b[i] = ' '
		}
	}
	if len(b) > 0 && b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

// UnguardedFields lists the contact merge fields d uses without a fallback
// (first_name, last_name, attr.<key>; not email or unsubscribe_url), each
// once, in order of first use. A contact without that value sees a gap,
// which the pre-send checklist warns about. Unparsable text yields none.
func UnguardedFields(d Draft) []string {
	seen := map[string]bool{}
	var out []string
	for _, src := range []string{d.Subject, d.Preheader, d.Body} {
		t, err := parseMerge("body", src, emailVocabulary)
		if err != nil {
			continue
		}
		for _, sg := range t {
			f := sg.Field
			if f == nil || f.Fallback != "" || f.Name == fieldEmail || f.Name == fieldUnsubscribe || seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			out = append(out, f.Name)
		}
	}
	return out
}

// LastTest returns when the latest test of the stored email emailID, or of
// unsaved content with this subject (SendTestDraft), was queued and its
// message status (queued, sent, delivered, skipped, failed, ...); a nil
// time when there was none.
func (s *Service) LastTest(ctx context.Context, emailID int64, subject string) (*time.Time, string, error) {
	var at, status string
	err := s.db.Read.QueryRowContext(ctx, `SELECT queued_at, status FROM messages WHERE kind = 'test'
		AND (email_id = ? OR (email_id IS NULL AND json_extract(data, '$.draft.subject') = ?)) ORDER BY id DESC LIMIT 1`, emailID, subject).Scan(&at, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("content: last test: %w", err)
	}
	t, err := db.ParseTime(at)
	if err != nil {
		return nil, "", err
	}
	return &t, status, nil
}

// PreviewLine renders a one-line field (subject or preheader) as contact c
// sees it (nil: fallbacks), as previews show it.
func PreviewLine(field, src string, c *core.Contact) (string, error) {
	return renderLine(field, src, contactValues(c, PreviewUnsubscribeURL))
}
