package ui

// "Start from" on the new-broadcast page (like Mailchimp's "replicate" and
// saved templates): the form is prefilled from an earlier broadcast
// (?from=broadcast:N) or a saved email (?from=email:N).

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/broadcasts"
	"mailroom/internal/content"
	"mailroom/internal/core"
)

// starter is one "Start from" choice.
type starter struct {
	Label, Sub, URL string
	At              time.Time
}

// starterLimit is how many broadcasts and saved emails "Start from" offers.
const starterLimit = 6

// broadcastStarters lists recent broadcasts and saved emails to start from.
func (u *UI) broadcastStarters(ctx context.Context) ([]starter, []starter, error) {
	bs, err := u.d.Broadcasts.List(ctx, broadcasts.ListFilter{Limit: starterLimit})
	if err != nil {
		return nil, nil, err
	}
	var fromB []starter
	for _, b := range bs {
		fromB = append(fromB, starter{Label: b.Name, Sub: b.Email.Subject, At: b.CreatedAt, URL: broadcastsPath + "/new?from=broadcast:" + strconv.FormatInt(b.ID, 10)})
	}
	es, err := u.d.Content.ListEmails(ctx, content.ListFilter{Limit: 200})
	if err != nil {
		return nil, nil, err
	}
	var fromE []starter
	for i := len(es) - 1; i >= 0 && len(fromE) < starterLimit; i-- { // newest first
		e := es[i]
		if e.UsedByBroadcasts > 0 || e.UsedBySteps > 0 {
			continue // a broadcast's or a step's own content: offered elsewhere
		}
		fromE = append(fromE, starter{Label: defaultString(e.Name, e.Subject), Sub: e.Subject, At: e.CreatedAt, URL: broadcastsPath + "/new?from=email:" + strconv.FormatInt(e.ID, 10)})
	}
	return fromB, fromE, nil
}

// prefillFrom fills f from the ?from= source. An unknown source is ignored.
func (u *UI) prefillFrom(ctx context.Context, f *Form, from string) error {
	kind, idStr, ok := strings.Cut(from, ":")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if !ok || err != nil || id <= 0 {
		return nil
	}
	switch kind {
	case "broadcast":
		b, err := u.d.Broadcasts.Get(ctx, id)
		if errors.Is(err, core.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		f.Set("name", "Copy of "+b.Name).Set("audience", audienceValue(b.Audience)).
			Set("subject", b.Email.Subject).Set("preheader", b.Email.Preheader).Set("format", b.Email.Format).Set("body", b.Email.Body)
		if b.FromName != nil {
			f.Set("from_name", *b.FromName)
		}
	case "email":
		e, err := u.d.Content.GetEmail(ctx, nil, id)
		if errors.Is(err, core.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		f.Set("subject", e.Subject).Set("preheader", e.Preheader).Set("format", e.Format).Set("body", e.Body)
	}
	return nil
}
