package inbound

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// NotificationReceived is the notificationType of SES receipt notifications.
const NotificationReceived = "Received"

// InboundPrefix is the key prefix SES stores received mail under
// (provision.InboundPrefix).
const InboundPrefix = "inbound/"

// receivedNotification is the part of an SES receipt notification
// (S3 action with a topic) that Mailroom reads.
type receivedNotification struct {
	NotificationType string `json:"notificationType"`
	Mail             struct {
		Timestamp     string   `json:"timestamp"`
		Source        string   `json:"source"`
		MessageID     string   `json:"messageId"`
		Destination   []string `json:"destination"`
		CommonHeaders struct {
			From      []string `json:"from"`
			To        []string `json:"to"`
			Cc        []string `json:"cc"`
			ReplyTo   []string `json:"replyTo"`
			MessageID string   `json:"messageId"`
			Subject   string   `json:"subject"`
			Date      string   `json:"date"`
		} `json:"commonHeaders"`
	} `json:"mail"`
	Receipt struct {
		Timestamp    string  `json:"timestamp"`
		SpamVerdict  verdict `json:"spamVerdict"`
		VirusVerdict verdict `json:"virusVerdict"`
		SPFVerdict   verdict `json:"spfVerdict"`
		DKIMVerdict  verdict `json:"dkimVerdict"`
		DMARCVerdict verdict `json:"dmarcVerdict"`
		Action       struct {
			Type       string `json:"type"`
			BucketName string `json:"bucketName"`
			ObjectKey  string `json:"objectKey"`
		} `json:"action"`
	} `json:"receipt"`
}

type verdict struct {
	Status string `json:"status"`
}

// Received implements core.InboundReceiver: it records a pending message
// for an SES receipt notification, in tx and without network I/O. A
// repeated notification for the same stored object changes nothing.
// Notifications for another bucket or prefix, or without an S3 action,
// are ignored (logged); receiving switched off ignores them all.
func (s *Service) Received(ctx context.Context, tx *sql.Tx, snsMessageID, message string) error {
	var n receivedNotification
	if err := json.Unmarshal([]byte(message), &n); err != nil {
		return core.Invalid("message", "SES receipt notification is not JSON: %v", err)
	}
	if n.NotificationType != NotificationReceived {
		return core.Invalid("message", "not an SES receipt notification")
	}
	a := n.Receipt.Action
	switch {
	case !s.Enabled():
		s.log.Warn("inbound: receipt notification while receiving is off (INBOUND_BUCKET unset); ignored", "sns_message_id", snsMessageID)
		return nil
	case !strings.EqualFold(a.Type, "S3") || a.BucketName != s.cfg.InboundBucket:
		s.log.Warn("inbound: receipt notification for another action or bucket; ignored", "type", a.Type, "bucket", a.BucketName)
		return nil
	case !strings.HasPrefix(a.ObjectKey, InboundPrefix) || len(a.ObjectKey) > 1024:
		s.log.Warn("inbound: receipt notification with an unexpected object key; ignored", "key", a.ObjectKey)
		return nil
	}
	ch := n.Mail.CommonHeaders
	fromName, fromEmail := firstAddress(ch.From)
	if fromEmail == "" {
		_, fromEmail = firstAddress([]string{n.Mail.Source})
	}
	received := s.clock.Now()
	if t, err := time.Parse(time.RFC3339Nano, n.Receipt.Timestamp); err == nil {
		received = t.UTC()
	}
	to, _ := db.MarshalJSON(nonNil(addressList(ch.To)))
	cc, _ := db.MarshalJSON(nonNil(addressList(ch.Cc)))
	now := db.FormatTime(s.clock.Now())
	_, err := tx.ExecContext(ctx, `
		INSERT INTO inbound_messages (s3_key, ses_message_id, sns_message_id, from_email, from_name, to_addrs, cc_addrs,
			subject, message_id_header, received_at, spam_verdict, virus_verdict, spf_verdict, dkim_verdict, dmarc_verdict,
			status, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)
		ON CONFLICT (s3_key) DO NOTHING`,
		a.ObjectKey, limit(n.Mail.MessageID, 200), limit(snsMessageID, 200), fromEmail, limit(fromName, 200), to, cc,
		limit(singleLine(ch.Subject), 998), limit(strings.Trim(ch.MessageID, "<> "), 250), db.FormatTime(received),
		verdictOf(n.Receipt.SpamVerdict), verdictOf(n.Receipt.VirusVerdict), verdictOf(n.Receipt.SPFVerdict),
		verdictOf(n.Receipt.DKIMVerdict), verdictOf(n.Receipt.DMARCVerdict), now, now, now)
	if err != nil {
		return fmt.Errorf("inbound: record notification: %w", err)
	}
	return nil
}

func verdictOf(v verdict) string { return limit(strings.ToUpper(strings.TrimSpace(v.Status)), 32) }

// firstAddress parses the first address of a header list: its display
// name and normalised address ("" when it doesn't parse).
func firstAddress(list []string) (name, email string) {
	for _, raw := range list {
		addrs, err := mail.ParseAddressList(raw)
		if err != nil || len(addrs) == 0 {
			if a, err := core.NormalizeEmail(strings.Trim(raw, "<> ")); err == nil {
				return "", a
			}
			continue
		}
		e, err := core.NormalizeEmail(addrs[0].Address)
		if err != nil {
			e = strings.ToLower(limit(addrs[0].Address, 254))
		}
		return limit(singleLine(addrs[0].Name), 200), e
	}
	return "", ""
}

// addressList formats each address of the header values as "Name <addr>"
// or "addr"; values that don't parse are kept as they are.
func addressList(values []string) []string {
	var out []string
	for _, raw := range values {
		addrs, err := mail.ParseAddressList(raw)
		if err != nil {
			if v := limit(singleLine(raw), 500); v != "" {
				out = append(out, v)
			}
			continue
		}
		for _, a := range addrs {
			addr := strings.ToLower(a.Address)
			if a.Name != "" {
				out = append(out, limit(singleLine(a.Name), 200)+" <"+addr+">")
			} else {
				out = append(out, addr)
			}
		}
		if len(out) >= 100 {
			break
		}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// singleLine collapses whitespace (including line breaks) to single spaces.
func singleLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// limit cuts s to at most n bytes, on a rune boundary.
func limit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
