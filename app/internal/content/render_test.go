package content_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
	"mailroom/internal/tokens"
)

var (
	rxClick = regexp.MustCompile(`href="` + regexp.QuoteMeta(testBaseURL) + `/c/([A-Za-z0-9_.-]+)"`)
	rxUnsub = regexp.MustCompile(regexp.QuoteMeta(testBaseURL) + `/u/([A-Za-z0-9_.-]+)`)
	rxOpen  = regexp.MustCompile(`src="` + regexp.QuoteMeta(testBaseURL) + `/o/([A-Za-z0-9_.-]+)"`)
)

const trackedBody = `# Hello {{first_name | "there"}}

Read [our post](https://example.com/post?a=1&b=2) and [again](https://example.com/post?a=1&b=2).

- [Mail us](mailto:hi@example.com)
- [Call](tel:+441234)
- [Top](#top)
- <a href="https://example.com/private" data-mr-notrack>private</a>
- [Unsubscribe here]({{unsubscribe_url}})

Plain www.example.org autolink.`

func TestRenderMarketing(t *testing.T) {
	f := newFixture(t)
	c := f.contact("alice@example.com", "Alice", map[string]any{"company": "ACME"})
	em := f.createEmail(content.EmailInput{Name: "n", Subject: `Hi {{first_name | "friend"}} from {{attr.company}}`,
		Preheader: "Preview for {{first_name}}", Body: trackedBody})

	r, err := f.svc.Render(f.ctx, core.RenderInput{MessageID: 77, EmailID: em.ID, Contact: &c, ToEmail: c.Email, Marketing: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Subject != "Hi Alice from ACME" {
		t.Fatalf("subject = %q", r.Subject)
	}

	// Unsubscribe: a signed (contact, message) token, in the footer and the result.
	m := rxUnsub.FindStringSubmatch(r.UnsubscribeURL)
	if m == nil {
		t.Fatalf("unsubscribe URL = %q", r.UnsubscribeURL)
	}
	pl, err := f.signer.Verify(tokens.PurposeUnsubscribe, m[1])
	if err != nil || pl.ContactID != c.ID || pl.MessageID != 77 {
		t.Fatalf("unsubscribe token = %+v %v", pl, err)
	}
	if !strings.Contains(r.HTML, `href="`+r.UnsubscribeURL+`"`) {
		t.Fatal("footer must link the unsubscribe URL")
	}
	if n := strings.Count(r.HTML, r.UnsubscribeURL); n != 2 {
		t.Fatalf("unsubscribe URL appears %d times, want 2 (body merge field + footer), untracked", n)
	}

	// Click tracking: http(s) links rewritten, the same URL shares one link row.
	clicks := rxClick.FindAllStringSubmatch(r.HTML, -1)
	if len(clicks) != 3 {
		t.Fatalf("tracked links = %d, want 3 (post twice + autolink):\n%s", len(clicks), r.HTML)
	}
	var linkIDs []int64
	for _, cm := range clicks {
		pl, err := f.signer.Verify(tokens.PurposeClick, cm[1])
		if err != nil || pl.MessageID != 77 || pl.ContactID != c.ID || pl.LinkID == 0 {
			t.Fatalf("click token = %+v %v", pl, err)
		}
		linkIDs = append(linkIDs, pl.LinkID)
	}
	if linkIDs[0] != linkIDs[1] || linkIDs[0] == linkIDs[2] {
		t.Fatalf("link ids = %v", linkIDs)
	}
	var url string
	if err := f.d.Read.QueryRow(`SELECT url FROM links WHERE id = ?`, linkIDs[0]).Scan(&url); err != nil || url != "https://example.com/post?a=1&b=2" {
		t.Fatalf("link url = %q %v", url, err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM links`); n != 2 {
		t.Fatalf("links rows = %d", n)
	}
	for _, keep := range []string{`href="mailto:hi@example.com"`, `href="tel:+441234"`, `href="#top"`, `href="https://example.com/private"`} {
		if !strings.Contains(r.HTML, keep) {
			t.Errorf("link %s must stay untracked", keep)
		}
	}
	if strings.Contains(r.HTML, content.NoTrackAttr) {
		t.Error("the no-track attribute must be removed")
	}
	if rxOpen.MatchString(r.HTML) {
		t.Error("open tracking is off by default")
	}

	// Layout, preheader, footer, inline styles.
	for _, want := range []string{
		`<title>Hi Alice from ACME</title>`,
		`data-mr-preheader`, `Preview for Alice`,
		`<h1 style="margin:0 0 16px;`, `Hello Alice</h1>`,
		`Example Ltd<br>1 High Street, London`,
		`border-top:4px solid #2563eb`,
		`<!--[if mso]>`,
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}

	// Text version.
	for _, want := range []string{"Hello Alice\n===========\n", "\n- Mail us (hi@example.com)\n", "Example Ltd\n1 High Street, London"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, r.Text)
		}
	}
	flat := strings.Join(strings.Fields(r.Text), " ") // long URLs wrap onto their own lines
	for _, want := range []string{"our post (" + testBaseURL + "/c/", "Unsubscribe (" + r.UnsubscribeURL + ") from these emails."} {
		if !strings.Contains(flat, want) {
			t.Errorf("text lacks %q:\n%s", want, r.Text)
		}
	}
	if strings.Contains(r.Text, "<") || strings.Contains(r.Text, "Preview for Alice") {
		t.Errorf("text has tags or the hidden preheader:\n%s", r.Text)
	}

	// A second message reuses the link rows but gets its own tokens.
	r2, err := f.svc.Render(f.ctx, core.RenderInput{MessageID: 78, EmailID: em.ID, Contact: &c, Marketing: true})
	if err != nil {
		t.Fatal(err)
	}
	if r2.HTML == r.HTML || testutil.Count(t, f.d.Read, `SELECT count(*) FROM links`) != 2 {
		t.Fatal("second render must reuse links and differ in tokens")
	}
}

func TestRenderTrackingSettings(t *testing.T) {
	f := newFixture(t)
	c := f.contact("bob@example.com", "Bob", nil)
	em := f.createEmail(content.EmailInput{Subject: "S", Body: "[x](https://example.com/a)"})
	in := core.RenderInput{MessageID: 5, EmailID: em.ID, Contact: &c, Marketing: true}

	f.tracking(false, true)
	r, err := f.svc.Render(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rxClick.MatchString(r.HTML) || !strings.Contains(r.HTML, `href="https://example.com/a"`) {
		t.Fatal("clicks off must leave links alone")
	}
	m := rxOpen.FindStringSubmatch(r.HTML)
	if m == nil || !strings.Contains(r.HTML, `width="1" height="1"`) {
		t.Fatalf("open pixel missing:\n%s", r.HTML)
	}
	if pl, err := f.signer.Verify(tokens.PurposeOpen, m[1]); err != nil || pl.ContactID != c.ID || pl.MessageID != 5 {
		t.Fatalf("open token = %+v %v", pl, err)
	}
	if i, j := strings.Index(r.HTML, "/o/"), strings.Index(r.HTML, "</body>"); i > j {
		t.Fatal("pixel must be inside the body")
	}
	if testutil.Count(t, f.d.Read, `SELECT count(*) FROM links`) != 0 {
		t.Fatal("no links rows without click tracking")
	}
}

func TestRenderPreviewAndTest(t *testing.T) {
	f := newFixture(t)
	f.tracking(true, true)
	c := f.contact("carol@example.com", "Carol", nil)
	em := f.createEmail(content.EmailInput{Subject: "S", Body: "[x](https://example.com/a) {{unsubscribe_url}}"})

	tests := []struct {
		name      string
		in        core.RenderInput
		wantUnsub string
	}{
		{"preview", core.RenderInput{EmailID: em.ID, Contact: &c, Marketing: true}, content.PreviewUnsubscribeURL},
		{"test send", core.RenderInput{MessageID: 9, EmailID: em.ID, ToEmail: "owner@example.org"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := f.svc.Render(f.ctx, tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if r.UnsubscribeURL != tt.wantUnsub {
				t.Fatalf("UnsubscribeURL = %q", r.UnsubscribeURL)
			}
			if rxClick.MatchString(r.HTML) || rxOpen.MatchString(r.HTML) || rxUnsub.MatchString(r.HTML) {
				t.Fatalf("previews and tests must not track or carry a working unsubscribe link:\n%s", r.HTML)
			}
			if !strings.Contains(r.HTML, `href="#preview"`) {
				t.Fatal("the footer keeps an inert unsubscribe link")
			}
		})
	}
	if testutil.Count(t, f.d.Read, `SELECT count(*) FROM links`) != 0 {
		t.Fatal("previews must not write links")
	}
}

// A test send never gets working tokens, even when the caller passes
// Marketing (sending does, so tests look like the real thing), and the
// contact it borrows from Data gets no unsubscribe or click tokens.
func TestRenderTestSendNeverLive(t *testing.T) {
	f := newFixture(t)
	f.tracking(true, true)
	c := f.contact("erin@example.com", "Erin", nil)
	em := f.createEmail(content.EmailInput{Subject: "Hi {{first_name}}", Body: "[x](https://example.com/a) {{unsubscribe_url}}"})
	var id int64
	if err := f.tx(func(tx *sql.Tx) error {
		ids, _, err := f.svc.SendTest(f.ctx, tx, em.ID, []string{"owner@example.org"}, c.ID)
		if len(ids) == 1 {
			id = ids[0]
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The data as the sender reads it back from messages.data.
	var raw string
	if err := f.d.Read.QueryRow(`SELECT data FROM messages WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	for _, in := range []core.RenderInput{
		{MessageID: id, EmailID: em.ID, ToEmail: "owner@example.org", Data: data, Marketing: true},
		{MessageID: id, EmailID: em.ID, ToEmail: "owner@example.org", Data: data, Marketing: true, Contact: &c},
	} {
		r, err := f.svc.Render(f.ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if r.Subject != "Hi Erin" {
			t.Fatalf("subject = %q: the test must show the contact's data", r.Subject)
		}
		if rxClick.MatchString(r.HTML) || rxOpen.MatchString(r.HTML) || rxUnsub.MatchString(r.HTML) || rxUnsub.MatchString(r.Text) {
			t.Fatalf("a test send must not track or carry a working unsubscribe link:\n%s", r.HTML)
		}
		if r.UnsubscribeURL != content.PreviewUnsubscribeURL || !strings.Contains(r.HTML, `href="https://example.com/a"`) {
			t.Fatalf("UnsubscribeURL = %q", r.UnsubscribeURL)
		}
	}
	if testutil.Count(t, f.d.Read, `SELECT count(*) FROM links`) != 0 {
		t.Fatal("test sends must not write links")
	}

	// A live marketing message must come with its contact.
	_, err := f.svc.Render(f.ctx, core.RenderInput{MessageID: 3, EmailID: em.ID, Marketing: true,
		Data: map[string]any{"contact_id": float64(c.ID)}})
	if fieldOf(err) != "contact" {
		t.Fatalf("marketing without contact: err = %v", err)
	}
}

// Links whose URL contains a merge field differ per recipient: they are
// not tracked (no personal data in links), their values are URL-encoded,
// and unsafe schemes from contact data are neutralised.
func TestRenderMergeFieldLinks(t *testing.T) {
	f := newFixture(t)
	f.tracking(true, false)
	body := "[e](https://example.com/?e={{email}}) [s](https://example.com/static) [w]({{attr.website | \"https://example.com\"}})\n\n" +
		`<a href="{{attr.website}}" title="{{first_name}}">raw</a>`
	em := f.createEmail(content.EmailInput{Subject: "S", Body: body})
	contacts := []core.Contact{
		f.contact("al+x@example.com", "Al", map[string]any{"website": "https://al.example.com"}),
		f.contact("bo@example.com", "Bo", map[string]any{"website": "javascript:alert(document.cookie)"}),
		f.contact("cy@example.com", "Cy", nil),
	}
	wantSite := []string{"https://al.example.com", "https://example.com", "https://example.com"}
	for i, c := range contacts {
		r, err := f.svc.Render(f.ctx, core.RenderInput{MessageID: int64(10 + i), EmailID: em.ID, Contact: &c, Marketing: true})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(rxClick.FindAllString(r.HTML, -1)); n != 1 {
			t.Fatalf("%s: tracked links = %d, want 1 (the static one):\n%s", c.Email, n, r.HTML)
		}
		if want := `href="https://example.com/?e=` + url.QueryEscape(c.Email) + `"`; !strings.Contains(r.HTML, want) {
			t.Fatalf("%s: HTML lacks %s:\n%s", c.Email, want, r.HTML)
		}
		if !strings.Contains(r.HTML, `href="`+wantSite[i]+`"`) {
			t.Fatalf("%s: markdown website link must be %s:\n%s", c.Email, wantSite[i], r.HTML)
		}
		if strings.Contains(strings.ToLower(r.HTML), "javascript:") || strings.Contains(r.HTML, content.NoTrackAttr) {
			t.Fatalf("%s: unsafe scheme or no-track marker left:\n%s", c.Email, r.HTML)
		}
	}
	if h := mustRenderHTML(t, f, em.ID, contacts[1]); !strings.Contains(h, `<a href="#" title="Bo" style=`) {
		t.Fatalf("raw HTML href with a javascript: value must become #:\n%s", h)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM links`); n != 1 {
		t.Fatalf("links rows = %d, want 1", n)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM links WHERE url LIKE '%@%' OR url LIKE '%40%'`); n != 0 {
		t.Fatal("links must not store merged personal data")
	}
}

func mustRenderHTML(t *testing.T, f *fixture, emailID int64, c core.Contact) string {
	t.Helper()
	r, err := f.svc.Render(f.ctx, core.RenderInput{MessageID: 99, EmailID: emailID, Contact: &c, Marketing: true})
	if err != nil {
		t.Fatal(err)
	}
	return r.HTML
}

// NoTrackAttr is internal: it is dropped from every rendered email, also
// when click tracking is off, in previews and in HTML-format emails.
func TestRenderDropsNoTrackAttr(t *testing.T) {
	f := newFixture(t)
	f.tracking(false, false)
	c := f.contact("fay@example.com", "Fay", nil)
	md := f.createEmail(content.EmailInput{Subject: "S", Body: `<a href="https://example.com/p" data-mr-notrack>p</a>`})
	h := f.createEmail(content.EmailInput{Subject: "S", Format: content.FormatHTML, Body: `<p><a DATA-MR-NOTRACK="" href="https://example.com/p">p</a></p>`})
	for _, in := range []core.RenderInput{
		{MessageID: 1, EmailID: md.ID, Contact: &c, Marketing: true},
		{EmailID: md.ID, Contact: &c, Marketing: true},
		{MessageID: 2, EmailID: h.ID, Contact: &c, Marketing: true},
		{MessageID: 3, EmailID: h.ID, ToEmail: "owner@example.org"},
	} {
		r, err := f.svc.Render(f.ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(r.HTML), content.NoTrackAttr) || !strings.Contains(r.HTML, `href="https://example.com/p"`) {
			t.Fatalf("%+v: marker must be dropped, link kept:\n%s", in, r.HTML)
		}
	}
	r, err := f.svc.RenderDraft(f.ctx, content.Draft{Subject: "S", Body: `<a href="https://example.com/p" data-mr-notrack>p</a>`}, nil, nil)
	if err != nil || strings.Contains(r.HTML, content.NoTrackAttr) {
		t.Fatalf("draft preview keeps the marker: %v", err)
	}
}

func TestRenderMergeEscaping(t *testing.T) {
	f := newFixture(t)
	c := f.contact("dan@example.com", "<b>Dan</b> & \"Co\"\r\nBcc: x@evil.com", map[string]any{"n": 3.0})
	em := f.createEmail(content.EmailInput{Subject: "Hi {{first_name}}", Body: "Hello {{first_name}}, you have {{attr.n}} seats."})
	r, err := f.svc.Render(f.ctx, core.RenderInput{EmailID: em.ID, Contact: &c, Marketing: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Subject != `Hi <b>Dan</b> & "Co" Bcc: x@evil.com` {
		t.Fatalf("subject must be raw and on one line: %q", r.Subject)
	}
	if strings.Contains(r.HTML, "<b>Dan</b>") || !strings.Contains(r.HTML, "&lt;b&gt;Dan&lt;/b&gt; &amp; &#34;Co&#34;") {
		t.Fatalf("values must be escaped in HTML:\n%s", r.HTML)
	}
	if !strings.Contains(r.HTML, "you have 3 seats") {
		t.Fatal("numeric attribute")
	}
	if !strings.Contains(r.Text, `Hello <b>Dan</b> & "Co"`) {
		t.Fatalf("text must carry the raw value:\n%s", r.Text)
	}

	// No contact: fallbacks.
	em2 := f.createEmail(content.EmailInput{Subject: `Hi {{first_name | "there"}}`, Body: `Dear {{first_name | "reader"}}`})
	r, err = f.svc.Render(f.ctx, core.RenderInput{EmailID: em2.ID, Marketing: true})
	if err != nil || r.Subject != "Hi there" || !strings.Contains(r.HTML, "Dear reader") {
		t.Fatalf("fallbacks = %q %v", r.Subject, err)
	}

	// A test send with data {"contact_id": ...} uses that contact.
	r, err = f.svc.Render(f.ctx, core.RenderInput{MessageID: 1, EmailID: em2.ID, ToEmail: "owner@example.org",
		Data: map[string]any{"contact_id": float64(c.ID)}})
	if err != nil || !strings.HasPrefix(r.Subject, "Hi <b>Dan</b>") {
		t.Fatalf("contact from data = %q %v", r.Subject, err)
	}
	// ... and falls back when the contact is gone.
	r, err = f.svc.Render(f.ctx, core.RenderInput{EmailID: em2.ID, Data: map[string]any{"contact_id": float64(9999)}})
	if err != nil || r.Subject != "Hi there" {
		t.Fatalf("missing contact = %q %v", r.Subject, err)
	}
}

func TestRenderHTMLDocument(t *testing.T) {
	f := newFixture(t)
	doc := `<!DOCTYPE html><html><head><title>GHL</title><style>.x{color:red}</style></head>
<body class="b"><div class="x">Imported for {{email | "you"}}</div></body></html>`
	em := f.createEmail(content.EmailInput{Subject: "Imported", Format: "HTML", Body: doc})
	if em.Format != content.FormatHTML {
		t.Fatalf("format = %q", em.Format)
	}
	r, err := f.svc.Render(f.ctx, core.RenderInput{EmailID: em.ID, Marketing: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.ToLower(r.HTML), "<body") != 1 || strings.Count(strings.ToLower(r.HTML), "<html") != 1 {
		t.Fatalf("document must be unwrapped into the layout:\n%s", r.HTML)
	}
	if i, j := strings.Index(r.HTML, ".x{color:red}"), strings.Index(r.HTML, "</head>"); i < 0 || i > j {
		t.Fatal("imported styles belong in the head")
	}
	if !strings.Contains(r.HTML, `<div class="x">Imported for you</div>`) || strings.Contains(r.HTML, "<title>GHL") {
		t.Fatalf("body content:\n%s", r.HTML)
	}
}

func TestRenderCustomLayout(t *testing.T) {
	f := newFixture(t)
	f.putLayout(content.LayoutInput{
		LogoURL:    "https://cdn.example.com/logo.png?a=1&b=2",
		BrandColor: "#AA0000",
		CustomHTML: `<html><body style="color:{{brand_color}}"><img src="{{logo_url}}">{{content}}<footer>{{footer}} <a href="{{unsubscribe_url}}">Stop</a></footer></body></html>`,
	})
	c := f.contact("erin@example.com", "Erin", nil)
	em := f.createEmail(content.EmailInput{Subject: "S", Preheader: "Pre <text>", Body: "Hi {{first_name}}"})
	r, err := f.svc.Render(f.ctx, core.RenderInput{MessageID: 3, EmailID: em.ID, Contact: &c, Marketing: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`style="color:#aa0000"`,
		`src="https://cdn.example.com/logo.png?a=1&amp;b=2"`,
		`Pre &lt;text&gt;`,
		`<p style="margin:0 0 16px;">Hi Erin</p>`,
		`<footer>Example Ltd<br>1 High Street, London <a href="` + r.UnsubscribeURL + `">Stop</a>`,
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("custom layout lacks %q:\n%s", want, r.HTML)
		}
	}
	if i, j := strings.Index(r.HTML, "data-mr-preheader"), strings.Index(r.HTML, "Hi Erin"); i < 0 || i > j {
		t.Fatal("the preheader goes before the content when the layout doesn't place it")
	}

	// System emails keep the built-in frame (custom layouts need an unsubscribe link).
	r, err = f.svc.Render(f.ctx, core.RenderInput{SystemTemplate: core.TemplateOptinConfirm, Contact: &c, ToEmail: c.Email,
		Data: map[string]any{"confirm_url": testBaseURL + "/f/confirm/tok"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.HTML, "<footer>") || !strings.Contains(r.HTML, "#aa0000") || !strings.Contains(r.HTML, "cdn.example.com/logo.png") {
		t.Fatalf("system email should use the built-in frame with the brand:\n%s", r.HTML)
	}
}

func TestRenderSystemTemplates(t *testing.T) {
	f := newFixture(t)
	f.tracking(true, true)
	confirm := testBaseURL + "/f/confirm/abc.def"
	// A first name a form visitor submitted, with a URL in it: it must never
	// reach a confirmation email.
	c := f.contact("fay@example.com", "Verify now at https://evil.example", nil)
	tests := []struct {
		name        string
		in          core.RenderInput
		subject     string
		contains    []string
		errField    string
		notContains []string
	}{
		{
			name:        "optin",
			in:          core.RenderInput{SystemTemplate: core.TemplateOptinConfirm, Contact: &c, Data: map[string]any{"confirm_url": confirm, "form_name": "Free community"}},
			subject:     "Please confirm your subscription to Example News",
			contains:    []string{`href="` + confirm + `"`, "Hello,", "&ldquo;Free community&rdquo;", "Confirm subscription", "Example Ltd"},
			notContains: []string{"evil.example", "Verify now"},
		},
		{
			name:     "resubscribe",
			in:       core.RenderInput{SystemTemplate: core.TemplateResubscribeConfirm, Data: map[string]any{"confirm_url": confirm}},
			subject:  "Confirm that you want our emails again",
			contains: []string{`href="` + confirm + `"`, "Hello,", "you stay unsubscribed"},
		},
		{
			name:        "alert",
			in:          core.RenderInput{SystemTemplate: core.TemplateAlert, ToEmail: "owner@example.org", Data: map[string]any{"title": "Broadcast paused", "body": "Bounce rate 7%.\nCheck the list.\n\nSecond <para>"}},
			subject:     "Mailroom alert: Broadcast paused",
			contains:    []string{"Alert: Broadcast paused", "Bounce rate 7%.<br>Check the list.", "Second &lt;para&gt;", testBaseURL},
			notContains: []string{"Example Ltd"},
		},
		{name: "optin without url", in: core.RenderInput{SystemTemplate: core.TemplateOptinConfirm, Data: map[string]any{}}, errField: "data.confirm_url"},
		{name: "optin js url", in: core.RenderInput{SystemTemplate: core.TemplateOptinConfirm, Data: map[string]any{"confirm_url": "javascript:alert(1)"}}, errField: "data.confirm_url"},
		{name: "unknown", in: core.RenderInput{SystemTemplate: "welcome"}, errField: "system_template"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := f.svc.Render(f.ctx, tt.in)
			if tt.errField != "" {
				if fieldOf(err) != tt.errField {
					t.Fatalf("err = %v, want validation of %s", err, tt.errField)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Subject != tt.subject || r.UnsubscribeURL != "" {
				t.Fatalf("subject = %q unsub = %q", r.Subject, r.UnsubscribeURL)
			}
			for _, want := range tt.contains {
				if !strings.Contains(r.HTML, want) {
					t.Errorf("HTML lacks %q", want)
				}
			}
			for _, bad := range append(tt.notContains, "Unsubscribe", "/c/", "/o/") {
				if strings.Contains(r.HTML, bad) {
					t.Errorf("HTML must not contain %q (no unsubscribe, no tracking, K-3)", bad)
				}
			}
			if !strings.Contains(r.Text, tt.subject[:10]) && tt.name != "alert" {
				t.Errorf("text version:\n%s", r.Text)
			}
		})
	}
}

func TestRenderErrors(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Render(f.ctx, core.RenderInput{EmailID: 404}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing email = %v", err)
	}
	if _, err := f.svc.Render(f.ctx, core.RenderInput{}); fieldOf(err) != "email_id" {
		t.Fatalf("no email = %v", err)
	}
}

func TestRenderDraft(t *testing.T) {
	f := newFixture(t)
	r, err := f.svc.RenderDraft(f.ctx, content.Draft{Subject: "Live", Body: "**bold** ~~gone~~\n\n| a | b |\n|---|---|\n| 1 | 2 |"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<strong>bold</strong>", "<del>gone</del>", "<table", "<th"} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("GFM output lacks %q", want)
		}
	}
	if !strings.Contains(r.Text, "a | b\n1 | 2") {
		t.Errorf("table text:\n%s", r.Text)
	}
	if _, err := f.svc.RenderDraft(f.ctx, content.Draft{Subject: "x", Body: "{{bogus}}"}, nil, nil); fieldOf(err) != "body" {
		t.Fatalf("invalid draft = %v", err)
	}
	bad := content.LayoutInput{CustomHTML: "<p>{{content}}</p>"}
	if _, err := f.svc.RenderDraft(f.ctx, content.Draft{Subject: "x", Body: "y"}, nil, &bad); fieldOf(err) != "custom_html" {
		t.Fatalf("invalid layout = %v", err)
	}
}
