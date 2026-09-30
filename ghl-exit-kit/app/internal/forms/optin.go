package forms

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/outbox"
	"mailroom/internal/tokens"
)

// optinPayload is what a confirmation applies (pending_optins.payload):
// the submitted fields and the form's public ID for the consent and
// enrolment source (the form may be deleted before the click). Tags and
// the sequence are read from the form at confirmation time; TagIDs (and,
// for payloads without them, Tags) and SequenceID, the form's settings at
// sign-up, are only used once the form is gone. See signupSettings.
type optinPayload struct {
	PublicID   string            `json:"public_id"`
	FormName   string            `json:"form_name"`
	Fields     map[string]string `json:"fields"`
	Tags       []string          `json:"tags"`
	TagIDs     []int64           `json:"tag_ids,omitempty"`
	SequenceID int64             `json:"sequence_id,omitempty"`
}

func newPayload(rec record, in cleanSubmission) optinPayload {
	return optinPayload{
		PublicID: rec.PublicID, FormName: rec.Name, Fields: in.Fields, Tags: rec.Tags, TagIDs: rec.TagIDs,
		SequenceID: sequenceOf(rec),
	}
}

// kindTemplate returns the system template and token purpose of a kind.
func kindTemplate(kind string) (string, tokens.Purpose) {
	if kind == KindResubscribe {
		return core.TemplateResubscribeConfirm, tokens.PurposeResubscribe
	}
	return core.TemplateOptinConfirm, tokens.PurposeOptin
}

// requestConfirmation creates or refreshes the contact's open pending
// opt-in of the given kind and sends the confirmation email when
// sendAllowed permits it. An open pending opt-in is refreshed (its
// payload, ip, user agent and consent text replaced) only together with a
// new email, whose link supersedes the old one: when no email may go out,
// it is left exactly as it is, so a later submission can't change what the
// link the contact already has applies. A resend never extends the
// pending opt-in: it and its new link still expire OptinTTL after the
// first request, so repeated submissions can't keep a sign-up alive. The
// contact itself doesn't change.
func (s *Service) requestConfirmation(ctx context.Context, tx *sql.Tx, kind string, rec record, c core.Contact, in cleanSubmission) error {
	now := s.clock.Now()
	payload, err := db.MarshalJSON(newPayload(rec, in))
	if err != nil {
		return err
	}
	var (
		rowID   int64
		expires string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, expires_at FROM pending_optins
		WHERE contact_id = ? AND kind = ? AND confirmed_at IS NULL AND expires_at > ?
		ORDER BY id DESC LIMIT 1`, c.ID, kind, db.FormatTime(now)).Scan(&rowID, &expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("find pending opt-in: %w", err)
	}
	send, err := s.sendAllowed(ctx, tx, kind, rec, c, now)
	if err != nil {
		return err
	}
	expiresAt := now.Add(OptinTTL).Truncate(time.Second)
	if rowID != 0 {
		if !send {
			return nil // keep the open opt-in and its link as they are
		}
		if expiresAt, err = db.ParseTime(expires); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE pending_optins SET form_id = ?, payload = ?, ip = ?, user_agent = ?, consent_text = ? WHERE id = ?`,
			rec.ID, payload, in.IP, in.UserAgent, rec.ConsentText, rowID); err != nil {
			return fmt.Errorf("refresh pending opt-in: %w", err)
		}
	} else {
		placeholder, err := randomHex(16)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO pending_optins (form_id, contact_id, kind, payload, token_hash, ip, user_agent, consent_text, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			rec.ID, c.ID, kind, payload, "pending:"+placeholder, in.IP, in.UserAgent, rec.ConsentText,
			db.FormatTime(now), db.FormatTime(expiresAt)).Scan(&rowID); err != nil {
			return fmt.Errorf("insert pending opt-in: %w", err)
		}
	}
	if !send {
		return nil
	}

	template, purpose := kindTemplate(kind)
	// A nonce keeps every email's link distinct (a resend supersedes the
	// previous link while the expiry stays the same).
	nonce, err := randomHex(8)
	if err != nil {
		return err
	}
	token, err := s.signer.Sign(purpose, tokens.Payload{
		ContactID: c.ID, Extra: strconv.FormatInt(rowID, 10) + "." + nonce, ExpiresAt: expiresAt,
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pending_optins SET token_hash = ? WHERE id = ?`, tokens.Hash(token), rowID); err != nil {
		return fmt.Errorf("store opt-in token: %w", err)
	}
	if _, _, err := outbox.Enqueue(ctx, tx, s.clock, outbox.MessageSpec{
		Kind: core.KindOptin, ContactID: c.ID, ToEmail: c.Email, SystemTemplate: template,
		Data: map[string]any{"confirm_url": s.confirmURL(token), "form_name": rec.Name},
	}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO confirmation_emails (email_hash, form_id, kind, queued_at) VALUES (?, ?, ?, ?)`,
		core.EmailHash(c.Email), rec.ID, kind, db.FormatTime(now)); err != nil {
		return fmt.Errorf("log confirmation email: %w", err)
	}
	if kind == KindOptin {
		return s.store.RecordConsent(ctx, tx, contactstore.ConsentEvent{
			ContactID: c.ID, Type: contactstore.ConsentOptinRequested, Source: formSource(rec.PublicID), FormID: rec.ID,
			IP: in.IP, UserAgent: in.UserAgent, ConsentText: rec.ConsentText,
		})
	}
	return nil
}

// sendAllowed reports whether another confirmation email may go to the
// contact now. Per address (by its hash in confirmation_emails, so the
// count survives the deletion of an expired pending contact): none was
// queued in the last ResendInterval, fewer than MaxConfirmationsPerDay in
// the last 24 hours and fewer than MaxConfirmationsPerMonth in the last
// ConfirmationWindow; for a resubscribe, none since the contact's last
// status change within ResubscribeCooldown. Then the hourly caps of all
// forms (MaxConfirmationsPerHour) and of this form
// (MaxFormConfirmationsPerHour); reaching one alerts the owner. The public
// form can so email an address only a few times, however often it is
// submitted, and can't be used to spray email at many addresses.
func (s *Service) sendAllowed(ctx context.Context, tx *sql.Tx, kind string, rec record, c core.Contact, now time.Time) (bool, error) {
	var (
		last               sql.NullString
		day, month, resubs int
		since              = now.Add(-ResubscribeCooldown)
	)
	if c.StatusChangedAt.After(since) {
		since = c.StatusChangedAt
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT max(queued_at),
		       count(*) FILTER (WHERE queued_at > ?),
		       count(*),
		       count(*) FILTER (WHERE kind = ? AND queued_at >= ?)
		FROM confirmation_emails
		WHERE email_hash = ? AND queued_at > ?`,
		db.FormatTime(now.Add(-24*time.Hour)), KindResubscribe, db.FormatTime(since),
		core.EmailHash(c.Email), db.FormatTime(now.Add(-ConfirmationWindow))).Scan(&last, &day, &month, &resubs); err != nil {
		return false, fmt.Errorf("find last confirmation emails: %w", err)
	}
	if last.Valid {
		if day >= MaxConfirmationsPerDay || month >= MaxConfirmationsPerMonth {
			return false, nil
		}
		if kind == KindResubscribe && resubs > 0 {
			return false, nil
		}
		t, err := db.ParseTime(last.String)
		if err != nil {
			return false, err
		}
		if now.Before(t.Add(ResendInterval)) {
			return false, nil
		}
	}
	return s.underHourlyCaps(ctx, tx, rec, now)
}

// underHourlyCaps reports whether the confirmation emails queued in the
// last hour, of all forms and of rec, are below MaxConfirmationsPerHour
// and MaxFormConfirmationsPerHour. When a cap is reached, the owner is
// alerted (de-duplicated by the alerter).
func (s *Service) underHourlyCaps(ctx context.Context, tx *sql.Tx, rec record, now time.Time) (bool, error) {
	var all, form int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*), count(*) FILTER (WHERE form_id = ?)
		FROM confirmation_emails WHERE queued_at > ?`,
		rec.ID, db.FormatTime(now.Add(-time.Hour))).Scan(&all, &form); err != nil {
		return false, fmt.Errorf("count confirmation emails: %w", err)
	}
	var key, title, body string
	switch {
	case all >= MaxConfirmationsPerHour:
		key, title = AlertOptinCap, "Sign-up confirmation emails are capped"
		body = fmt.Sprintf("Your sign-up forms asked for more than %d confirmation emails within an hour. "+
			"Mailroom stopped sending them for now, so the forms can't be used to send mass email from your domain. "+
			"Sign-ups are still recorded; confirmation emails go out again once the hour has passed. "+
			"If this is not a real campaign, someone may be abusing a form: check the recent submissions in Forms.", MaxConfirmationsPerHour)
	case form >= MaxFormConfirmationsPerHour:
		key, title = AlertOptinCap+":form:"+strconv.FormatInt(rec.ID, 10), "Sign-up confirmation emails of a form are capped"
		body = fmt.Sprintf("The form %q asked for more than %d confirmation emails within an hour. "+
			"Mailroom stopped sending them for this form for now, so it can't be used to send mass email from your domain. "+
			"Sign-ups are still recorded; confirmation emails go out again once the hour has passed. "+
			"If this is not a real campaign, someone may be abusing the form: check its recent submissions in Forms.", rec.Name, MaxFormConfirmationsPerHour)
	default:
		return true, nil
	}
	s.log.WarnContext(ctx, "forms: confirmation email cap reached", "form_id", rec.ID, "all", all, "form", form)
	if s.alerter != nil {
		if err := s.alerter.Alert(ctx, tx, key, title, body); err != nil {
			return false, err
		}
	}
	return false, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("forms: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
