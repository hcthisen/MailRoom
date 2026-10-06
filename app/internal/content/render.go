package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"mailroom/internal/core"
	"mailroom/internal/tokens"
)

// PreviewUnsubscribeURL is the unsubscribe link of previews and other
// messages that must not carry a working one (e.g. test sends).
const PreviewUnsubscribeURL = "#preview"

// Draft is email content that may not be saved yet (live preview).
type Draft struct {
	Subject   string
	Preheader string
	Format    string
	Body      string
}

// Render implements core.Renderer.
//
// Email content (EmailID) is always wrapped in the base layout. A live
// message (Marketing, a MessageID, and not a test send) gets a working
// unsubscribe link BaseURL/u/<token> in the footer and, as the settings
// say, links rewritten for click tracking and an open pixel; it must have
// its Contact. Everything else (previews with MessageID 0, test sends
// marked {"test": true} in Data, even when the caller passes Marketing)
// gets the unsubscribe link "#preview" and no tracking, so a test send
// can never unsubscribe the contact whose data it shows or add to that
// contact's click history.
//
// When Contact is nil and Data carries "contact_id" (test sends with a
// real contact's data), that contact is loaded for its merge values; a
// missing one means fallbacks. SystemTemplate renders a branded system
// email without unsubscribe link or tracking.
//
// Render may write to links (see the package doc): never call it while
// holding a write transaction.
func (s *Service) Render(ctx context.Context, in core.RenderInput) (core.Rendered, error) {
	if in.SystemTemplate != "" {
		return s.renderSystem(ctx, in)
	}
	if in.EmailID <= 0 {
		return core.Rendered{}, invalid("email_id", "is required unless a system template is used")
	}
	em, err := s.GetEmail(ctx, nil, in.EmailID)
	if err != nil {
		return core.Rendered{}, err
	}
	// Test sends (the {"test": true} marker SendTest writes) never get
	// tokens, whatever Marketing says. Nor does a contact taken from Data:
	// it is borrowed for its merge values only (a test showing a real
	// contact's data), so its tokens would act for the wrong person.
	live := in.Marketing && in.MessageID > 0 && !isTestData(in.Data)
	if live && in.Contact == nil {
		return core.Rendered{}, invalid("contact", "a marketing message needs its contact for the unsubscribe link")
	}
	if in.Contact == nil {
		if in.Contact, err = s.contactFromData(ctx, in.Data); err != nil {
			return core.Rendered{}, err
		}
	}
	return s.renderDraft(ctx, Draft{Subject: em.Subject, Preheader: em.Preheader, Format: em.Format, Body: em.Body}, in, nil, live)
}

// isTestData reports whether message data carries the test-send marker
// SendTest writes ({"test": true}).
func isTestData(data map[string]any) bool {
	v, ok := data["test"].(bool)
	return ok && v
}

// RenderDraft renders unsaved content as a preview (MessageID 0), the way
// Render renders a stored email, with contact's data (nil: fallbacks).
// A non-nil layout is used instead of the stored one (layout preview). It
// validates d and layout first.
func (s *Service) RenderDraft(ctx context.Context, d Draft, contact *core.Contact, layout *LayoutInput) (core.Rendered, error) {
	in, err := ValidateEmail(EmailInput{Subject: d.Subject, Preheader: d.Preheader, Format: d.Format, Body: d.Body})
	if err != nil {
		return core.Rendered{}, err
	}
	if layout != nil {
		l, err := ValidateLayout(*layout)
		if err != nil {
			return core.Rendered{}, err
		}
		layout = &l
	}
	return s.renderDraft(ctx, Draft{Subject: in.Subject, Preheader: in.Preheader, Format: in.Format, Body: in.Body},
		core.RenderInput{Contact: contact, Marketing: true}, layout, false)
}

// renderDraft renders d for in; layout nil means the stored layout. Only
// a live message (Render decides) gets a working unsubscribe link and
// tracking; everything else gets PreviewUnsubscribeURL and no tokens.
func (s *Service) renderDraft(ctx context.Context, d Draft, in core.RenderInput, layout *LayoutInput, live bool) (core.Rendered, error) {
	st, err := s.settings.Get(ctx)
	if err != nil {
		return core.Rendered{}, err
	}
	if layout == nil {
		l, err := s.GetLayout(ctx, nil)
		if err != nil {
			return core.Rendered{}, err
		}
		layout = &l.LayoutInput
	}
	var contactID int64
	if in.Contact != nil {
		contactID = in.Contact.ID
	}
	unsub := PreviewUnsubscribeURL
	if live {
		t, err := s.signer.Sign(tokens.PurposeUnsubscribe, tokens.Payload{ContactID: contactID, MessageID: in.MessageID})
		if err != nil {
			return core.Rendered{}, fmt.Errorf("content: sign unsubscribe token: %w", err)
		}
		unsub = s.baseURL + "/u/" + t
	}
	vals := contactValues(in.Contact, unsub)

	subject, err := renderLine("subject", d.Subject, vals)
	if err != nil {
		return core.Rendered{}, err
	}
	preheader, err := renderLine("preheader", d.Preheader, vals)
	if err != nil {
		return core.Rendered{}, err
	}
	body, headStyles, err := s.renderBody(d, vals, layout.BrandColor)
	if err != nil {
		return core.Rendered{}, err
	}
	out, err := wrap(*layout, wrapParams{
		Subject: subject, Preheader: preheader, HeadStyles: headStyles, Content: body,
		Footer: st.CompanyFooter, UnsubscribeURL: unsub,
	})
	if err != nil {
		return core.Rendered{}, err
	}
	// Always run, also without click tracking: it drops NoTrackAttr.
	if out, err = s.rewriteLinks(ctx, out, linkRewrite{
		Track: live && st.Tracking.Clicks, MessageID: in.MessageID, ContactID: contactID, UnsubscribeURL: unsub,
	}); err != nil {
		return core.Rendered{}, err
	}
	if live && st.Tracking.Opens {
		if out, err = s.addOpenPixel(out, in.MessageID, contactID); err != nil {
			return core.Rendered{}, err
		}
	}
	r := core.Rendered{Subject: subject, HTML: out, Text: HTMLToText(out)}
	if in.Marketing {
		r.UnsubscribeURL = unsub
	}
	return r, nil
}

// renderBody renders the body as HTML with the merge values substituted
// by context (placeholderSet.substituteHTML: escaped in text, URL-checked
// or URL-encoded in link and image URLs). Markdown gets inline styles; an
// HTML document is split into its head styles and body.
func (s *Service) renderBody(d Draft, vals values, brandColor string) (body, headStyles string, err error) {
	t, err := parseMerge("body", d.Body, emailVocabulary)
	if err != nil {
		return "", "", err
	}
	src, ph := t.placeholders(d.Body)
	if d.Format == FormatHTML {
		headStyles, body = splitDocument(src)
	} else {
		out, err := s.markdownToHTML(src)
		if err != nil {
			return "", "", err
		}
		body = inlineStyles(out, brandColor)
	}
	return ph.substituteHTML(body, vals), ph.substituteHTML(headStyles, vals), nil
}

// renderLine renders a one-line field (subject, preheader) with raw values
// and makes it a clean single line: merged contact data can't inject
// header line breaks.
func renderLine(field, src string, vals values) (string, error) {
	t, err := parseMerge(field, src, emailVocabulary)
	if err != nil {
		return "", err
	}
	return singleLine(t.execute(vals, false)), nil
}

// singleLine replaces control characters with spaces and collapses runs
// of whitespace.
func singleLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// contactFromData loads the contact named by data["contact_id"], if any.
// A contact that no longer exists yields nil (merge fields fall back).
func (s *Service) contactFromData(ctx context.Context, data map[string]any) (*core.Contact, error) {
	id := dataInt(data, "contact_id")
	if id <= 0 {
		return nil, nil
	}
	c, err := s.contacts.Get(ctx, nil, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// dataInt reads an integer from message data decoded from JSON.
func dataInt(data map[string]any, key string) int64 {
	switch v := data[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// dataString reads a string from message data.
func dataString(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}
