package sending

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func parseMessage(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse message: %v\n%s", err, raw)
	}
	return m
}

// parts returns the decoded parts of a multipart/alternative message by
// content type.
func parts(t *testing.T, m *mail.Message) map[string]string {
	t.Helper()
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q: %v", m.Header.Get("Content-Type"), err)
	}
	r := multipart.NewReader(m.Body, params["boundary"])
	out := map[string]string{}
	var order []string
	for {
		p, err := r.NextPart() // decodes quoted-printable
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		ct := p.Header.Get("Content-Type")
		out[ct] = string(b)
		order = append(order, ct)
	}
	if len(order) != 2 || order[0] != "text/plain; charset=UTF-8" || order[1] != "text/html; charset=UTF-8" {
		t.Fatalf("parts = %v", order)
	}
	return out
}

func decodeHeader(t *testing.T, v string) string {
	t.Helper()
	s, err := new(mime.WordDecoder).DecodeHeader(v)
	if err != nil {
		t.Fatalf("decode %q: %v", v, err)
	}
	return s
}

func TestBuildMIME(t *testing.T) {
	date := time.Date(2026, 3, 2, 10, 30, 0, 0, time.UTC)
	long := strings.Repeat("Ünïcödé wörds in a very long subject line ", 8)
	tests := []struct {
		name        string
		in          mimeMessage
		wantFrom    string // decoded display name
		wantSubject string
	}{
		{
			name: "plain marketing",
			in: mimeMessage{FromName: "Example News", From: "news@example.com", To: "alice@example.org", ReplyTo: "hello@example.com",
				Subject: "October news", ListUnsubscribe: "https://newsletter.example.com/u/abc", FeedbackID: "b7:broadcast:mailroom:example.com"},
			wantFrom: "Example News", wantSubject: "October news",
		},
		{
			name: "non-ASCII name and subject",
			in: mimeMessage{FromName: "Café Ünïcode", From: "news@example.com", To: "bob@example.org",
				Subject: "Grüße aus Köln ☕"},
			wantFrom: "Café Ünïcode", wantSubject: "Grüße aus Köln ☕",
		},
		{
			name: "header injection",
			in: mimeMessage{FromName: "Evil\r\nBcc: victim@example.net", From: "news@example.com", To: "bob@example.org\r\nX-Injected: 1",
				Subject: "Hi\r\nBcc: victim@example.net\r\n\r\nbody", ListUnsubscribe: "https://x.example.com/u/a>\r\nX-Evil: 1",
				FeedbackID: "a:b\nX-Evil: 2"},
			wantFrom: "Evil  Bcc: victim@example.net", wantSubject: "Hi  Bcc: victim@example.net    body",
		},
		{
			name:     "long non-ASCII subject is folded",
			in:       mimeMessage{FromName: "N", From: "news@example.com", To: "bob@example.org", Subject: long},
			wantFrom: "N", wantSubject: long,
		},
		{
			name:     "long ASCII subject is folded",
			in:       mimeMessage{FromName: "N", From: "news@example.com", To: "bob@example.org", Subject: strings.Repeat("plain words ", 30)},
			wantFrom: "N", wantSubject: strings.TrimSpace(strings.Repeat("plain words ", 30)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.Date = date
			tt.in.MessageID = "mr.42.00ff00ff00ff00ff@example.com"
			tt.in.Text = "Hello Ünïcode\nSecond line = with equals and a long line " + strings.Repeat("x", 120)
			tt.in.HTML = "<p>Hello Ünïcode</p>\r\n<p>" + strings.Repeat("y", 100) + "</p>"
			raw, err := buildMIME(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(raw), "\r\n") {
				if len(line) > 998 {
					t.Fatalf("line %d is %d bytes", i, len(line))
				}
				if strings.ContainsAny(line, "\r\n") {
					t.Fatalf("bare CR/LF in line %d", i)
				}
			}
			head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
			for _, l := range strings.Split(head, "\r\n") {
				if strings.HasPrefix(l, "Bcc:") || strings.HasPrefix(l, "X-Injected") || strings.HasPrefix(l, "X-Evil") {
					t.Fatalf("injected header line %q", l)
				}
				if tt.name == "long ASCII subject is folded" && len(l) > maxLine {
					t.Fatalf("header line longer than %d: %q", maxLine, l)
				}
			}
			m := parseMessage(t, raw)
			h := m.Header
			from, err := mail.ParseAddress(h.Get("From"))
			if err != nil || from.Name != tt.wantFrom || from.Address != "news@example.com" {
				t.Fatalf("From = %q -> %+v %v", h.Get("From"), from, err)
			}
			if got := decodeHeader(t, h.Get("Subject")); got != tt.wantSubject {
				t.Fatalf("Subject = %q, want %q", got, tt.wantSubject)
			}
			if h.Get("MIME-Version") != "1.0" || h.Get("Message-ID") != "<mr.42.00ff00ff00ff00ff@example.com>" {
				t.Fatalf("headers = %v", h)
			}
			if d, err := h.Date(); err != nil || !d.Equal(date) {
				t.Fatalf("Date = %v %v", d, err)
			}
			if _, err := mail.ParseAddress(h.Get("To")); err != nil && tt.name != "header injection" {
				t.Fatalf("To = %q: %v", h.Get("To"), err)
			}
			if tt.in.ReplyTo != "" && h.Get("Reply-To") != "<hello@example.com>" {
				t.Fatalf("Reply-To = %q", h.Get("Reply-To"))
			}
			if tt.in.ListUnsubscribe == "" && (h.Get("List-Unsubscribe") != "" || h.Get("List-Unsubscribe-Post") != "") {
				t.Fatal("list headers on a non-marketing message")
			}
			if tt.in.ListUnsubscribe != "" {
				lu := h.Get("List-Unsubscribe")
				if !strings.HasPrefix(lu, "<https://") || strings.Count(lu, "<") != 1 || strings.Count(lu, ">") != 1 || !strings.HasSuffix(lu, ">") {
					t.Fatalf("List-Unsubscribe = %q", lu)
				}
				if h.Get("List-Unsubscribe-Post") != "List-Unsubscribe=One-Click" {
					t.Fatal("List-Unsubscribe-Post missing")
				}
			}
			for k := range h {
				switch k {
				case "Bcc", "X-Injected", "X-Evil":
					t.Fatalf("injected header %s", k)
				}
			}
			p := parts(t, m)
			if want := strings.ReplaceAll(tt.in.Text, "\n", "\r\n"); p["text/plain; charset=UTF-8"] != want {
				t.Fatalf("text = %q, want %q", p["text/plain; charset=UTF-8"], want)
			}
			if p["text/html; charset=UTF-8"] != tt.in.HTML {
				t.Fatalf("html = %q", p["text/html; charset=UTF-8"])
			}
		})
	}
}

func TestNewMessageID(t *testing.T) {
	a, err := newMessageID(7, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newMessageID(7, "example.com")
	if a == b || !strings.HasPrefix(a, "mr.7.") || !strings.HasSuffix(a, "@example.com") || len(a) != len("mr.7.")+16+len("@example.com") {
		t.Fatalf("ids %q %q", a, b)
	}
}

func TestFeedbackID(t *testing.T) {
	tests := []struct {
		m    message
		want string
	}{
		{message{Kind: "broadcast", BroadcastID: 3}, "b3:broadcast:mailroom:example.com"},
		{message{Kind: "sequence", StepID: 9, EnrollmentID: 2}, "s9:sequence:mailroom:example.com"},
		{message{Kind: "alert"}, "alert:alert:mailroom:example.com"},
	}
	for _, tt := range tests {
		if got := feedbackID(tt.m, "example.com"); got != tt.want {
			t.Errorf("feedbackID(%+v) = %q, want %q", tt.m, got, tt.want)
		}
	}
}
