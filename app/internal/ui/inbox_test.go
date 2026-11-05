package ui_test

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"mailroom/internal/db"
)

// addReceived stores a fetched message the way the inbound fetcher does.
func addReceived(t *testing.T, hs *harness, from, subject, text, html string) int64 {
	t.Helper()
	now := db.FormatTime(hs.clock.Now())
	res, err := hs.app.DB.Write.Exec(`INSERT INTO inbound_messages (s3_key, from_email, from_name, subject, message_id_header,
		received_at, text_body, html_body, snippet, status, next_attempt_at, created_at, updated_at, spam_verdict, dkim_verdict)
		VALUES (?, ?, 'Anna Jensen', ?, 'orig@example.org', ?, ?, ?, ?, 'received', ?, ?, ?, 'PASS', 'PASS')`,
		"inbound/"+subject, from, subject, now, text, html, text, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := hs.app.DB.Write.Exec(`INSERT INTO inbound_attachments (inbound_id, position, filename, content_type, size_bytes, data)
		VALUES (?, 1, 'offer.pdf', 'application/pdf', 4, X'25504446')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestInboxPages(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	c.get("/admin/inbox").want(t, http.StatusOK).contains(t, "Receiving email isn&#39;t switched on", "No email yet")

	id := addReceived(t, hs, "anna@example.org", "Question about prices", "Hi! What does it cost?",
		`<p>Hi!</p><img src="https://track.example.net/x.gif"><script>alert(1)</script>`)
	path := "/admin/inbox/" + itoa(id)
	c.get("/admin/").want(t, http.StatusOK).contains(t, `title="1 unread">1<`)
	c.get("/admin/inbox").want(t, http.StatusOK).contains(t, "Question about prices", "Anna Jensen", "Unread (1)")
	c.get("/admin/inbox?q=nothing-matches").want(t, http.StatusOK).contains(t, "Nothing found")

	page := c.get(path).want(t, http.StatusOK)
	page.contains(t, "Question about prices", "offer.pdf", "is not a contact", "Add as contact", "remote image was",
		`sandbox="allow-popups allow-popups-to-escape-sandbox"`, `hx-post="`+path+`/flags"`)
	page.lacks(t, "track.example.net", "&lt;script")
	c.get(path+"?images=1").want(t, http.StatusOK).contains(t, "track.example.net")

	// Opening marks it read (htmx), then it can be marked unread again.
	c.htmx(http.MethodPost, path+"/flags", url.Values{"action": {"read"}}).want(t, http.StatusNoContent)
	c.get("/admin/").want(t, http.StatusOK).lacks(t, `title="1 unread"`)
	c.post(path+"/flags", url.Values{"action": {"unread"}}).wantRedirect(t, "/admin/inbox")

	// The attachment is a download.
	att := c.get(path+"/attachments/1").want(t, http.StatusOK)
	if att.header.Get("Content-Disposition") != `attachment; filename=offer.pdf` || att.header.Get("Content-Type") != "application/pdf" {
		t.Errorf("attachment headers = %v", att.header)
	}

	// Reply: an empty body is an error in the form; a body queues it.
	c.post(path+"/reply", url.Values{"body": {""}, "quote": {"1"}}).want(t, http.StatusUnprocessableEntity)
	c.post(path+"/reply", url.Values{"body": {"Hi Anna,\n\nIt costs 100 kr."}, "quote": {"1"}}).wantRedirect(t, path)
	if n := count(t, hs, `SELECT count(*) FROM messages WHERE kind = 'reply' AND inbound_id = ?`, id); n != 1 {
		t.Fatalf("replies = %d", n)
	}
	c.get(path).want(t, http.StatusOK).contains(t, "Your replies", "It costs 100 kr.")

	// Archive, then delete after confirming.
	c.post(path+"/flags", url.Values{"action": {"archive"}}).wantRedirect(t, "/admin/inbox")
	c.get("/admin/inbox").want(t, http.StatusOK).lacks(t, "Question about prices")
	c.get("/admin/inbox?box=archived").want(t, http.StatusOK).contains(t, "Question about prices")
	c.post(path+"/delete", url.Values{}).want(t, http.StatusOK).contains(t, "Delete this email?")
	c.post(path+"/delete", url.Values{"confirm": {"yes"}}).wantRedirect(t, "/admin/inbox")
	c.get(path).want(t, http.StatusNotFound)
}

func count(t *testing.T, hs *harness, q string, args ...any) int {
	t.Helper()
	var n int
	if err := hs.app.DB.Read.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
