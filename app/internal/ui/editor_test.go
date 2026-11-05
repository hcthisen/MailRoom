package ui_test

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// TestEditorHelpers covers the email editor's actions: the "Preview as"
// contacts, the merge-field menu, previewing as a contact, Markdown →
// visual conversion and the Send test of unsaved content.
func TestEditorHelpers(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	ann := testutil.InsertContact(t, hs.app.DB, "ann@example.org", core.StatusActive)
	if _, err := hs.app.DB.Write.Exec(`UPDATE contacts SET first_name = 'Ann', attributes = '{"company":"Acme"}' WHERE id = ?`, ann.ID); err != nil {
		t.Fatal(err)
	}
	testutil.InsertContact(t, hs.app.DB, "gone@example.org", core.StatusUnsubscribed)

	// The editor is enhanced in place: modes, toolbar, preview picker, test box.
	c.get("/admin/emails/new").want(t, http.StatusOK).contains(t, `data-editor`, `data-toolbar`, `id="preview-contact"`,
		`hx-post="/admin/editor/test"`, `value="owner@example.org"`, `name="format" value="html" checked`)

	// Only active contacts, the first selected; "0" keeps none selected.
	c.get("/admin/editor/contacts?audience=all").want(t, http.StatusOK).
		contains(t, `value="`+strconv.FormatInt(ann.ID, 10)+`" selected>Ann  · ann@example.org`).lacks(t, "gone@example.org")
	c.get("/admin/editor/contacts?preview_contact=0").want(t, http.StatusOK).contains(t, `<option value="0" selected>`)

	// Merge fields list the attribute keys found on contacts.
	c.get("/admin/editor/fields").want(t, http.StatusOK).
		contains(t, `data-merge-tag="first_name"`, `data-merge-tag="attr.company"`, "{{attr.company}}", "e.g. Acme")

	// The preview shows the chosen contact's data instead of the fallback.
	draft := url.Values{"subject": {`Hi {{first_name | "there"}}`}, "format": {"html"},
		"body": {"<!-- mailroom:visual -->\n<p>From {{attr.company}}</p><p><a class=\"mr-button\" href=\"https://example.com\">Go</a></p>"}}
	pv := url.Values{}
	for k, v := range draft {
		pv[k] = v
	}
	pv.Set("preview_contact", strconv.FormatInt(ann.ID, 10))
	c.htmx(http.MethodPost, "/admin/preview/email", pv).want(t, http.StatusOK).contains(t, "Hi Ann", "From Acme", "display:inline-block;background:")
	pv.Set("preview_contact", "0")
	c.htmx(http.MethodPost, "/admin/preview/email", pv).want(t, http.StatusOK).contains(t, "Hi there")
	// An empty draft still previews the layout.
	c.htmx(http.MethodPost, "/admin/preview/email", url.Values{}).want(t, http.StatusOK).contains(t, "(No subject yet)")

	// Markdown converts to visual HTML, keeping merge fields.
	c.post("/admin/editor/convert", url.Values{"body": {`Hi **{{first_name | "you"}}**`}}).want(t, http.StatusOK).
		contains(t, "<!-- mailroom:visual -->", "<strong>{{first_name | &#34;you&#34;}}</strong>")
	c.post("/admin/editor/convert", url.Values{"body": {"{{nope}}"}}).want(t, http.StatusUnprocessableEntity).contains(t, "unknown merge field")

	// Send test of unsaved content: queued with the content and contact.
	tv := url.Values{}
	for k, v := range draft {
		tv[k] = v
	}
	tv.Set("test_to", "not-an-address")
	c.htmx(http.MethodPost, "/admin/editor/test", tv).want(t, http.StatusOK).contains(t, "Send to", "is not a valid email address")
	tv.Set("test_to", "me@example.org, owner@example.org")
	tv.Set("preview_contact", strconv.FormatInt(ann.ID, 10))
	c.htmx(http.MethodPost, "/admin/editor/test", tv).want(t, http.StatusOK).contains(t, "Test queued for me@example.org, owner@example.org")
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'test' AND email_id IS NULL AND json_extract(data, '$.contact_id') = ?
		AND json_extract(data, '$.draft.subject') = 'Hi {{first_name | "there"}}'`, ann.ID); n != 2 {
		t.Fatalf("%d draft test messages", n)
	}
	c.postNoCSRF("/admin/editor/test", tv).want(t, http.StatusForbidden)
}
