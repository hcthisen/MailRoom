package broadcasts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// EmailContent is the content of a new broadcast email.
type EmailContent struct {
	Subject   string
	Preheader string
	Format    string // "" = markdown
	Body      string
}

// EmailContentPatch changes the non-nil fields of a broadcast's email.
type EmailContentPatch struct {
	Subject   *string
	Preheader *string
	Format    *string
	Body      *string
}

// CreateInput is a new broadcast.
type CreateInput struct {
	Name     string
	Audience BroadcastAudience
	FromName string  // "" = the settings default
	ReplyTo  *string // nil = the settings default; "" = no Reply-To
	Email    EmailContent
}

// UpdateInput changes the non-nil fields of a broadcast.
type UpdateInput struct {
	Name     *string
	Audience *BroadcastAudience
	FromName *string // "" = the settings default
	ReplyTo  *string // "" = no Reply-To
	// DefaultReplyTo resets reply_to to the settings default (ReplyTo must
	// then be nil).
	DefaultReplyTo bool
	Email          *EmailContentPatch
}

// Create stores a new draft broadcast and its email in one transaction.
// With dryRun everything is validated and rolled back.
func (s *Service) Create(ctx context.Context, in CreateInput, dryRun bool) (Broadcast, error) {
	name, err := validName(in.Name)
	if err != nil {
		return Broadcast{}, err
	}
	fromName, err := validFromName(in.FromName)
	if err != nil {
		return Broadcast{}, err
	}
	var replyTo *string
	if in.ReplyTo != nil {
		r, err := s.validReplyTo(*in.ReplyTo)
		if err != nil {
			return Broadcast{}, err
		}
		replyTo = &r
	}
	aud, err := s.resolveAudience(ctx, in.Audience)
	if err != nil {
		return Broadcast{}, err
	}
	audJSON, err := db.MarshalJSON(aud)
	if err != nil {
		return Broadcast{}, err
	}
	var out Broadcast
	err = api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		e, err := s.content.CreateEmail(ctx, tx, content.EmailInput{
			Name: emailName(name), Subject: in.Email.Subject, Preheader: in.Email.Preheader,
			Format: in.Email.Format, Body: in.Email.Body,
		})
		if err != nil {
			return prefixEmailField(err)
		}
		now := s.now()
		var id int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO broadcasts (name, email_id, audience, status, from_name, reply_to, created_at, updated_at)
			VALUES (?, ?, ?, 'draft', ?, ?, ?, ?) RETURNING id`,
			name, e.ID, audJSON, db.NullString(fromName), nullablePtr(replyTo), now, now).Scan(&id); err != nil {
			return fmt.Errorf("broadcasts: create: %w", err)
		}
		if err := s.logAudit(ctx, tx, "broadcast.create", id, map[string]any{
			"name": name, "audience": aud, "email_id": e.ID, "from_name": fromName, "reply_to": replyTo,
		}); err != nil {
			return err
		}
		out, err = s.get(ctx, tx, id)
		return err
	})
	return out, err
}

// Update changes a draft or scheduled broadcast and, with in.Email, its
// email. ifVersion (the ETag version) must match unless it is 0. A change
// while the broadcast is sending, paused, sent or cancelled is a conflict.
func (s *Service) Update(ctx context.Context, id int64, in UpdateInput, ifVersion int64, dryRun bool) (Broadcast, error) {
	if in.DefaultReplyTo && in.ReplyTo != nil {
		return Broadcast{}, core.Invalid("reply_to", "can't be combined with use_default_reply_to")
	}
	var name, fromName, replyTo *string
	if in.Name != nil {
		n, err := validName(*in.Name)
		if err != nil {
			return Broadcast{}, err
		}
		name = &n
	}
	if in.FromName != nil {
		n, err := validFromName(*in.FromName)
		if err != nil {
			return Broadcast{}, err
		}
		fromName = &n
	}
	if in.ReplyTo != nil {
		r, err := s.validReplyTo(*in.ReplyTo)
		if err != nil {
			return Broadcast{}, err
		}
		replyTo = &r
	}
	var aud *BroadcastAudience
	if in.Audience != nil {
		a, err := s.resolveAudience(ctx, *in.Audience)
		if err != nil {
			return Broadcast{}, err
		}
		aud = &a
	}

	var out Broadcast
	err := api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		cur, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		if !editable(cur.Status) {
			return fmt.Errorf("broadcast %d is %s; only draft and scheduled broadcasts can be changed: %w", id, cur.Status, core.ErrConflict)
		}
		next := cur
		changes := map[string]any{}
		if name != nil {
			audit.Diff(changes, "name", cur.Name, *name)
			next.Name = *name
		}
		if fromName != nil {
			audit.Diff(changes, "from_name", ptrValue(cur.FromName), *fromName)
			next.FromName = nil
			if *fromName != "" {
				next.FromName = fromName
			}
		}
		switch {
		case replyTo != nil:
			if cur.ReplyTo == nil || *cur.ReplyTo != *replyTo {
				changes["reply_to"] = audit.Change{From: cur.ReplyTo, To: *replyTo}
			}
			next.ReplyTo = replyTo
		case in.DefaultReplyTo:
			if cur.ReplyTo != nil {
				changes["reply_to"] = audit.Change{From: cur.ReplyTo, To: nil}
			}
			next.ReplyTo = nil
		}
		if aud != nil {
			if cur.Audience.Type != aud.Type || cur.Audience.SegmentID != aud.SegmentID || cur.Audience.SegmentName != aud.SegmentName {
				changes["audience"] = audit.Change{From: cur.Audience, To: *aud}
			}
			next.Audience = *aud
		}
		if in.Email != nil || next.Name != cur.Name {
			if err := s.updateEmail(ctx, tx, cur, next.Name, in.Email, changes); err != nil {
				return err
			}
		}
		if len(changes) == 0 {
			out = cur
			return nil
		}
		audJSON, err := db.MarshalJSON(next.Audience)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE broadcasts SET name = ?, audience = ?, from_name = ?, reply_to = ?, version = version + 1, updated_at = ?
			WHERE id = ?`,
			next.Name, audJSON, nullablePtr(next.FromName), nullablePtr(next.ReplyTo), s.now(), id); err != nil {
			return fmt.Errorf("broadcasts: update %d: %w", id, err)
		}
		if err := s.logAudit(ctx, tx, "broadcast.update", id, changes); err != nil {
			return err
		}
		out, err = s.get(ctx, tx, id)
		return err
	})
	return out, err
}

// updateEmail applies an email patch (and a rename) through the content
// service, recording the changed fields under "email.*".
func (s *Service) updateEmail(ctx context.Context, tx *sql.Tx, cur Broadcast, name string, p *EmailContentPatch, changes map[string]any) error {
	patch := content.EmailPatch{}
	if name != cur.Name {
		n := emailName(name)
		patch.Name = &n
	}
	if p != nil {
		patch.Subject, patch.Preheader, patch.Format, patch.Body = p.Subject, p.Preheader, p.Format, p.Body
	}
	e, err := s.content.UpdateEmail(ctx, tx, cur.Email.ID, patch, 0)
	if err != nil {
		return prefixEmailField(err)
	}
	audit.Diff(changes, "email.subject", cur.Email.Subject, e.Subject)
	audit.Diff(changes, "email.preheader", cur.Email.Preheader, e.Preheader)
	audit.Diff(changes, "email.format", cur.Email.Format, e.Format)
	if e.Body != cur.Email.Body {
		changes["email.body_bytes"] = audit.Change{From: len(cur.Email.Body), To: len(e.Body)}
	}
	return nil
}

// Delete deletes a draft or cancelled broadcast together with its email
// and its unsent (queued or skipped) messages. A broadcast that sent any
// message is kept (conflict), so its delivery history survives. ifVersion
// must match unless it is 0.
func (s *Service) Delete(ctx context.Context, id int64, ifVersion int64) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		if cur.Status != StatusDraft && cur.Status != StatusCancelled {
			return fmt.Errorf("broadcast %d is %s; only draft and cancelled broadcasts can be deleted: %w", id, cur.Status, core.ErrConflict)
		}
		// Messages that left the queue carry delivery history (events,
		// clicks, opens, bounce counts, timelines) that other features
		// rely on; deleting the broadcast would cascade them away.
		var out int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM messages
			WHERE broadcast_id = ? AND (sent_at IS NOT NULL OR status NOT IN ('queued', 'skipped'))`, id).Scan(&out); err != nil {
			return fmt.Errorf("broadcasts: delete %d: %w", id, err)
		}
		if out > 0 {
			return fmt.Errorf("broadcast %d already sent (or is sending) %d message(s); a broadcast that sent anything is kept for its delivery history: %w",
				id, out, core.ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM broadcasts WHERE id = ?`, id); err != nil {
			return fmt.Errorf("broadcasts: delete %d: %w", id, err)
		}
		if err := s.content.DeleteEmail(ctx, tx, cur.Email.ID, 0); err != nil {
			return fmt.Errorf("delete the email of broadcast %d: %w", id, err)
		}
		return s.logAudit(ctx, tx, "broadcast.delete", id, map[string]any{"name": cur.Name, "status": cur.Status})
	})
}

// Duplicate creates a new draft with a copy of the broadcast's email,
// audience and sender (B-9). Any broadcast can be duplicated. The audience
// is resolved again like a new one: a segment audience gets the segment's
// current name, and a segment deleted since is a validation error
// (audience.segment_id), so the copy never shows one segment's name while
// sending to another.
func (s *Service) Duplicate(ctx context.Context, id int64) (Broadcast, error) {
	var out Broadcast
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		src, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		aud, err := s.resolveAudience(ctx, src.Audience)
		if err != nil {
			var ve *core.ValidationError
			if errors.As(err, &ve) && ve.Field == "audience.segment_id" {
				return core.Invalid("audience.segment_id", "can't copy the audience: %s; create a new broadcast and choose its audience", ve.Message)
			}
			return err
		}
		name := copyName(src.Name)
		e, err := s.content.CreateEmail(ctx, tx, content.EmailInput{
			Name: emailName(name), Subject: src.Email.Subject, Preheader: src.Email.Preheader,
			Format: src.Email.Format, Body: src.Email.Body,
		})
		if err != nil {
			return err
		}
		audJSON, err := db.MarshalJSON(aud)
		if err != nil {
			return err
		}
		now := s.now()
		var newID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO broadcasts (name, email_id, audience, status, from_name, reply_to, created_at, updated_at)
			VALUES (?, ?, ?, 'draft', ?, ?, ?, ?) RETURNING id`,
			name, e.ID, audJSON, nullablePtr(src.FromName), nullablePtr(src.ReplyTo), now, now).Scan(&newID); err != nil {
			return fmt.Errorf("broadcasts: duplicate %d: %w", id, err)
		}
		if err := s.logAudit(ctx, tx, "broadcast.duplicate", newID, map[string]any{"from_broadcast_id": id, "name": name, "email_id": e.ID}); err != nil {
			return err
		}
		out, err = s.get(ctx, tx, newID)
		return err
	})
	return out, err
}

// copyName is the name of a duplicate, within MaxNameLength.
func copyName(name string) string {
	const suffix = " (copy)"
	r := []rune(name)
	if max := MaxNameLength - len(suffix); len(r) > max {
		r = r[:max]
	}
	return string(r) + suffix
}

// prefixEmailField moves a content validation error under "email.".
func prefixEmailField(err error) error {
	var ve *core.ValidationError
	if errors.As(err, &ve) && ve.Field != "" {
		return core.Invalid("email."+ve.Field, "%s", ve.Message)
	}
	return err
}

// nullablePtr is SQL NULL for nil, else the string.
func nullablePtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
