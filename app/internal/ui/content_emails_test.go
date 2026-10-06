package ui_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

func TestContentLoggedOut(t *testing.T) {
	hs := newHarness(t)
	c := hs.anon()
	for _, p := range []string{"/admin/emails", "/admin/emails/new", "/admin/emails/1", "/admin/media"} {
		c.get(p).wantLogin(t)
	}
	c.postRaw("/admin/emails", url.Values{"subject": {"x"}, "body": {"y"}}).wantLogin(t)
	if n := hs.count(`SELECT count(*) FROM emails`); n != 0 {
		t.Fatalf("%d emails created logged out", n)
	}
}

func TestContentEmailsCSRF(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	id := contentCreateEmail(t, c, "Draft", "Hello", "Hi there")
	path := fmt.Sprintf("/admin/emails/%d", id)
	for _, p := range []string{"/admin/emails", path, path + "/test", path + "/delete"} {
		c.postNoCSRF(p, url.Values{"subject": {"x"}, "body": {"y"}, "confirm": {"yes"}}).want(t, http.StatusForbidden)
	}
	if n := hs.count(`SELECT count(*) FROM emails`); n != 1 {
		t.Fatalf("%d emails, want 1", n)
	}
	if n := hs.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("%d messages queued without CSRF", n)
	}
}

// contentCreateEmail creates an email through the UI and returns its ID.
func contentCreateEmail(t *testing.T, c *client, name, subject, body string) int64 {
	t.Helper()
	r := c.post("/admin/emails", url.Values{"name": {name}, "subject": {subject}, "format": {"markdown"}, "body": {body}}).
		wantRedirect(t, "/admin/emails/")
	return audLocationID(t, r, "/admin/emails/")
}

func TestContentEmailLibrary(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()

	// The nav has the Content section; the empty list says what it is for.
	c.get("/admin/emails").want(t, http.StatusOK).contains(t, "<h1>Emails</h1>", `aria-current="page">Content`,
		`aria-label="Content sections"`, `href="/admin/media"`, `href="/admin/layout"`, "No emails yet")
	c.get("/admin/emails/new").want(t, http.StatusOK).contains(t, "<h1>New email</h1>", `hx-post="/admin/preview/email"`, `name="body"`)

	// Validation errors are shown next to the field; nothing is saved.
	c.post("/admin/emails", url.Values{"subject": {""}, "body": {"x"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-subject-error"`)
	c.post("/admin/emails", url.Values{"subject": {"Hi {{contact.first_name}}"}, "body": {"x"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-subject-error"`, "Hi {{contact.first_name}}")
	if n := hs.count(`SELECT count(*) FROM emails`); n != 0 {
		t.Fatalf("%d emails after invalid posts", n)
	}

	// Create (create-email) with an audit record, then list and search.
	id := contentCreateEmail(t, c, "Welcome 1", "Welcome, {{first_name | \"friend\"}}!", "Hi **there**")
	path := fmt.Sprintf("/admin/emails/%d", id)
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'email.create'`); n != 1 {
		t.Fatalf("%d email.create audit rows", n)
	}
	page := c.get(path).want(t, http.StatusOK)
	page.contains(t, "Email saved.", "<h1>Welcome 1</h1>", "Nothing: no sequence step or broadcast uses this email", `action="/admin/emails/`+strconv.FormatInt(id, 10)+`/test"`)
	ver := page.field(t, "version")
	contentCreateEmail(t, c, "Other", "Different", "x")
	c.get("/admin/emails").want(t, http.StatusOK).contains(t, "Welcome 1", "Other", "unused", "Markdown")
	c.get("/admin/emails?q=welcome").want(t, http.StatusOK).contains(t, "Welcome 1").lacks(t, ">Other<")
	c.get("/admin/emails?q=nothing-like-it").want(t, http.StatusOK).contains(t, "No email matches")

	// Update (update-email) with the loaded version as If-Match.
	upd := url.Values{"version": {ver}, "name": {"Welcome 1"}, "subject": {"Welcome!"}, "format": {"html"}, "body": {"<p>Hello</p>"}}
	c.post(path, upd).wantRedirect(t, path)
	e, err := hs.app.Content.GetEmail(hs.ctx, nil, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.Subject != "Welcome!" || e.Format != "html" || e.Body != "<p>Hello</p>" || e.Version != 2 {
		t.Fatalf("saved %+v", e)
	}
	upd.Set("subject", "Stale")
	c.post(path, upd).want(t, http.StatusPreconditionFailed).contains(t, "Someone else changed this", `name="version" value="2"`)

	// Test send (send-test-email): to the owner by default, with a
	// contact's data given by email address.
	ann := testutil.InsertContact(t, hs.app.DB, "ann@example.org", core.StatusActive)
	c.post(path+"/test", url.Values{"to": {"not-an-address"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-to-error"`, "is not a valid email address")
	c.post(path+"/test", url.Values{"contact": {"nobody@example.org"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-contact-error"`, "no contact has the address")
	c.post(path+"/test", url.Values{"contact": {"ann@example.org"}}).wantRedirect(t, path)
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'test' AND email_id = ? AND to_email = 'owner@example.org'
		AND json_extract(data, '$.contact_id') = ?`, id, ann.ID); n != 1 {
		t.Fatalf("%d test messages to the owner with ann's data", n)
	}
	c.get(path).want(t, http.StatusOK).contains(t, "Test queued to owner@example.org", "1 message with this email still waiting to be sent")

	// Delete (delete-email): refused while in use, with what uses it.
	seq := testutil.InsertSequence(t, hs.app.DB, "Onboarding")
	testutil.InsertStep(t, hs.app.DB, seq, 1, 60, id)
	c.get("/admin/emails").want(t, http.StatusOK).contains(t, "1 sequence step")
	c.get(path).want(t, http.StatusOK).contains(t, "Step 1 of the sequence", "Onboarding", fmt.Sprintf(`href="/admin/sequences/%d/steps/`, seq))
	c.post(path+"/delete", url.Values{"version": {"2"}}).want(t, http.StatusConflict).
		contains(t, "can&#39;t be deleted while it is in use", "Step 1 of the sequence &#34;Onboarding&#34;", "still waiting to be sent")
	c.post(path+"/delete", url.Values{"version": {"2"}, "confirm": {"yes"}}).wantRedirect(t, path)
	if n := hs.count(`SELECT count(*) FROM emails WHERE id = ?`, id); n != 1 {
		t.Fatal("an email in use was deleted")
	}

	// An unused email: confirmation first, then deleted.
	free := contentCreateEmail(t, c, "Free", "Nobody uses me", "x")
	fpath := fmt.Sprintf("/admin/emails/%d", free)
	c.post(fpath+"/delete", url.Values{"version": {"1"}}).want(t, http.StatusOK).
		contains(t, "Delete &#34;Free&#34;?", "Subject: Nobody uses me", `name="confirm" value="yes"`, "Delete email")
	if n := hs.count(`SELECT count(*) FROM emails WHERE id = ?`, free); n != 1 {
		t.Fatal("the confirmation step deleted the email")
	}
	c.post(fpath+"/delete", url.Values{"version": {"1"}, "confirm": {"yes"}}).wantRedirect(t, "/admin/emails")
	if n := hs.count(`SELECT count(*) FROM emails WHERE id = ?`, free); n != 0 {
		t.Fatal("the email was not deleted")
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'email.delete'`); n != 1 {
		t.Fatalf("%d email.delete audit rows", n)
	}
	c.get(fpath).want(t, http.StatusNotFound)
}

// TestContentEmailOfSentBroadcast: a sent broadcast's email is shown
// read-only with a pointer to duplicating the broadcast.
func TestContentEmailOfSentBroadcast(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	id := contentCreateEmail(t, c, "", "Spring news", "x")
	b := testutil.InsertBroadcast(t, hs.app.DB, "Spring", id)
	testutil.Exec(t, hs.app.DB, `UPDATE broadcasts SET status = 'sent' WHERE id = ?`, b)
	path := fmt.Sprintf("/admin/emails/%d", id)
	c.get(path).want(t, http.StatusOK).contains(t, "<h1>Spring news</h1>", "which is sent: it can't change any more",
		fmt.Sprintf(`href="/admin/broadcasts/%d"`, b)).lacks(t, "Save changes")
	c.post(path, url.Values{"version": {"1"}, "subject": {"Changed"}, "body": {"x"}}).want(t, http.StatusConflict)
}
