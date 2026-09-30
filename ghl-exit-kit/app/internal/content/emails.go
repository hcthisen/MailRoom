package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Email formats.
const (
	FormatMarkdown = "markdown"
	FormatHTML     = "html"
)

// Email field limits.
const (
	MaxNameLength      = 200     // characters
	MaxSubjectLength   = 300     // characters
	MaxPreheaderLength = 300     // characters
	MaxBodyBytes       = 1 << 20 // 1 MiB
)

// Email is a stored email: the content of broadcasts and sequence steps.
type Email struct {
	ID        int64     `json:"id" example:"3"`
	Name      string    `json:"name" doc:"Internal name, shown in lists" example:"Welcome 1"`
	Subject   string    `json:"subject" doc:"Subject line; may use merge fields" example:"Welcome, {{first_name | \"friend\"}}!"`
	Preheader string    `json:"preheader" doc:"Preview text shown after the subject in most inboxes; may use merge fields" example:"Here is what happens next"`
	Format    string    `json:"format" enum:"markdown,html" doc:"markdown (default) or html (raw HTML, e.g. imported from GHL)" example:"markdown"`
	Body      string    `json:"body" doc:"The content, wrapped in the base layout when sent" example:"Hi {{first_name | \"there\"}},\n\nThanks for joining."`
	Version   int64     `json:"version" doc:"Increases with every change; sent as the ETag" example:"1"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EmailSummary is an email without its body, for lists.
type EmailSummary struct {
	ID        int64  `json:"id" example:"3"`
	Name      string `json:"name" example:"Welcome 1"`
	Subject   string `json:"subject" example:"Welcome!"`
	Preheader string `json:"preheader" example:""`
	Format    string `json:"format" enum:"markdown,html" example:"markdown"`
	BodyBytes int    `json:"body_bytes" doc:"Size of the body in bytes" example:"1830"`
	// UsedBySteps and UsedByBroadcasts count what uses the email; both 0
	// means nothing does (get-email-usage has the details).
	UsedBySteps      int       `json:"used_by_steps" doc:"Number of sequence steps that send this email" example:"1"`
	UsedByBroadcasts int       `json:"used_by_broadcasts" doc:"Number of broadcasts (any status) whose content this is" example:"0"`
	Version          int64     `json:"version" example:"1"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// EmailInput is the content of a new email.
type EmailInput struct {
	Name      string
	Subject   string
	Preheader string
	Format    string // "" = markdown
	Body      string
}

// EmailPatch changes the non-nil fields of an email.
type EmailPatch struct {
	Name      *string
	Subject   *string
	Preheader *string
	Format    *string
	Body      *string
}

// ListFilter selects emails for ListEmails.
type ListFilter struct {
	AfterID int64  // keyset cursor: only IDs greater than this
	Limit   int    // maximum rows (callers pass page size + 1)
	Query   string // case-insensitive substring of name or subject
}

// ValidateEmail normalises in (trimmed name and subject, lowercased
// format, markdown by default) and checks lengths and merge fields. Errors
// are core validation errors naming the field.
func ValidateEmail(in EmailInput) (EmailInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Subject = strings.TrimSpace(in.Subject)
	in.Preheader = strings.TrimSpace(in.Preheader)
	in.Format = strings.ToLower(strings.TrimSpace(in.Format))
	if in.Format == "" {
		in.Format = FormatMarkdown
	}
	if err := checkLine("name", in.Name, MaxNameLength, false); err != nil {
		return in, err
	}
	if err := checkLine("subject", in.Subject, MaxSubjectLength, true); err != nil {
		return in, err
	}
	if err := checkLine("preheader", in.Preheader, MaxPreheaderLength, false); err != nil {
		return in, err
	}
	if in.Format != FormatMarkdown && in.Format != FormatHTML {
		return in, invalid("format", "must be markdown or html")
	}
	switch {
	case strings.TrimSpace(in.Body) == "":
		return in, invalid("body", "is required")
	case len(in.Body) > MaxBodyBytes:
		return in, invalid("body", "is larger than %d bytes", MaxBodyBytes)
	case !utf8.ValidString(in.Body):
		return in, invalid("body", "is not valid UTF-8")
	}
	for _, f := range []struct{ name, v string }{{"subject", in.Subject}, {"preheader", in.Preheader}, {"body", in.Body}} {
		if err := ValidateMergeFields(f.name, f.v); err != nil {
			return in, err
		}
	}
	return in, nil
}

// ValidateMergeFields checks that every {{...}} in s is a known merge
// field with a well-formed fallback. field names the input in errors.
func ValidateMergeFields(field, s string) error {
	_, err := parseMerge(field, s, emailVocabulary)
	return err
}

// checkLine validates a single-line text field.
func checkLine(field, v string, maxLen int, required bool) error {
	switch {
	case required && v == "":
		return invalid(field, "is required")
	case utf8.RuneCountInString(v) > maxLen:
		return invalid(field, "is longer than %d characters", maxLen)
	case !utf8.ValidString(v):
		return invalid(field, "is not valid UTF-8")
	case strings.IndexFunc(v, unicode.IsControl) >= 0:
		return invalid(field, "must be a single line without control characters")
	}
	return nil
}

const emailColumns = `id, name, subject, preheader, format, body, version, created_at, updated_at`

func scanEmail(sc interface{ Scan(...any) error }) (Email, error) {
	var e Email
	var created, updated string
	if err := sc.Scan(&e.ID, &e.Name, &e.Subject, &e.Preheader, &e.Format, &e.Body, &e.Version, &created, &updated); err != nil {
		return Email{}, err
	}
	var err error
	if e.CreatedAt, err = db.ParseTime(created); err != nil {
		return Email{}, err
	}
	if e.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return Email{}, err
	}
	return e, nil
}

// GetEmail returns an email. A nil q reads from the read pool. Unknown IDs
// wrap core.ErrNotFound.
func (s *Service) GetEmail(ctx context.Context, q db.Querier, id int64) (Email, error) {
	if q == nil {
		q = s.db.Read
	}
	e, err := scanEmail(q.QueryRowContext(ctx, `SELECT `+emailColumns+` FROM emails WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Email{}, fmt.Errorf("email %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return Email{}, fmt.Errorf("content: get email %d: %w", id, err)
	}
	return e, nil
}

// EmailExists reports whether the email exists. A nil q reads from the
// read pool.
func (s *Service) EmailExists(ctx context.Context, q db.Querier, id int64) (bool, error) {
	if q == nil {
		q = s.db.Read
	}
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM emails WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("content: email exists %d: %w", id, err)
	}
	return true, nil
}

// ListEmails returns emails ordered by ID, without bodies.
func (s *Service) ListEmails(ctx context.Context, f ListFilter) ([]EmailSummary, error) {
	if f.Limit <= 0 {
		f.Limit = api.DefaultLimit
	}
	query := `SELECT id, name, subject, preheader, format, length(CAST(body AS BLOB)),
		(SELECT count(*) FROM sequence_steps st WHERE st.email_id = emails.id),
		(SELECT count(*) FROM broadcasts b WHERE b.email_id = emails.id),
		version, created_at, updated_at
		FROM emails WHERE id > ?`
	args := []any{f.AfterID}
	if q := strings.TrimSpace(f.Query); q != "" {
		query += ` AND (instr(lower(name), lower(?)) > 0 OR instr(lower(subject), lower(?)) > 0)`
		args = append(args, q, q)
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.Read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("content: list emails: %w", err)
	}
	defer rows.Close()
	var out []EmailSummary
	for rows.Next() {
		var e EmailSummary
		var created, updated string
		if err := rows.Scan(&e.ID, &e.Name, &e.Subject, &e.Preheader, &e.Format, &e.BodyBytes,
			&e.UsedBySteps, &e.UsedByBroadcasts, &e.Version, &created, &updated); err != nil {
			return nil, fmt.Errorf("content: list emails: %w", err)
		}
		if e.CreatedAt, err = db.ParseTime(created); err != nil {
			return nil, err
		}
		if e.UpdatedAt, err = db.ParseTime(updated); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EmailUsage is what uses an email: the sequence steps that send it, the
// broadcasts whose content it is, and test or sequence messages with it
// still waiting to be sent. DeleteEmail refuses while any of them exist.
type EmailUsage struct {
	Steps           []EmailStepUse      `json:"sequence_steps" doc:"Sequence steps that send the email"`
	Broadcasts      []EmailBroadcastUse `json:"broadcasts" doc:"Broadcasts whose content the email is (a broadcast owns its email)"`
	PendingMessages int                 `json:"pending_messages" doc:"Messages with this email still queued or sending" example:"0"`
}

// EmailStepUse is a sequence step that sends an email.
type EmailStepUse struct {
	SequenceID   int64  `json:"sequence_id" example:"2"`
	SequenceName string `json:"sequence_name" example:"Welcome series"`
	StepID       int64  `json:"step_id" example:"5"`
	Position     int    `json:"position" doc:"1-based position of the step in its sequence" example:"1"`
}

// EmailBroadcastUse is a broadcast that owns an email.
type EmailBroadcastUse struct {
	ID     int64  `json:"id" example:"4"`
	Name   string `json:"name" example:"May newsletter"`
	Status string `json:"status" enum:"draft,scheduled,sending,paused,sent,cancelled" example:"draft"`
}

// InUse reports whether anything refers to the email.
func (u EmailUsage) InUse() bool {
	return len(u.Steps) > 0 || len(u.Broadcasts) > 0 || u.PendingMessages > 0
}

// EmailUsage returns what uses email id (core.ErrNotFound when it doesn't
// exist).
func (s *Service) EmailUsage(ctx context.Context, id int64) (EmailUsage, error) {
	if _, err := s.GetEmail(ctx, nil, id); err != nil {
		return EmailUsage{}, err
	}
	u := EmailUsage{Steps: []EmailStepUse{}, Broadcasts: []EmailBroadcastUse{}}
	rows, err := s.db.Read.QueryContext(ctx, `SELECT sq.id, sq.name, st.id, st.position
		FROM sequence_steps st JOIN sequences sq ON sq.id = st.sequence_id
		WHERE st.email_id = ? ORDER BY sq.name, st.position`, id)
	if err != nil {
		return EmailUsage{}, fmt.Errorf("content: email %d steps: %w", id, err)
	}
	for rows.Next() {
		var x EmailStepUse
		if err := rows.Scan(&x.SequenceID, &x.SequenceName, &x.StepID, &x.Position); err != nil {
			rows.Close()
			return EmailUsage{}, fmt.Errorf("content: email %d steps: %w", id, err)
		}
		u.Steps = append(u.Steps, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return EmailUsage{}, fmt.Errorf("content: email %d steps: %w", id, err)
	}
	rows, err = s.db.Read.QueryContext(ctx, `SELECT id, name, status FROM broadcasts WHERE email_id = ? ORDER BY id`, id)
	if err != nil {
		return EmailUsage{}, fmt.Errorf("content: email %d broadcasts: %w", id, err)
	}
	for rows.Next() {
		var x EmailBroadcastUse
		if err := rows.Scan(&x.ID, &x.Name, &x.Status); err != nil {
			rows.Close()
			return EmailUsage{}, fmt.Errorf("content: email %d broadcasts: %w", id, err)
		}
		u.Broadcasts = append(u.Broadcasts, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return EmailUsage{}, fmt.Errorf("content: email %d broadcasts: %w", id, err)
	}
	if err := s.db.Read.QueryRowContext(ctx,
		`SELECT count(*) FROM messages WHERE email_id = ? AND status IN ('queued','sending')`, id).Scan(&u.PendingMessages); err != nil {
		return EmailUsage{}, fmt.Errorf("content: email %d messages: %w", id, err)
	}
	return u, nil
}

// CreateEmail validates and stores a new email in tx, with an audit
// record. Other packages (e.g. a sequence YAML import) call it with their
// own transaction.
func (s *Service) CreateEmail(ctx context.Context, tx *sql.Tx, in EmailInput) (Email, error) {
	in, err := ValidateEmail(in)
	if err != nil {
		return Email{}, err
	}
	now := s.clock.Now()
	ts := db.FormatTime(now)
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO emails (name, subject, preheader, format, body, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?) RETURNING id`,
		in.Name, in.Subject, in.Preheader, in.Format, in.Body, ts, ts).Scan(&id); err != nil {
		return Email{}, fmt.Errorf("content: create email: %w", err)
	}
	e := Email{ID: id, Name: in.Name, Subject: in.Subject, Preheader: in.Preheader, Format: in.Format,
		Body: in.Body, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	if err := audit.Write(ctx, tx, audit.Entry{
		Operation: "email.create", TargetType: "email", TargetID: strconv.FormatInt(id, 10), At: now,
		Changes: map[string]any{"name": e.Name, "subject": e.Subject, "format": e.Format, "body_bytes": len(e.Body)},
	}); err != nil {
		return Email{}, err
	}
	return e, nil
}

// UpdateEmail applies p to the email in tx. ifVersion (from If-Match) must
// match unless it is 0. A patch that changes nothing returns the email
// without a new version. The email of a broadcast that has left draft and
// scheduled (sending, paused, sent or cancelled) can't change (B-2): its
// content is rendered at send time, so an edit would reach the recipients
// not sent yet; that is core.ErrConflict.
func (s *Service) UpdateEmail(ctx context.Context, tx *sql.Tx, id int64, p EmailPatch, ifVersion int64) (Email, error) {
	cur, err := s.GetEmail(ctx, tx, id)
	if err != nil {
		return Email{}, err
	}
	if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
		return Email{}, err
	}
	in := EmailInput{Name: cur.Name, Subject: cur.Subject, Preheader: cur.Preheader, Format: cur.Format, Body: cur.Body}
	for _, f := range []struct {
		dst *string
		src *string
	}{{&in.Name, p.Name}, {&in.Subject, p.Subject}, {&in.Preheader, p.Preheader}, {&in.Format, p.Format}, {&in.Body, p.Body}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	if in, err = ValidateEmail(in); err != nil {
		return Email{}, err
	}
	changes := audit.Diff(nil, "name", cur.Name, in.Name)
	audit.Diff(changes, "subject", cur.Subject, in.Subject)
	audit.Diff(changes, "preheader", cur.Preheader, in.Preheader)
	audit.Diff(changes, "format", cur.Format, in.Format)
	if in.Body != cur.Body {
		changes["body_bytes"] = audit.Change{From: len(cur.Body), To: len(in.Body)}
	}
	if len(changes) == 0 {
		return cur, nil
	}
	if err := refuseLockedBroadcastEmail(ctx, tx, id); err != nil {
		return Email{}, err
	}
	now := s.clock.Now()
	res, err := tx.ExecContext(ctx,
		`UPDATE emails SET name = ?, subject = ?, preheader = ?, format = ?, body = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		in.Name, in.Subject, in.Preheader, in.Format, in.Body, db.FormatTime(now), id, cur.Version)
	if err != nil {
		return Email{}, fmt.Errorf("content: update email %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Email{}, fmt.Errorf("email %d changed concurrently: %w", id, core.ErrPrecondition)
	}
	if err := audit.Write(ctx, tx, audit.Entry{
		Operation: "email.update", TargetType: "email", TargetID: strconv.FormatInt(id, 10), Changes: changes, At: now,
	}); err != nil {
		return Email{}, err
	}
	cur.Name, cur.Subject, cur.Preheader, cur.Format, cur.Body = in.Name, in.Subject, in.Preheader, in.Format, in.Body
	cur.Version++
	cur.UpdatedAt = now.UTC()
	return cur, nil
}

// DeleteEmail deletes the email in tx. It refuses (core.ErrConflict) while
// a sequence step or a broadcast uses it, or messages that still have to
// be sent reference it.
func (s *Service) DeleteEmail(ctx context.Context, tx *sql.Tx, id int64, ifVersion int64) error {
	cur, err := s.GetEmail(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
		return err
	}
	var steps, broadcasts, drafts, pending int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM sequence_steps WHERE email_id = ?1),
		(SELECT count(*) FROM broadcasts WHERE email_id = ?1),
		(SELECT count(*) FROM broadcasts WHERE email_id = ?1 AND status = 'draft'),
		(SELECT count(*) FROM messages WHERE email_id = ?1 AND status IN ('queued','sending'))`, id).
		Scan(&steps, &broadcasts, &drafts, &pending); err != nil {
		return fmt.Errorf("content: email %d usage: %w", id, err)
	}
	var uses []string
	if steps > 0 {
		uses = append(uses, fmt.Sprintf("%d sequence step(s)", steps))
	}
	if broadcasts > drafts {
		uses = append(uses, fmt.Sprintf("%d sent or scheduled broadcast(s)", broadcasts-drafts))
	}
	if drafts > 0 {
		uses = append(uses, fmt.Sprintf("%d draft broadcast(s) (delete them or pick another email first)", drafts))
	}
	if pending > 0 {
		uses = append(uses, fmt.Sprintf("%d message(s) waiting to be sent", pending))
	}
	if len(uses) > 0 {
		return fmt.Errorf("email %d is used by %s: %w", id, strings.Join(uses, ", "), core.ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM emails WHERE id = ?`, id); err != nil {
		if db.IsForeignKeyViolation(err) {
			return fmt.Errorf("email %d is still referenced: %w", id, core.ErrConflict)
		}
		return fmt.Errorf("content: delete email %d: %w", id, err)
	}
	return audit.Write(ctx, tx, audit.Entry{
		Operation: "email.delete", TargetType: "email", TargetID: strconv.FormatInt(id, 10), At: s.clock.Now(),
		Changes: map[string]any{"name": cur.Name, "subject": cur.Subject},
	})
}

// refuseLockedBroadcastEmail returns core.ErrConflict when email id belongs
// to a broadcast whose status is past draft and scheduled.
func refuseLockedBroadcastEmail(ctx context.Context, q db.Querier, id int64) error {
	var bID int64
	var status string
	err := q.QueryRowContext(ctx, `SELECT id, status FROM broadcasts
		WHERE email_id = ? AND status NOT IN ('draft', 'scheduled') ORDER BY id LIMIT 1`, id).Scan(&bID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("content: check broadcasts of email %d: %w", id, err)
	}
	return fmt.Errorf("email %d is the content of broadcast %d, which is %s; only a draft or scheduled broadcast's email can change (duplicate the broadcast to send changed content): %w",
		id, bID, status, core.ErrConflict)
}
