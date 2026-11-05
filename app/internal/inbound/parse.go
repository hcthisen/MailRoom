package inbound

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"mailroom/internal/core"
)

// Parsing limits. SES accepts messages up to 40 MB; Mailroom keeps the
// bodies and attachments up to these sizes and notes what it left out.
const (
	MaxRawBytes        = 40 << 20
	MaxTextBytes       = 1 << 20
	MaxHTMLBytes       = 2 << 20
	MaxAttachmentBytes = 25 << 20 // all attachments of one message together
	MaxAttachments     = 50
	maxParts           = 200
	maxDepth           = 10
	snippetLength      = 200
)

// parsed is a received message reduced to what Mailroom stores.
type parsed struct {
	FromName, FromEmail string
	To, Cc              []string
	ReplyTo             string
	Subject             string
	MessageID           string
	InReplyTo           string
	References          string
	Date                *time.Time
	Text, HTML          string
	Attachments         []attachment
	Notes               []string // what was left out (limits), shown with the message
	AutoSubmitted       bool     // Auto-Submitted other than "no" (RFC 3834)
}

type attachment struct {
	Filename    string
	ContentType string
	ContentID   string
	Inline      bool
	Data        []byte
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charset.NewReaderLabel}

// decodeHeader decodes RFC 2047 encoded words; undecodable text is kept.
func decodeHeader(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		s = d
	}
	return strings.ToValidUTF8(s, "�")
}

// parseMessage parses a raw RFC 5322 message. Only a message whose
// header can't be read is an error; a broken body part is skipped with a
// note.
func parseMessage(raw []byte) (parsed, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return parsed{}, fmt.Errorf("inbound: read message header: %w", err)
	}
	h := msg.Header
	var p parsed
	addrParser := mail.AddressParser{WordDecoder: wordDecoder}
	if from, err := addrParser.ParseList(h.Get("From")); err == nil && len(from) > 0 {
		p.FromName = limit(singleLine(from[0].Name), 200)
		if e, err := core.NormalizeEmail(from[0].Address); err == nil {
			p.FromEmail = e
		} else {
			p.FromEmail = strings.ToLower(limit(from[0].Address, 254))
		}
	}
	p.To = formatList(addrParser, h.Get("To"))
	p.Cc = formatList(addrParser, h.Get("Cc"))
	if rt, err := addrParser.ParseList(h.Get("Reply-To")); err == nil && len(rt) > 0 {
		if e, err := core.NormalizeEmail(rt[0].Address); err == nil {
			p.ReplyTo = e
		}
	}
	p.Subject = limit(singleLine(decodeHeader(h.Get("Subject"))), 998)
	p.MessageID = limit(strings.Trim(strings.TrimSpace(h.Get("Message-Id")), "<>"), 250)
	p.InReplyTo = limit(strings.Trim(strings.TrimSpace(h.Get("In-Reply-To")), "<>"), 250)
	p.References = limit(singleLine(h.Get("References")), 2000)
	if d, err := h.Date(); err == nil {
		u := d.UTC()
		p.Date = &u
	}
	if v := strings.ToLower(strings.TrimSpace(h.Get("Auto-Submitted"))); v != "" && v != "no" {
		p.AutoSubmitted = true
	}

	w := &walker{p: &p}
	w.part(textprotoHeader(h), msg.Body, 0)
	if p.Text == "" && p.HTML != "" {
		p.Text = htmlToText(p.HTML)
	}
	return p, nil
}

// header is the MIME header of a part (mail.Header and textproto both fit).
type header interface{ Get(string) string }

type textprotoHeader mail.Header

func (h textprotoHeader) Get(k string) string { return mail.Header(h).Get(k) }

// walker collects the bodies and attachments of a message.
type walker struct {
	p        *parsed
	parts    int
	attBytes int
	textSet  bool
	htmlSet  bool
}

func (w *walker) note(format string, args ...any) {
	if len(w.p.Notes) < 20 {
		w.p.Notes = append(w.p.Notes, fmt.Sprintf(format, args...))
	}
}

func (w *walker) part(h header, body io.Reader, depth int) {
	w.parts++
	if w.parts > maxParts || depth > maxDepth {
		if w.parts == maxParts+1 || depth == maxDepth+1 {
			w.note("The message has too many parts; the rest was left out.")
		}
		return
	}
	ctype, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || ctype == "" {
		ctype, params = "text/plain", map[string]string{"charset": "us-ascii"}
	}
	ctype = strings.ToLower(ctype)
	if strings.HasPrefix(ctype, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			w.note("A multipart section had no boundary and was left out.")
			return
		}
		mr := multipart.NewReader(body, boundary)
		for {
			np, err := mr.NextRawPart()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				w.note("A broken part of the message was left out.")
				return
			}
			w.part(np.Header, np, depth+1)
		}
	}

	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	disp = strings.ToLower(disp)
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	filename = decodeHeader(filename)
	data, err := io.ReadAll(io.LimitReader(decodeTransfer(h.Get("Content-Transfer-Encoding"), body), MaxRawBytes))
	if err != nil {
		w.note("A part of the message could not be decoded and was left out.")
		return
	}
	isText := ctype == "text/plain" || ctype == "text/html"
	if isText && disp != "attachment" && filename == "" {
		s := toUTF8(data, params["charset"])
		switch {
		case ctype == "text/plain" && !w.textSet:
			w.textSet = true
			if len(s) > MaxTextBytes {
				s = limit(s, MaxTextBytes)
				w.note("The text was cut at 1 MB.")
			}
			w.p.Text = s
			return
		case ctype == "text/html" && !w.htmlSet:
			w.htmlSet = true
			if len(s) > MaxHTMLBytes {
				w.note("The HTML version was larger than 2 MB and was left out.")
				return
			}
			w.p.HTML = s
			return
		}
	}
	if ctype == "message/rfc822" && filename == "" {
		filename = "forwarded-message.eml"
	}
	if len(w.p.Attachments) >= MaxAttachments {
		w.note("More than %d attachments: the rest were left out.", MaxAttachments)
		return
	}
	if w.attBytes+len(data) > MaxAttachmentBytes {
		w.note("Attachment %q (%s) was left out: the attachments are larger than 25 MB together.", safeFilename(filename, len(w.p.Attachments)+1, ctype), humanSize(len(data)))
		return
	}
	w.attBytes += len(data)
	cid := strings.Trim(strings.TrimSpace(h.Get("Content-Id")), "<>")
	w.p.Attachments = append(w.p.Attachments, attachment{
		Filename:    safeFilename(filename, len(w.p.Attachments)+1, ctype),
		ContentType: limit(ctype, 100),
		ContentID:   limit(cid, 250),
		Inline:      disp != "attachment" && cid != "",
		Data:        data,
	})
}

// decodeTransfer undoes a Content-Transfer-Encoding.
func decodeTransfer(enc string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &base64Cleaner{r: r})
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// base64Cleaner drops the characters base64 bodies may contain besides
// the alphabet (line breaks, spaces), which the decoder would reject.
type base64Cleaner struct{ r io.Reader }

func (c *base64Cleaner) Read(p []byte) (int, error) {
	for {
		n, err := c.r.Read(p)
		j := 0
		for _, b := range p[:n] {
			if b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '+' || b == '/' || b == '=' {
				p[j] = b
				j++
			}
		}
		if j > 0 || err != nil {
			return j, err
		}
	}
}

// toUTF8 converts text in the given charset to valid UTF-8.
func toUTF8(data []byte, cs string) string {
	cs = strings.ToLower(strings.TrimSpace(cs))
	if cs != "" && cs != "utf-8" && cs != "us-ascii" && cs != "utf8" {
		if r, err := charset.NewReaderLabel(cs, bytes.NewReader(data)); err == nil {
			if out, err := io.ReadAll(r); err == nil {
				data = out
			}
		}
	}
	s := string(data)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\x00", "")
}

func formatList(p mail.AddressParser, v string) []string {
	if strings.TrimSpace(v) == "" {
		return []string{}
	}
	addrs, err := p.ParseList(v)
	if err != nil {
		return []string{limit(singleLine(decodeHeader(v)), 500)}
	}
	out := []string{}
	for _, a := range addrs {
		addr := strings.ToLower(a.Address)
		if a.Name != "" {
			out = append(out, limit(singleLine(a.Name), 200)+" <"+addr+">")
		} else {
			out = append(out, addr)
		}
		if len(out) >= 100 {
			break
		}
	}
	return out
}

// safeFilename keeps the base name of a received file name, without
// control or path characters; empty names get "attachment-N" with an
// extension from the type.
func safeFilename(name string, n int, ctype string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" || name == "_" {
		name = fmt.Sprintf("attachment-%d", n)
		if exts, _ := mime.ExtensionsByType(ctype); len(exts) > 0 {
			name += exts[0]
		}
	}
	return limit(name, 200)
}

func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// htmlToText is a plain reading of an HTML body: the text of the visible
// elements with line breaks at block elements, for messages without a
// text part and for snippets.
func htmlToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	skip := 0
	for b.Len() < MaxTextBytes {
		switch z.Next() {
		case html.ErrorToken:
			return tidyText(b.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "head", "title":
				skip++
			case "br", "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "table", "blockquote":
				b.WriteString("\n")
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "head", "title":
				if skip > 0 {
					skip--
				}
			case "p", "div", "h1", "h2", "h3", "h4", "table":
				b.WriteString("\n")
			}
		case html.TextToken:
			if skip == 0 {
				b.WriteString(strings.Join(strings.Fields(string(z.Text())), " "))
				b.WriteString(" ")
			}
		}
	}
	return tidyText(b.String())
}

// tidyText trims lines and collapses runs of blank lines.
func tidyText(s string) string {
	var out []string
	blank := false
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// snippet is the start of the text without quoted lines, on one line.
func snippet(text string) string {
	var b strings.Builder
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, ">") {
			continue
		}
		if strings.HasPrefix(t, "On ") && strings.HasSuffix(t, "wrote:") {
			break
		}
		b.WriteString(t)
		b.WriteString(" ")
		if b.Len() > snippetLength*2 {
			break
		}
	}
	s := singleLine(b.String())
	if utf8.RuneCountInString(s) > snippetLength {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:snippetLength])) + "…"
	}
	return s
}
