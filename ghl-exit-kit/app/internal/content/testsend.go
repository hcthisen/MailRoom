package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/outbox"
)

// MaxTestRecipients is the most addresses one test send may go to (B-7).
const MaxTestRecipients = 5

// SendTest queues a test message (kind test) of the email to each address
// in to (1-5, duplicates removed; empty means the owner's address from the
// settings, S-10). With contactID > 0 the test shows that contact's data:
// the message data carries "contact_id", which Render loads. Test
// messages don't count in stats and carry no working unsubscribe link; the
// sender skips them for suppressed addresses and for addresses whose
// contact is unsubscribed, bounced, complained or suppressed. It
// returns the new message IDs and the normalised addresses, in order.
func (s *Service) SendTest(ctx context.Context, tx *sql.Tx, emailID int64, to []string, contactID int64) ([]int64, []string, error) {
	return s.SendTestAs(ctx, tx, emailID, to, contactID, SenderOverride{})
}

// SenderOverride replaces the settings' From name and Reply-To of test
// messages, e.g. with a broadcast's own sender (B-7). A nil field keeps the
// settings' value; ReplyTo "" means no Reply-To. The sender checks at send
// time that the Reply-To is on the sending domain (D-7).
type SenderOverride struct {
	FromName *string
	ReplyTo  *string
}

// SendTestAs is SendTest with the sender override stored in the messages'
// data (core.DataFromName, core.DataReplyTo), where the sender applies it.
func (s *Service) SendTestAs(ctx context.Context, tx *sql.Tx, emailID int64, to []string, contactID int64, sender SenderOverride) ([]int64, []string, error) {
	addrs, err := s.testRecipients(ctx, to)
	if err != nil {
		return nil, nil, err
	}
	ok, err := s.EmailExists(ctx, tx, emailID)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("email %d: %w", emailID, core.ErrNotFound)
	}
	data := map[string]any{"test": true}
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
		id, _, err := outbox.Enqueue(ctx, tx, s.clock, outbox.MessageSpec{
			Kind: core.KindTest, EmailID: emailID, ToEmail: a, Data: data,
		})
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	changes := map[string]any{"recipients": len(addrs), "message_ids": ids}
	if contactID > 0 {
		changes["contact_id"] = contactID
	}
	if err := audit.Write(ctx, tx, audit.Entry{
		Operation: "email.test_send", TargetType: "email", TargetID: strconv.FormatInt(emailID, 10),
		Changes: changes, At: s.clock.Now(),
	}); err != nil {
		return nil, nil, err
	}
	return ids, addrs, nil
}

// testRecipients normalises and de-duplicates the addresses, defaulting
// to the owner's address.
func (s *Service) testRecipients(ctx context.Context, to []string) ([]string, error) {
	if len(to) == 0 {
		st, err := s.settings.Get(ctx)
		if err != nil {
			return nil, err
		}
		if st.OwnerEmail == "" {
			return nil, invalid("to", "is required: no owner_email is set in the settings")
		}
		to = []string{st.OwnerEmail}
	}
	if len(to) > MaxTestRecipients {
		return nil, invalid("to", "at most %d addresses are allowed", MaxTestRecipients)
	}
	seen := make(map[string]bool, len(to))
	out := make([]string, 0, len(to))
	for i, raw := range to {
		a, err := core.NormalizeEmail(raw)
		if err != nil {
			return nil, invalid(fmt.Sprintf("to[%d]", i), "is not a valid email address")
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out, nil
}
