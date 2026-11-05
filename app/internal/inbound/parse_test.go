package inbound

import (
	"strings"
	"testing"
)

func TestParseMessage(t *testing.T) {
	raw := "From: =?ISO-8859-1?Q?S=F8ren?= <Soren@Example.ORG>\r\n" +
		"To: a@example.com, \"B\" <b@example.com>\r\n" +
		"Cc: c@example.com\r\n" +
		"Reply-To: reply@example.org\r\n" +
		"Subject: =?UTF-8?B?w4ZibGVyIG9nIHDDpnJlcg==?=\r\n" +
		"Message-ID: <m1@example.org>\r\n" +
		"In-Reply-To: <mr.42.abcd@example.com>\r\n" +
		"Auto-Submitted: auto-replied\r\n" +
		"Content-Type: multipart/related; boundary=r\r\n\r\n" +
		"--r\r\nContent-Type: text/html; charset=windows-1252\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"<p>K=E6re ven</p><img src=3D\"cid:logo@x\">\r\n" +
		"--r\r\nContent-Type: image/png\r\nContent-ID: <logo@x>\r\nContent-Transfer-Encoding: base64\r\n\r\niVBO\r\nRw==\r\n" +
		"--r--\r\n"
	p, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.FromName != "Søren" || p.FromEmail != "soren@example.org" || p.ReplyTo != "reply@example.org" ||
		p.Subject != "Æbler og pærer" || p.MessageID != "m1@example.org" || p.InReplyTo != "mr.42.abcd@example.com" || !p.AutoSubmitted {
		t.Errorf("headers = %+v", p)
	}
	if strings.Join(p.To, "|") != "a@example.com|B <b@example.com>" || strings.Join(p.Cc, "|") != "c@example.com" {
		t.Errorf("to %v cc %v", p.To, p.Cc)
	}
	if !strings.Contains(p.HTML, "Kære ven") || p.Text != "Kære ven" {
		t.Errorf("html %q text %q", p.HTML, p.Text)
	}
	if len(p.Attachments) != 1 || !p.Attachments[0].Inline || p.Attachments[0].ContentID != "logo@x" || p.Attachments[0].Filename != "attachment-1.png" ||
		string(p.Attachments[0].Data) != "\x89PNG" {
		t.Errorf("attachments = %+v", p.Attachments)
	}
	if id, ok := ourMessageID(p.InReplyTo); !ok || id != 42 {
		t.Errorf("ourMessageID = %d %v", id, ok)
	}
}

func TestParseMessagePlainAndBroken(t *testing.T) {
	p, err := parseMessage([]byte("From: x@example.org\r\nSubject: hi\r\n\r\nJust text\r\n> quoted\r\n"))
	if err != nil || p.Text != "Just text\n> quoted\n" || snippet(p.Text) != "Just text" {
		t.Fatalf("p = %+v, %v", p, err)
	}
	// A multipart without boundary is noted, not an error.
	p, err = parseMessage([]byte("From: x@example.org\r\nContent-Type: multipart/mixed\r\n\r\nbody"))
	if err != nil || len(p.Notes) != 1 {
		t.Fatalf("p = %+v, %v", p, err)
	}
	if _, err := parseMessage([]byte("not a message")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestSafeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd":   "passwd",
		`C:\Users\x\a.pdf`:   "a.pdf",
		"evil\x00name.exe":   "evil_name.exe",
		"":                   "attachment-3.pdf",
		"..":                 "attachment-3.pdf",
		"report <final>.pdf": "report _final_.pdf",
	} {
		if got := safeFilename(in, 3, "application/pdf"); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrepareHTML(t *testing.T) {
	src := `<html><head><base href="https://evil.example/"><meta http-equiv="refresh" content="0;url=https://x"></head>` +
		`<body onload="x()"><a href="https://ok.example/">ok</a><a href="javascript:alert(1)">bad</a>` +
		`<img src="cid:Logo@X"><img src="https://pixel.example/p.gif"><table background="https://bg.example/i.png"></table>` +
		`<script>alert(1)</script><form action="/x"><input></form><iframe src="https://x"></iframe></body></html>`
	d := prepareHTML(src, map[string]string{"logo@x": "data:image/png;base64,AA=="}, false)
	for _, bad := range []string{"<base", "refresh", "onload", "javascript:", "pixel.example", "bg.example", "<script", "<form", "<iframe"} {
		if strings.Contains(d.HTML, bad) {
			t.Errorf("display keeps %q: %s", bad, d.HTML)
		}
	}
	if !strings.Contains(d.HTML, `src="data:image/png;base64,AA=="`) || !strings.Contains(d.HTML, `target="_blank"`) ||
		!strings.Contains(d.HTML, `rel="noopener noreferrer"`) || d.BlockedImages != 2 {
		t.Errorf("display = %d %s", d.BlockedImages, d.HTML)
	}
	if d := prepareHTML(src, nil, true); !strings.Contains(d.HTML, "pixel.example") || d.BlockedImages != 0 {
		t.Errorf("show images = %+v", d)
	}
}

func TestReplySubject(t *testing.T) {
	for in, want := range map[string]string{"Hello": "Re: Hello", "RE: Hello": "RE: Hello", "Sv: Hej": "Sv: Hej", "": "Re: your email"} {
		if got := replySubject(in); got != want {
			t.Errorf("replySubject(%q) = %q", in, got)
		}
	}
}
