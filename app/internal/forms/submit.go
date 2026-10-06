package forms

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
)

// SubmissionInput is one public sign-up, already parsed from the request.
type SubmissionInput struct {
	// Values holds the submitted values by field name: "email",
	// "first_name", "last_name", "attr.<key>". Names the form doesn't have
	// are ignored.
	Values    map[string]string
	IP        string
	UserAgent string
}

// SubmitResult is what a submission did. It is never shown to the
// visitor (F-7): every outcome gets the same response.
type SubmitResult struct {
	Outcome   string
	ContactID int64
}

// FieldError is a problem with one submitted value, shown next to it.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// cleanSubmission is a validated submission.
type cleanSubmission struct {
	Email     string
	FirstName *string // nil = not submitted (or empty)
	LastName  *string
	Attrs     map[string]any // only non-empty values
	Fields    map[string]string
	IP        string
	UserAgent string
}

// ValidateSubmission checks a submission against the form: a valid email
// and every required field. It returns the problems in form order (email
// first); none means the submission is valid.
func ValidateSubmission(f Form, values map[string]string) []FieldError {
	_, errs := validateSubmission(f, SubmissionInput{Values: values})
	return errs
}

func validateSubmission(f Form, in SubmissionInput) (cleanSubmission, []FieldError) {
	var (
		errs     []FieldError
		lastAttr FormField
	)
	c := cleanSubmission{Attrs: map[string]any{}, Fields: map[string]string{}, IP: in.IP, UserAgent: truncateUserAgent(in.UserAgent)}
	rawEmail := strings.TrimSpace(in.Values[FieldEmail])
	email, err := core.NormalizeEmail(rawEmail)
	switch {
	case rawEmail == "":
		errs = append(errs, FieldError{FieldEmail, "Please enter your email address."})
	case err != nil:
		errs = append(errs, FieldError{FieldEmail, "Please enter a valid email address."})
	default:
		c.Email = email
	}
	for _, fd := range f.Fields {
		v := strings.TrimSpace(in.Values[fd.Key])
		if v == "" {
			if fd.Required {
				errs = append(errs, FieldError{fd.Key, "Please fill in " + fd.Label + "."})
			}
			continue
		}
		switch fd.Key {
		case FieldFirstName, FieldLastName:
			name, err := contactstore.ValidateName(fd.Key, v)
			if err != nil {
				errs = append(errs, FieldError{fd.Key, fd.Label + " is too long or contains invalid characters."})
				continue
			}
			if fd.Key == FieldFirstName {
				c.FirstName = &name
			} else {
				c.LastName = &name
			}
		default:
			if utf8.RuneCountInString(v) > MaxValueLength || !utf8.ValidString(v) || hasControl(v) {
				errs = append(errs, FieldError{fd.Key, fmt.Sprintf("%s must be at most %d characters without control characters.", fd.Label, MaxValueLength)})
				continue
			}
			c.Attrs[strings.TrimPrefix(fd.Key, AttrPrefix)] = v
			lastAttr = fd
		}
		c.Fields[fd.Key] = v
	}
	// Each value fits, but together they must fit a contact's attributes;
	// checked here so the store never refuses a submission later, which
	// would answer differently for new and existing addresses (F-7).
	if len(c.Attrs) > 0 {
		if err := contactstore.ValidateAttributes(c.Attrs); err != nil {
			errs = append(errs, FieldError{lastAttr.Key, "Your answers are too long together. Please shorten " + lastAttr.Label + " or another answer."})
		}
	}
	return c, errs
}

// truncateUserAgent bounds a user agent to MaxUserAgentLength bytes of
// valid UTF-8.
func truncateUserAgent(ua string) string {
	if len(ua) <= MaxUserAgentLength {
		return ua
	}
	return strings.ToValidUTF8(ua[:MaxUserAgentLength], "")
}

// Submit processes a validated public submission of the form with the
// given ID in one transaction and records its outcome (F-2, F-6, F-7):
//
//   - a suppressed (erased or blocked) address, or an existing bounced,
//     complained or manually suppressed contact: nothing changes (rejected);
//   - an existing active contact on a double opt-in form: nothing changes
//     until a confirmation is clicked, like a new address (optin_pending;
//     F-6: an unauthenticated submission can't enrol a subscriber, fire tag
//     triggers or write consent for them);
//   - an existing active contact on a single opt-in form: names and
//     attributes the contact doesn't have yet are filled in (stored values
//     are never overwritten from the public form), tags added, a signup
//     consent event recorded and the sequence started (updated);
//   - an existing unsubscribed contact: nothing changes until a
//     resubscribe confirmation is clicked (resubscribe_pending);
//   - an existing pending contact: its opt-in is refreshed and the
//     confirmation re-sent, at most once per ResendInterval (optin_pending);
//   - a new address on a double opt-in form: a pending contact and a
//     confirmation email (optin_pending);
//   - a new address otherwise: an active contact with tags, consent and
//     enrolment (created).
//
// Validation problems are returned as a *core.ValidationError for the
// first problem; use ValidateSubmission for all of them.
func (s *Service) Submit(ctx context.Context, formID int64, in SubmissionInput) (SubmitResult, error) {
	rec, err := s.getRecord(ctx, s.db.Read, `id = ?`, formID)
	if err != nil {
		return SubmitResult{}, err
	}
	c, errs := validateSubmission(rec.Form, in)
	if len(errs) > 0 {
		return SubmitResult{}, core.Invalid(errs[0].Field, "%s", errs[0].Message)
	}
	return s.submit(ctx, formID, c)
}

func (s *Service) submit(ctx context.Context, formID int64, in cleanSubmission) (SubmitResult, error) {
	var res SubmitResult
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		res = SubmitResult{}
		// Re-read the form in the transaction: tags and sequence are read
		// at use time (tags may have been renamed or merged).
		rec, err := s.getRecord(ctx, tx, `id = ?`, formID)
		if err != nil {
			return err
		}
		suppressed, err := s.store.IsSuppressed(ctx, tx, in.Email)
		if err != nil {
			return err
		}
		existing, lookupErr := s.store.GetByEmail(ctx, tx, in.Email)
		found := lookupErr == nil
		if lookupErr != nil && !errors.Is(lookupErr, core.ErrNotFound) {
			return lookupErr
		}
		if found {
			res.ContactID = existing.ID
		}
		switch {
		case suppressed:
			res.Outcome = OutcomeRejected
		case found:
			res.Outcome, err = s.submitExisting(ctx, tx, rec, existing, in)
		case rec.DoubleOptin:
			res.ContactID, err = s.createPending(ctx, tx, rec, in)
			res.Outcome = OutcomeOptinPending
		default:
			res.ContactID, err = s.createActive(ctx, tx, rec, in)
			res.Outcome = OutcomeCreated
		}
		if err != nil {
			return err
		}
		return s.recordSubmission(ctx, tx, rec.ID, res.ContactID, res.Outcome)
	})
	if err != nil {
		return SubmitResult{}, err
	}
	return res, nil
}

// submitExisting handles a submission for an address that has a contact.
func (s *Service) submitExisting(ctx context.Context, tx *sql.Tx, rec record, c core.Contact, in cleanSubmission) (string, error) {
	switch c.Status {
	case core.StatusActive:
		if rec.DoubleOptin {
			return OutcomeOptinPending, s.requestConfirmation(ctx, tx, KindOptin, rec, c, in)
		}
		first, last, attrs := missingDetails(c, in)
		if err := s.updateDetails(ctx, tx, c.ID, first, last, attrs); err != nil {
			return "", err
		}
		if err := s.applySignup(ctx, tx, rec, c.ID, in); err != nil {
			return "", err
		}
		return OutcomeUpdated, nil
	case core.StatusUnsubscribed:
		// F-7: only a confirmed resubscribe reactivates the contact.
		return OutcomeResubscribePending, s.requestConfirmation(ctx, tx, KindResubscribe, rec, c, in)
	case core.StatusPending:
		return OutcomeOptinPending, s.requestConfirmation(ctx, tx, KindOptin, rec, c, in)
	}
	// bounced, complained, suppressed_manual: silently rejected (F-7).
	return OutcomeRejected, nil
}

// createActive creates a contact for a single opt-in sign-up with the
// form's tags, a signup consent event and the sequence enrolment.
func (s *Service) createActive(ctx context.Context, tx *sql.Tx, rec record, in cleanSubmission) (int64, error) {
	c, err := s.store.Create(ctx, tx, contactstore.CreateInput{
		Email: in.Email, FirstName: deref(in.FirstName), LastName: deref(in.LastName), Attributes: in.Attrs,
		Tags: rec.Tags, Status: core.StatusActive, Source: formSource(rec.PublicID),
	})
	if err != nil {
		return 0, err
	}
	if err := s.store.RecordConsent(ctx, tx, s.signupConsent(c.ID, rec, in)); err != nil {
		return 0, err
	}
	return c.ID, s.enroll(ctx, tx, rec.PublicID, sequenceOf(rec), c.ID)
}

// createPending creates a pending contact for a double opt-in sign-up and
// sends the confirmation. Tags and the sequence wait for the click.
func (s *Service) createPending(ctx context.Context, tx *sql.Tx, rec record, in cleanSubmission) (int64, error) {
	c, err := s.store.Create(ctx, tx, contactstore.CreateInput{
		Email: in.Email, FirstName: deref(in.FirstName), LastName: deref(in.LastName), Attributes: in.Attrs,
		Status: core.StatusPending, Source: formSource(rec.PublicID),
	})
	if err != nil {
		return 0, err
	}
	return c.ID, s.requestConfirmation(ctx, tx, KindOptin, rec, c, in)
}

// applySignup adds the form's tags, records the signup consent and
// starts the sequence for an active contact.
func (s *Service) applySignup(ctx context.Context, tx *sql.Tx, rec record, contactID int64, in cleanSubmission) error {
	if len(rec.Tags) > 0 {
		if err := s.store.AddTags(ctx, tx, contactID, rec.Tags, formSource(rec.PublicID)); err != nil {
			return err
		}
	}
	if err := s.store.RecordConsent(ctx, tx, s.signupConsent(contactID, rec, in)); err != nil {
		return err
	}
	return s.enroll(ctx, tx, rec.PublicID, sequenceOf(rec), contactID)
}

func (s *Service) signupConsent(contactID int64, rec record, in cleanSubmission) contactstore.ConsentEvent {
	return contactstore.ConsentEvent{
		ContactID: contactID, Type: contactstore.ConsentSignup, Source: formSource(rec.PublicID), FormID: rec.ID,
		IP: in.IP, UserAgent: in.UserAgent, ConsentText: rec.ConsentText,
	}
}

// missingDetails returns the submitted names and attributes that the
// contact doesn't have yet. The public form needs no login, so anyone who
// knows a subscriber's address can submit it: its stored names (merged
// into real mail as {{first_name}}) and attributes must not be
// overwritten from there. A confirmation (Confirm) may overwrite them.
func missingDetails(c core.Contact, in cleanSubmission) (first, last *string, attrs map[string]any) {
	if c.FirstName == "" {
		first = in.FirstName
	}
	if c.LastName == "" {
		last = in.LastName
	}
	attrs = map[string]any{}
	for k, v := range in.Attrs {
		if _, ok := c.Attributes[k]; !ok {
			attrs[k] = v
		}
	}
	return first, last, attrs
}

// updateDetails updates the submitted (non-empty) names and attributes of
// an existing contact; nothing submitted means no change. When the
// submitted attributes don't fit next to the contact's existing ones, the
// attributes are left alone and only the names are updated: a public
// sign-up must not fail (or answer differently, F-7) because of what the
// contact already stores.
func (s *Service) updateDetails(ctx context.Context, tx *sql.Tx, contactID int64, first, last *string, attrs map[string]any) error {
	if first == nil && last == nil && len(attrs) == 0 {
		return nil
	}
	_, err := s.store.Update(ctx, tx, contactID, contactstore.UpdateInput{FirstName: first, LastName: last, Attributes: attrs}, 0)
	var ve *core.ValidationError
	if len(attrs) == 0 || !errors.As(err, &ve) || ve.Field != "attributes" {
		return err
	}
	s.log.WarnContext(ctx, "forms: submitted attributes don't fit the contact's; keeping its attributes",
		"contact_id", contactID, "err", err)
	if first == nil && last == nil {
		return nil
	}
	_, err = s.store.Update(ctx, tx, contactID, contactstore.UpdateInput{FirstName: first, LastName: last}, 0)
	return err
}

// enroll starts sequenceID for the contact with source form:<public_id>.
// It does nothing without an enroller or sequence, or when the sequence
// no longer exists.
func (s *Service) enroll(ctx context.Context, tx *sql.Tx, publicID string, sequenceID, contactID int64) error {
	if s.enroller == nil || sequenceID <= 0 {
		return nil
	}
	ok, err := sequenceExists(ctx, tx, sequenceID)
	if err != nil || !ok {
		return err
	}
	if _, err := s.enroller.Enroll(ctx, tx, sequenceID, contactID, core.EnrollOptions{Source: formSource(publicID)}); err != nil {
		return fmt.Errorf("enrol contact %d in sequence %d: %w", contactID, sequenceID, err)
	}
	return nil
}

// formSource is the tag, consent and enrolment source of a form.
func formSource(publicID string) string { return "form:" + publicID }

func sequenceOf(rec record) int64 {
	if rec.SequenceID == nil {
		return 0
	}
	return *rec.SequenceID
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
