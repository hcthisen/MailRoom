package forms

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strings"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

const formColumns = `id, public_id, name, fields, tag_ids, sequence_id, double_optin, consent_text,
	redirect_url, allowed_origins, version, created_at, updated_at`

// formValues are a form's validated, writable values.
type formValues struct {
	Name        string
	Fields      []FormField
	Tags        []string
	SequenceID  int64 // 0 = none
	DoubleOptin bool
	ConsentText string
	RedirectURL string
	Origins     []string
}

// record is a stored form with the tag IDs behind its tag names.
type record struct {
	Form
	TagIDs []int64
}

func (r record) values() formValues {
	v := formValues{
		Name: r.Name, Fields: r.Fields, Tags: r.Tags, DoubleOptin: r.DoubleOptin,
		ConsentText: r.ConsentText, RedirectURL: r.RedirectURL, Origins: r.AllowedOrigins,
	}
	if r.SequenceID != nil {
		v.SequenceID = *r.SequenceID
	}
	return v
}

func validateInput(in FormInput) (formValues, error) {
	var v formValues
	var err error
	if v.Name, err = validName(in.Name); err != nil {
		return v, err
	}
	if v.Fields, err = ValidateFields(in.Fields); err != nil {
		return v, err
	}
	if v.Tags, err = validTags(in.Tags); err != nil {
		return v, err
	}
	if in.SequenceID != nil {
		v.SequenceID = *in.SequenceID
	}
	if v.SequenceID < 0 {
		return v, core.Invalid("sequence_id", "must be a sequence ID")
	}
	v.DoubleOptin = in.DoubleOptin
	if v.ConsentText, err = validConsentText(in.ConsentText); err != nil {
		return v, err
	}
	if v.RedirectURL, err = validRedirectURL(in.RedirectURL); err != nil {
		return v, err
	}
	if v.Origins, err = ValidateOrigins(in.AllowedOrigins); err != nil {
		return v, err
	}
	return v, nil
}

// apply validates the members of p and applies them to v.
func (p FormPatch) apply(v formValues) (formValues, error) {
	var err error
	if p.Name != nil {
		if v.Name, err = validName(*p.Name); err != nil {
			return v, err
		}
	}
	if p.Fields != nil {
		if v.Fields, err = ValidateFields(*p.Fields); err != nil {
			return v, err
		}
	}
	if p.Tags != nil {
		if v.Tags, err = validTags(*p.Tags); err != nil {
			return v, err
		}
	}
	if p.SequenceID != nil {
		if *p.SequenceID < 0 {
			return v, core.Invalid("sequence_id", "must be a sequence ID, or 0 for none")
		}
		v.SequenceID = *p.SequenceID
	}
	if p.DoubleOptin != nil {
		v.DoubleOptin = *p.DoubleOptin
	}
	if p.ConsentText != nil {
		if v.ConsentText, err = validConsentText(*p.ConsentText); err != nil {
			return v, err
		}
	}
	if p.RedirectURL != nil {
		if v.RedirectURL, err = validRedirectURL(*p.RedirectURL); err != nil {
			return v, err
		}
	}
	if p.AllowedOrigins != nil {
		if v.Origins, err = ValidateOrigins(*p.AllowedOrigins); err != nil {
			return v, err
		}
	}
	return v, nil
}

// Create validates and stores a new form with a fresh public ID. Tags are
// created as needed. With dryRun nothing is stored, and the returned form
// shows what would be created.
func (s *Service) Create(ctx context.Context, in FormInput, dryRun bool) (Form, error) {
	v, err := validateInput(in)
	if err != nil {
		return Form{}, err
	}
	var out Form
	err = api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		tagIDs, err := s.resolveTags(ctx, tx, v)
		if err != nil {
			return err
		}
		cols, err := columnValues(v, tagIDs)
		if err != nil {
			return err
		}
		now := db.FormatTime(s.clock.Now())
		var id int64
		for attempt := 0; ; attempt++ {
			pid, err := newPublicID()
			if err != nil {
				return err
			}
			err = tx.QueryRowContext(ctx, `
				INSERT INTO forms (public_id, name, fields, tag_ids, sequence_id, double_optin, consent_text,
				                   redirect_url, allowed_origins, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
				append(append([]any{pid}, cols...), now, now)...).Scan(&id)
			if db.IsUniqueViolation(err) && attempt < 5 {
				continue
			}
			if err != nil {
				return fmt.Errorf("insert form: %w", err)
			}
			break
		}
		rec, err := s.getRecord(ctx, tx, `id = ?`, id)
		if err != nil {
			return err
		}
		out = rec.Form
		return s.audit(ctx, tx, "form.create", id, map[string]any{
			"name": v.Name, "public_id": out.PublicID, "fields": v.Fields, "tags": v.Tags,
			"sequence_id": out.SequenceID, "double_optin": v.DoubleOptin, "consent_text": v.ConsentText,
			"redirect_url": v.RedirectURL, "allowed_origins": v.Origins,
		})
	})
	if err != nil {
		return Form{}, err
	}
	return out, nil
}

// Get returns a form; unknown IDs wrap core.ErrNotFound.
func (s *Service) Get(ctx context.Context, id int64) (Form, error) {
	rec, err := s.getRecord(ctx, s.db.Read, `id = ?`, id)
	return rec.Form, err
}

// GetByPublicID returns the form with the given public slug (q nil = read
// pool); unknown slugs wrap core.ErrNotFound.
func (s *Service) GetByPublicID(ctx context.Context, q db.Querier, publicID string) (Form, error) {
	if q == nil {
		q = s.db.Read
	}
	rec, err := s.getRecord(ctx, q, `public_id = ?`, publicID)
	return rec.Form, err
}

// List returns up to limit forms with ID greater than afterID, by ID.
func (s *Service) List(ctx context.Context, afterID int64, limit int) ([]Form, error) {
	recs, err := s.queryRecords(ctx, s.db.Read, `id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Form, len(recs))
	for i, r := range recs {
		out[i] = r.Form
	}
	return out, nil
}

// Update applies p to form id. ifVersion (If-Match) must match unless 0.
// A patch that changes nothing returns the form unchanged. With dryRun the
// change is validated and returned but not stored.
func (s *Service) Update(ctx context.Context, id int64, p FormPatch, ifVersion int64, dryRun bool) (Form, error) {
	var out Form
	err := api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		cur, err := s.getRecord(ctx, tx, `id = ?`, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		curV := cur.values()
		next, err := p.apply(curV)
		if err != nil {
			return err
		}
		tagIDs := cur.TagIDs
		if p.Tags != nil {
			if tagIDs, err = s.resolveTags(ctx, tx, next); err != nil {
				return err
			}
		} else if p.SequenceID != nil && next.SequenceID != curV.SequenceID {
			if err := checkSequence(ctx, tx, next.SequenceID); err != nil {
				return err
			}
		}
		changes := diffValues(curV, next, cur.TagIDs, tagIDs)
		if len(changes) == 0 {
			out = cur.Form
			return nil
		}
		cols, err := columnValues(next, tagIDs)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE forms SET name = ?, fields = ?, tag_ids = ?, sequence_id = ?, double_optin = ?, consent_text = ?,
			                 redirect_url = ?, allowed_origins = ?, version = version + 1, updated_at = ?
			WHERE id = ?`,
			append(cols, db.FormatTime(s.clock.Now()), id)...); err != nil {
			return fmt.Errorf("update form %d: %w", id, err)
		}
		rec, err := s.getRecord(ctx, tx, `id = ?`, id)
		if err != nil {
			return err
		}
		out = rec.Form
		return s.audit(ctx, tx, "form.update", id, changes)
	})
	if err != nil {
		return Form{}, err
	}
	return out, nil
}

// Delete removes a form and its submission log. Pending confirmations of
// it stay valid: they then apply the tags and sequence the form had at
// sign-up, stored with them (see signupSettings). ifVersion
// (If-Match) must match unless 0. With dryRun nothing is deleted.
func (s *Service) Delete(ctx context.Context, id int64, ifVersion int64, dryRun bool) error {
	return api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		cur, err := s.getRecord(ctx, tx, `id = ?`, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM forms WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete form %d: %w", id, err)
		}
		return s.audit(ctx, tx, "form.delete", id, map[string]any{"name": cur.Name, "public_id": cur.PublicID})
	})
}

// diffValues returns the audit changes between two versions of a form.
func diffValues(a, b formValues, aTags, bTags []int64) map[string]any {
	ch := map[string]any{}
	audit.Diff(ch, "name", a.Name, b.Name)
	if !slices.Equal(a.Fields, b.Fields) {
		ch["fields"] = audit.Change{From: a.Fields, To: b.Fields}
	}
	if !slices.Equal(aTags, bTags) {
		ch["tags"] = audit.Change{From: a.Tags, To: b.Tags}
	}
	audit.Diff(ch, "sequence_id", a.SequenceID, b.SequenceID)
	audit.Diff(ch, "double_optin", a.DoubleOptin, b.DoubleOptin)
	audit.Diff(ch, "consent_text", a.ConsentText, b.ConsentText)
	audit.Diff(ch, "redirect_url", a.RedirectURL, b.RedirectURL)
	if !slices.Equal(a.Origins, b.Origins) {
		ch["allowed_origins"] = audit.Change{From: a.Origins, To: b.Origins}
	}
	return ch
}

// resolveTags checks the sequence and returns the IDs of v's tags,
// creating missing tags.
func (s *Service) resolveTags(ctx context.Context, tx *sql.Tx, v formValues) ([]int64, error) {
	if err := checkSequence(ctx, tx, v.SequenceID); err != nil {
		return nil, err
	}
	now := db.FormatTime(s.clock.Now())
	ids := make([]int64, 0, len(v.Tags))
	for _, name := range v.Tags {
		id, _, err := contactstore.EnsureTag(ctx, tx, name, now)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// checkSequence fails with a validation error unless id is 0 or an
// existing sequence.
func checkSequence(ctx context.Context, q db.Querier, id int64) error {
	if id == 0 {
		return nil
	}
	ok, err := sequenceExists(ctx, q, id)
	if err != nil {
		return err
	}
	if !ok {
		return core.Invalid("sequence_id", "sequence %d does not exist", id)
	}
	return nil
}

func sequenceExists(ctx context.Context, q db.Querier, id int64) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM sequences WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find sequence %d: %w", id, err)
	}
	return true, nil
}

// columnValues returns the stored columns name .. allowed_origins of v.
func columnValues(v formValues, tagIDs []int64) ([]any, error) {
	fields, err := db.MarshalJSON(v.Fields)
	if err != nil {
		return nil, err
	}
	tags, err := db.MarshalJSON(tagIDs)
	if err != nil {
		return nil, err
	}
	origins, err := db.MarshalJSON(v.Origins)
	if err != nil {
		return nil, err
	}
	return []any{v.Name, fields, tags, db.NullID(v.SequenceID), v.DoubleOptin, v.ConsentText, v.RedirectURL, origins}, nil
}

// getRecord returns the one form matching where; none wraps ErrNotFound.
func (s *Service) getRecord(ctx context.Context, q db.Querier, where string, args ...any) (record, error) {
	recs, err := s.queryRecords(ctx, q, where+` LIMIT 1`, args...)
	if err != nil {
		return record{}, err
	}
	if len(recs) == 0 {
		return record{}, fmt.Errorf("form: %w", core.ErrNotFound)
	}
	return recs[0], nil
}

// queryRecords returns the forms matching where (which may end in ORDER
// BY / LIMIT), with their tag names resolved.
func (s *Service) queryRecords(ctx context.Context, q db.Querier, where string, args ...any) ([]record, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+formColumns+` FROM forms WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("query forms: %w", err)
	}
	var recs []record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		r.HostedURL = s.HostedURL(r.PublicID)
		r.Endpoint = r.HostedURL
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query forms: %w", err)
	}
	// Tag names are read at use time: tags may be renamed or merged.
	for i := range recs {
		if recs[i].Tags, err = tagNames(ctx, q, recs[i].TagIDs); err != nil {
			return nil, err
		}
	}
	return recs, nil
}

func scanRecord(rows *sql.Rows) (record, error) {
	var (
		r                       record
		fields, tagIDs, origins string
		seq                     sql.NullInt64
		created, updated        string
	)
	if err := rows.Scan(&r.ID, &r.PublicID, &r.Name, &fields, &tagIDs, &seq, &r.DoubleOptin, &r.ConsentText,
		&r.RedirectURL, &origins, &r.Version, &created, &updated); err != nil {
		return record{}, fmt.Errorf("scan form: %w", err)
	}
	r.Fields, r.TagIDs, r.AllowedOrigins = []FormField{}, []int64{}, []string{}
	if err := db.UnmarshalJSON(fields, &r.Fields); err != nil {
		return record{}, err
	}
	if err := db.UnmarshalJSON(tagIDs, &r.TagIDs); err != nil {
		return record{}, err
	}
	if err := db.UnmarshalJSON(origins, &r.AllowedOrigins); err != nil {
		return record{}, err
	}
	if seq.Valid {
		r.SequenceID = &seq.Int64
	}
	var err error
	if r.CreatedAt, err = db.ParseTime(created); err != nil {
		return record{}, err
	}
	if r.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return record{}, err
	}
	return r, nil
}

// tagNames returns the names of tag IDs in order, skipping unknown IDs.
func tagNames(ctx context.Context, q db.Querier, ids []int64) ([]string, error) {
	names := []string{}
	if len(ids) == 0 {
		return names, nil
	}
	raw, err := db.MarshalJSON(ids)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT t.name FROM json_each(?) j JOIN tags t ON t.id = j.value ORDER BY j.key`, raw)
	if err != nil {
		return nil, fmt.Errorf("resolve form tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// publicIDEncoding is lowercase base32 without padding.
var publicIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newPublicID returns a random PublicIDLength-character slug (50 bits).
func newPublicID() (string, error) {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("forms: random public id: %w", err)
	}
	return publicIDEncoding.EncodeToString(b)[:PublicIDLength], nil
}

// validPublicID reports whether s looks like a public ID, so malformed
// slugs are refused without a query.
func validPublicID(s string) bool {
	return len(s) == PublicIDLength && strings.Trim(s, "abcdefghijklmnopqrstuvwxyz234567") == ""
}
