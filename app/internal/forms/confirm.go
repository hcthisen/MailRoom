package forms

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/tokens"
)

// Errors of confirmation links. The public pages explain each one.
var (
	// ErrLinkInvalid: the token is malformed, tampered with or unknown.
	ErrLinkInvalid = errors.New("forms: invalid confirmation link")
	// ErrLinkExpired: the link is older than OptinTTL, or the sign-up was
	// deleted after expiring.
	ErrLinkExpired = errors.New("forms: confirmation link expired")
	// ErrLinkUsed: the link was already confirmed.
	ErrLinkUsed = errors.New("forms: confirmation link already used")
	// ErrLinkSuperseded: a newer confirmation email replaced this link.
	ErrLinkSuperseded = errors.New("forms: confirmation link replaced by a newer one")
	// ErrCannotConfirm: the contact can no longer be (re)subscribed, e.g.
	// it bounced, complained or was suppressed since the sign-up.
	ErrCannotConfirm = errors.New("forms: address can no longer be subscribed")
)

// pendingOptin is a pending_optins row.
type pendingOptin struct {
	ID          int64
	FormID      int64
	ContactID   int64
	Kind        string
	Payload     optinPayload
	TokenHash   string
	ConsentText string
	ExpiresAt   time.Time
	Confirmed   bool
}

// ConfirmResult is what a confirmation did.
type ConfirmResult struct {
	Kind      string // KindOptin or KindResubscribe
	ContactID int64
	FormName  string
}

// CheckConfirmation verifies a confirmation token without changing
// anything (the GET page shows a button, so link prefetchers don't
// confirm). It returns the pending opt-in's kind, or one of the ErrLink*
// errors.
func (s *Service) CheckConfirmation(ctx context.Context, token string) (string, error) {
	p, err := s.lookupToken(ctx, s.db.Read, token)
	if err != nil {
		return "", err
	}
	return p.Kind, nil
}

// Confirm applies the pending opt-in behind token in one transaction: it
// marks it confirmed, activates the contact (source optin), records an
// optin_confirmed or resubscribe consent event with the confirming
// request's ip and user agent, and applies the sign-up: submitted fields,
// and the form's tags and sequence enrolment (read at use time, see
// signupSettings). For a contact that was already active (a double opt-in
// form submitted for a subscriber) the submitted names and attributes only
// fill in what the contact doesn't have yet: the confirmation email went
// to the subscriber, but the values came from whoever submitted the form.
func (s *Service) Confirm(ctx context.Context, token, ip, userAgent string) (ConfirmResult, error) {
	var res ConfirmResult
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		p, err := s.lookupToken(ctx, tx, token)
		if err != nil {
			return err
		}
		c, err := s.store.Get(ctx, tx, p.ContactID)
		if err != nil {
			return err
		}
		suppressed, err := s.store.IsSuppressed(ctx, tx, c.Email)
		if err != nil {
			return err
		}
		want := core.StatusPending
		consentType := contactstore.ConsentOptinConfirmed
		if p.Kind == KindResubscribe {
			want, consentType = core.StatusUnsubscribed, contactstore.ConsentResubscribe
		}
		if suppressed || (c.Status != want && c.Status != core.StatusActive) {
			return ErrCannotConfirm
		}
		now := db.FormatTime(s.clock.Now())
		if _, err := tx.ExecContext(ctx, `UPDATE pending_optins SET confirmed_at = ? WHERE id = ?`, now, p.ID); err != nil {
			return fmt.Errorf("confirm pending opt-in %d: %w", p.ID, err)
		}
		if c.Status != core.StatusActive {
			if _, err := s.store.SetStatus(ctx, tx, c.ID, core.StatusActive, contactstore.StatusChange{
				Source: contactstore.SourceOptin, Actor: core.ActorPublic,
			}); err != nil {
				return err
			}
		}
		source := formSource(p.Payload.PublicID)
		if err := s.store.RecordConsent(ctx, tx, contactstore.ConsentEvent{
			ContactID: c.ID, Type: consentType, Source: source, FormID: p.FormID,
			IP: ip, UserAgent: userAgent, ConsentText: p.ConsentText,
		}); err != nil {
			return err
		}
		tags, sequenceID, err := signupSettings(ctx, tx, p)
		if err != nil {
			return err
		}
		if err := s.applyPayload(ctx, tx, c, p.Payload, tags, sequenceID); err != nil {
			return err
		}
		res = ConfirmResult{Kind: p.Kind, ContactID: c.ID, FormName: p.Payload.FormName}
		return nil
	})
	if err != nil {
		return ConfirmResult{}, err
	}
	return res, nil
}

// signupSettings returns the tag names and sequence a confirmation
// applies. Like every tag and sequence reference, they are read at use
// time: while the form exists, its current tags (tag_ids follow renames
// and merges) and its current sequence, so a form edited between the
// sign-up and the click applies what it says at the click. Once the form
// is deleted, the sign-up's tag IDs that still exist (a renamed tag is
// followed, a merged-away or deleted one is not re-created) and the
// sequence of when they signed up. Only payloads without tag IDs fall
// back to the stored names.
func signupSettings(ctx context.Context, q db.Querier, p pendingOptin) ([]string, int64, error) {
	if p.FormID != 0 {
		var (
			raw string
			seq sql.NullInt64
		)
		err := q.QueryRowContext(ctx, `SELECT tag_ids, sequence_id FROM forms WHERE id = ?`, p.FormID).Scan(&raw, &seq)
		switch {
		case err == nil:
			var ids []int64
			if err := db.UnmarshalJSON(raw, &ids); err != nil {
				return nil, 0, err
			}
			tags, err := tagNames(ctx, q, ids)
			return tags, seq.Int64, err
		case !errors.Is(err, sql.ErrNoRows):
			return nil, 0, fmt.Errorf("read form %d: %w", p.FormID, err)
		}
	}
	if len(p.Payload.TagIDs) == 0 {
		return p.Payload.Tags, p.Payload.SequenceID, nil
	}
	tags, err := tagNames(ctx, q, p.Payload.TagIDs)
	return tags, p.Payload.SequenceID, err
}

// applyPayload updates the submitted fields of a confirmed sign-up, adds
// tags and starts sequenceID (see signupSettings). c is the contact as it
// was before the confirmation: when it was active, only the fields it
// doesn't have yet are filled in (missingDetails).
func (s *Service) applyPayload(ctx context.Context, tx *sql.Tx, c core.Contact, p optinPayload, tags []string, sequenceID int64) error {
	contactID := c.ID
	var first, last *string
	attrs := map[string]any{}
	for k, v := range p.Fields {
		if v == "" {
			continue
		}
		switch k {
		case FieldFirstName:
			first = &v
		case FieldLastName:
			last = &v
		default:
			if attr, ok := cutAttr(k); ok {
				attrs[attr] = v
			}
		}
	}
	if c.Status == core.StatusActive {
		first, last, attrs = missingDetails(c, cleanSubmission{FirstName: first, LastName: last, Attrs: attrs})
	}
	if err := s.updateDetails(ctx, tx, contactID, first, last, attrs); err != nil {
		return err
	}
	if len(tags) > 0 {
		if err := s.store.AddTags(ctx, tx, contactID, tags, formSource(p.PublicID)); err != nil {
			return err
		}
	}
	return s.enroll(ctx, tx, p.PublicID, sequenceID, contactID)
}

func cutAttr(key string) (string, bool) {
	if len(key) <= len(AttrPrefix) || key[:len(AttrPrefix)] != AttrPrefix {
		return "", false
	}
	return key[len(AttrPrefix):], true
}

// lookupToken verifies token and returns its open pending opt-in.
func (s *Service) lookupToken(ctx context.Context, q db.Querier, token string) (pendingOptin, error) {
	var (
		pl      tokens.Payload
		purpose tokens.Purpose
		err     error
	)
	for _, purpose = range []tokens.Purpose{tokens.PurposeOptin, tokens.PurposeResubscribe} {
		if pl, err = s.signer.Verify(purpose, token); !errors.Is(err, tokens.ErrInvalid) {
			break
		}
	}
	switch {
	case errors.Is(err, tokens.ErrExpired):
		return pendingOptin{}, ErrLinkExpired
	case err != nil:
		return pendingOptin{}, ErrLinkInvalid
	}
	rowID, _, _ := strings.Cut(pl.Extra, ".") // "<pending opt-in id>.<nonce>"
	id, err := strconv.ParseInt(rowID, 10, 64)
	if err != nil || id <= 0 {
		return pendingOptin{}, ErrLinkInvalid
	}
	p, err := getPendingOptin(ctx, q, id)
	if errors.Is(err, core.ErrNotFound) {
		return pendingOptin{}, ErrLinkExpired // deleted by the expiry worker
	}
	if err != nil {
		return pendingOptin{}, err
	}
	wantKind := KindOptin
	if purpose == tokens.PurposeResubscribe {
		wantKind = KindResubscribe
	}
	switch {
	case p.ContactID != pl.ContactID || p.Kind != wantKind:
		return pendingOptin{}, ErrLinkInvalid
	case p.Confirmed:
		return pendingOptin{}, ErrLinkUsed
	case p.TokenHash != tokens.Hash(token):
		return pendingOptin{}, ErrLinkSuperseded
	case !s.clock.Now().Before(p.ExpiresAt):
		return pendingOptin{}, ErrLinkExpired
	}
	return p, nil
}

func getPendingOptin(ctx context.Context, q db.Querier, id int64) (pendingOptin, error) {
	var (
		p                pendingOptin
		formID           sql.NullInt64
		payload, expires string
		confirmed        sql.NullString
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, form_id, contact_id, kind, payload, token_hash, consent_text, expires_at, confirmed_at
		FROM pending_optins WHERE id = ?`, id).
		Scan(&p.ID, &formID, &p.ContactID, &p.Kind, &payload, &p.TokenHash, &p.ConsentText, &expires, &confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return pendingOptin{}, fmt.Errorf("pending opt-in %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return pendingOptin{}, fmt.Errorf("read pending opt-in %d: %w", id, err)
	}
	p.FormID, p.Confirmed = formID.Int64, confirmed.Valid
	if err := db.UnmarshalJSON(payload, &p.Payload); err != nil {
		return pendingOptin{}, err
	}
	if p.ExpiresAt, err = db.ParseTime(expires); err != nil {
		return pendingOptin{}, err
	}
	return p, nil
}
