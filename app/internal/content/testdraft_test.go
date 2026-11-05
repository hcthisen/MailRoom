package content_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// A test of unsaved content carries the content in its data and renders
// from there like a test of a stored email: the contact's data, no live
// links, no links rows; a visual-editor body gets the inline styles.
func TestSendTestDraft(t *testing.T) {
	f := newFixture(t)
	f.tracking(true, true)
	c := f.contact("erin@example.com", "Erin", nil)
	d := content.Draft{Subject: "Hi {{first_name}}", Format: content.FormatHTML,
		Body: content.VisualMarker + "\n<p>Hello <a href=\"https://example.com/a\">x</a></p><p><a class=\"mr-button\" href=\"https://example.com/b\">Go</a></p>"}
	var ids []int64
	if err := f.tx(func(tx *sql.Tx) error {
		var err error
		ids, _, err = f.svc.SendTestDraft(f.ctx, tx, d, []string{"owner@example.org"}, c.ID, content.SenderOverride{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := f.d.Read.QueryRow(`SELECT data FROM messages WHERE id = ? AND kind = 'test' AND email_id IS NULL`, ids[0]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.Render(f.ctx, core.RenderInput{MessageID: ids[0], ToEmail: "owner@example.org", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if r.Subject != "Hi Erin" || !strings.Contains(r.HTML, "margin:0 0 16px;") || !strings.Contains(r.HTML, "display:inline-block;background:") {
		t.Fatalf("rendered %q:\n%s", r.Subject, r.HTML)
	}
	if rxClick.MatchString(r.HTML) || rxUnsub.MatchString(r.HTML) || testutil.Count(t, f.d.Read, `SELECT count(*) FROM links`) != 0 {
		t.Fatal("a draft test must not track or carry a working unsubscribe link")
	}

	// Invalid content is refused like on save.
	err = f.tx(func(tx *sql.Tx) error {
		_, _, err := f.svc.SendTestDraft(f.ctx, tx, content.Draft{Subject: "x", Format: "markdown", Body: "{{nope}}"}, nil, 0, content.SenderOverride{})
		return err
	})
	if fieldOf(err) != "body" {
		t.Fatalf("err = %v", err)
	}
}

// Plain HTML (not from the visual editor) is used exactly as written.
func TestHTMLBodyNotStyledWithoutMarker(t *testing.T) {
	f := newFixture(t)
	r, err := f.svc.RenderDraft(f.ctx, content.Draft{Subject: "x", Format: content.FormatHTML, Body: "<p>Hello</p>"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.HTML, "<p>Hello</p>") {
		t.Fatalf("plain HTML was changed:\n%s", r.HTML)
	}
}

func TestMarkdownToHTMLBody(t *testing.T) {
	f := newFixture(t)
	out, err := f.svc.MarkdownToHTMLBody("Hi **{{first_name | \"you\"}}**\n\n[Shop]({{attr.url}})")
	if err != nil {
		t.Fatal(err)
	}
	if !content.IsVisualBody(out) || !strings.Contains(out, "<strong>{{first_name | &#34;you&#34;}}</strong>") || !strings.Contains(out, `href="{{attr.url}}"`) {
		t.Fatalf("got %s", out)
	}
}
