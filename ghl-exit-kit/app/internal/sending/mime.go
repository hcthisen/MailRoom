package sending

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// mimeMessage is everything needed to build one RFC 5322 message.
type mimeMessage struct {
	FromName  string
	From      string // bare address
	To        string // bare address
	ReplyTo   string // bare address; "" = none
	Subject   string
	Date      time.Time
	MessageID string // without angle brackets
	Text      string
	HTML      string
	// ListUnsubscribe is the one-click unsubscribe URL (marketing only);
	// when set, List-Unsubscribe and List-Unsubscribe-Post are added.
	ListUnsubscribe string
	FeedbackID      string // "" = no Feedback-ID header
}

// maxLine is where headers are folded (RFC 5322 recommends 78).
const maxLine = 78

// newMessageID returns "mr.<id>.<random>@<domain>".
func newMessageID(id int64, domain string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sending: message id: %w", err)
	}
	return "mr." + strconv.FormatInt(id, 10) + "." + hex.EncodeToString(b[:]) + "@" + clean(domain), nil
}

// clean strips CR, LF and other control characters from a header value,
// so no input can inject a header or break the message structure.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
}

// encodeWord encodes s as RFC 2047 encoded words when it isn't plain
// printable ASCII.
func encodeWord(s string) string {
	return mime.QEncoding.Encode("UTF-8", s)
}

// formatAddress formats "Name <addr>" (the name RFC 2047-encoded or quoted
// as needed), or a bare address when name is empty.
func formatAddress(name, addr string) string {
	a := mail.Address{Name: strings.TrimSpace(clean(name)), Address: clean(addr)}
	return a.String()
}

// fold writes "Name: value" folded at spaces so lines stay within maxLine
// where possible. Folding only inserts CRLF before existing whitespace,
// so unfolding restores the value exactly.
func fold(buf *bytes.Buffer, name, value string) {
	line := name + ":"
	for _, word := range strings.Split(value, " ") {
		if len(line)+1+len(word) > maxLine && len(line) > len(name)+1 {
			buf.WriteString(line)
			buf.WriteString("\r\n")
			line = ""
		}
		line += " " + word
	}
	buf.WriteString(line)
	buf.WriteString("\r\n")
}

// buildMIME renders m as a multipart/alternative message (text/plain and
// text/html, UTF-8, quoted-printable) with CRLF line endings.
func buildMIME(m mimeMessage) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	boundary := mw.Boundary()

	var hdr bytes.Buffer
	fold(&hdr, "From", formatAddress(m.FromName, m.From))
	fold(&hdr, "To", formatAddress("", m.To))
	if m.ReplyTo != "" {
		fold(&hdr, "Reply-To", formatAddress("", m.ReplyTo))
	}
	fold(&hdr, "Subject", encodeWord(clean(m.Subject)))
	fold(&hdr, "Date", m.Date.Format(time.RFC1123Z))
	fold(&hdr, "Message-ID", "<"+clean(m.MessageID)+">")
	fold(&hdr, "MIME-Version", "1.0")
	if m.ListUnsubscribe != "" {
		u := strings.NewReplacer("<", "%3C", ">", "%3E", " ", "%20").Replace(clean(m.ListUnsubscribe))
		fold(&hdr, "List-Unsubscribe", "<"+u+">")
		fold(&hdr, "List-Unsubscribe-Post", "List-Unsubscribe=One-Click")
	}
	if m.FeedbackID != "" {
		fold(&hdr, "Feedback-ID", clean(m.FeedbackID))
	}
	fold(&hdr, "Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	hdr.WriteString("\r\n")

	for _, part := range []struct{ ctype, body string }{
		{"text/plain; charset=UTF-8", m.Text},
		{"text/html; charset=UTF-8", m.HTML},
	} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", part.ctype)
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, fmt.Errorf("sending: mime part: %w", err)
		}
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write([]byte(normalizeNewlines(part.body))); err != nil {
			return nil, fmt.Errorf("sending: mime encode: %w", err)
		}
		if err := qp.Close(); err != nil {
			return nil, fmt.Errorf("sending: mime encode: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("sending: mime close: %w", err)
	}
	out := make([]byte, 0, hdr.Len()+buf.Len())
	out = append(out, hdr.Bytes()...)
	out = append(out, buf.Bytes()...)
	return out, nil
}

// normalizeNewlines turns CRLF and lone CR into LF; the quoted-printable
// writer then emits CRLF line breaks.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
