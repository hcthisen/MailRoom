package ui_test

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailroom/internal/broadcasts"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/segments"
	"mailroom/internal/testutil"
)

// newBroadcastForm is a valid create form.
func newBroadcastForm(name string) url.Values {
	return url.Values{
		"name": {name}, "audience": {"all"},
		"subject": {"Autumn news for {{first_name | \"you\"}}"}, "preheader": {"Three things"},
		"format": {"markdown"}, "body": {"Hi {{first_name | \"there\"}}, this goes to {{email}}."},
	}
}

// createBroadcast creates a broadcast through the UI and returns its ID.
func createBroadcast(t *testing.T, c *client, name string) int64 {
	t.Helper()
	r := c.post("/admin/broadcasts", newBroadcastForm(name)).wantRedirect(t, "/admin/broadcasts/")
	id, err := strconv.ParseInt(strings.TrimPrefix(r.header.Get("Location"), "/admin/broadcasts/"), 10, 64)
	if err != nil {
		t.Fatalf("redirect %q: %v", r.header.Get("Location"), err)
	}
	return id
}

func bcStatus(t *testing.T, hs *harness, id int64) broadcasts.Broadcast {
	t.Helper()
	b, err := hs.app.Broadcasts.Get(hs.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func Test_broadcasts_loggedOutAndCSRF(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	id := createBroadcast(t, c, "Guarded")
	anon := hs.anon()
	for _, p := range []string{"/admin/broadcasts", "/admin/broadcasts/new", fmt.Sprintf("/admin/broadcasts/%d", id),
		fmt.Sprintf("/admin/broadcasts/%d/edit", id), fmt.Sprintf("/admin/broadcasts/%d/stats", id),
		"/admin/broadcasts/audience-count?audience=all", "/admin/layout"} {
		anon.get(p).wantLogin(t)
	}
	for _, p := range []string{"/admin/broadcasts", "/admin/layout", "/admin/layout/preview"} {
		anon.postNoCSRF(p, nil).wantLogin(t)
	}
	for _, a := range []string{"edit", "preview", "test", "schedule", "unschedule", "send", "pause", "resume", "cancel", "duplicate", "delete"} {
		c.postNoCSRF(fmt.Sprintf("/admin/broadcasts/%d/%s", id, a), url.Values{"confirm": {"yes"}}).want(t, http.StatusForbidden)
	}
	c.postNoCSRF("/admin/broadcasts", newBroadcastForm("No token")).want(t, http.StatusForbidden)
	c.postNoCSRF("/admin/layout", url.Values{"brand_color": {"#112233"}}).want(t, http.StatusForbidden)
	if b := bcStatus(t, hs, id); b.Status != broadcasts.StatusDraft {
		t.Fatalf("status %s after forged posts", b.Status)
	}
	if n := hs.count(`SELECT count(*) FROM broadcasts`); n != 1 {
		t.Fatalf("%d broadcasts, want 1", n)
	}
}

func Test_broadcasts_createAndEdit(t *testing.T) {
	hs := newHarness(t)
	testutil.InsertContact(t, hs.app.DB, "ann@example.org", core.StatusActive, "vip")
	testutil.InsertContact(t, hs.app.DB, "bob@example.org", core.StatusActive)
	testutil.InsertContact(t, hs.app.DB, "cid@example.org", core.StatusUnsubscribed, "vip")
	seg, err := hs.app.Segments.Create(hs.ctx, segments.CreateInput{Name: "VIPs",
		Filter: segments.SegmentFilter{Rules: []segments.SegmentRule{{Field: "tag", Op: "has", Value: "vip"}}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	c := hs.login()

	c.get("/admin/broadcasts").want(t, http.StatusOK).contains(t, "<h1>Broadcasts</h1>", "No broadcasts yet", `aria-current="page">Broadcasts`)
	c.get("/admin/broadcasts/new").want(t, http.StatusOK).contains(t,
		"<h1>New broadcast</h1>", `value="all" selected`, "Everyone active", fmt.Sprintf(`value="segment:%d"`, seg.ID), "Segment: VIPs",
		`hx-post="/admin/preview/email"`, `hx-get="/admin/broadcasts/audience-count"`, "Example News")

	// Validation errors land next to their fields and keep the input.
	bad := newBroadcastForm("")
	bad.Set("body", "Hello {{nope}} <b>")
	r := c.post("/admin/broadcasts", bad).want(t, http.StatusUnprocessableEntity)
	r.contains(t, `id="f-name-error"`, "Please correct the highlighted field.")
	bad.Set("name", "Has body error")
	c.post("/admin/broadcasts", bad).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-body-error"`, "Hello {{nope}} &lt;b&gt;")
	bad = newBroadcastForm("Bad reply")
	bad.Set("reply_to", "someone@elsewhere.org")
	c.post("/admin/broadcasts", bad).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-reply_to-error"`, "sending domain")
	bad = newBroadcastForm("Bad audience")
	bad.Set("audience", "segment:9999")
	c.post("/admin/broadcasts", bad).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-audience-error"`, "does not exist")
	if n := hs.count(`SELECT count(*) FROM broadcasts`); n != 0 {
		t.Fatalf("%d broadcasts after invalid posts", n)
	}

	// A valid draft to the segment with overrides.
	form := newBroadcastForm("October <news>")
	form.Set("audience", fmt.Sprintf("segment:%d", seg.ID))
	form.Set("from_name", "The Team")
	form.Set("no_reply_to", "1")
	r = c.post("/admin/broadcasts", form).wantRedirect(t, "/admin/broadcasts/")
	id, _ := strconv.ParseInt(strings.TrimPrefix(r.header.Get("Location"), "/admin/broadcasts/"), 10, 64)
	b := bcStatus(t, hs, id)
	if b.Status != "draft" || b.Audience.SegmentID != seg.ID || b.FromName == nil || *b.FromName != "The Team" || b.ReplyTo == nil || *b.ReplyTo != "" {
		t.Fatalf("created %+v", b)
	}
	show := c.get(r.header.Get("Location")).want(t, http.StatusOK)
	show.contains(t, "Draft saved", "<h1>October &lt;news&gt;</h1>", "Segment: ", "VIPs", "The Team", "Send now…", "Send a test", "owner@example.org").
		lacks(t, "<news>")
	c.get("/admin/broadcasts").want(t, http.StatusOK).contains(t, "October &lt;news&gt;", `badge-muted">draft`)
	c.get("/admin/broadcasts?status=sent").want(t, http.StatusOK).contains(t, "No broadcasts with this status")

	// Edit: prefilled, then saved with the loaded version.
	edit := c.get(fmt.Sprintf("/admin/broadcasts/%d/edit", id)).want(t, http.StatusOK)
	edit.contains(t, `value="The Team"`, `name="no_reply_to" value="1" checked`, fmt.Sprintf(`value="segment:%d" selected`, seg.ID))
	ver := edit.field(t, "version")
	upd := newBroadcastForm("October news")
	upd.Set("version", ver)
	upd.Set("subject", "New subject")
	upd.Set("reply_to", "hello@example.com")
	c.post(fmt.Sprintf("/admin/broadcasts/%d/edit", id), upd).wantRedirect(t, fmt.Sprintf("/admin/broadcasts/%d", id))
	b = bcStatus(t, hs, id)
	if b.Email.Subject != "New subject" || b.Audience.Type != "all" || b.FromName != nil || b.ReplyTo == nil || *b.ReplyTo != "hello@example.com" {
		t.Fatalf("updated %+v", b)
	}
	// The same (now stale) version is refused with 412.
	upd.Set("subject", "Lost update")
	c.post(fmt.Sprintf("/admin/broadcasts/%d/edit", id), upd).want(t, http.StatusPreconditionFailed).contains(t, "Someone else changed this")
	if bcStatus(t, hs, id).Email.Subject != "New subject" {
		t.Fatal("stale edit was saved")
	}
	// Clearing the reply-to goes back to the default.
	upd = newBroadcastForm("October news")
	c.post(fmt.Sprintf("/admin/broadcasts/%d/edit", id), upd).want(t, http.StatusSeeOther)
	if b := bcStatus(t, hs, id); b.ReplyTo != nil {
		t.Fatalf("reply_to %q, want the default", *b.ReplyTo)
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'broadcast.update'`); n < 2 {
		t.Fatalf("%d broadcast.update audit records", n)
	}
}

func Test_broadcasts_audienceCount(t *testing.T) {
	hs := newHarness(t)
	testutil.InsertContact(t, hs.app.DB, "ann@example.org", core.StatusActive, "vip")
	testutil.InsertContact(t, hs.app.DB, "bob@example.org", core.StatusActive)
	testutil.InsertContact(t, hs.app.DB, "cid@example.org", core.StatusBounced, "vip")
	vip, err := hs.app.Segments.Create(hs.ctx, segments.CreateInput{Name: "VIPs",
		Filter: segments.SegmentFilter{Rules: []segments.SegmentRule{{Field: "tag", Op: "has", Value: "vip"}}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	ghost, err := hs.app.Segments.Create(hs.ctx, segments.CreateInput{Name: "Ghosts",
		Filter: segments.SegmentFilter{Rules: []segments.SegmentRule{{Field: "tag", Op: "has", Value: "ghost-tag"}}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	c := hs.session()
	c.htmx(http.MethodGet, "/admin/broadcasts/audience-count?audience=all", nil).want(t, http.StatusOK).
		contains(t, "About <strong>2</strong> active contacts in total").lacks(t, "<html")
	c.htmx(http.MethodGet, fmt.Sprintf("/admin/broadcasts/audience-count?audience=segment:%d", vip.ID), nil).want(t, http.StatusOK).
		contains(t, "About <strong>1</strong> active contact in this segment").lacks(t, "Check the segment")
	c.htmx(http.MethodGet, fmt.Sprintf("/admin/broadcasts/audience-count?audience=segment:%d", ghost.ID), nil).want(t, http.StatusOK).
		contains(t, "Check the segment", "ghost-tag")
	c.htmx(http.MethodGet, "/admin/broadcasts/audience-count?audience=segment:777", nil).want(t, http.StatusOK).
		contains(t, "This segment no longer exists.")
	c.htmx(http.MethodGet, "/admin/broadcasts/audience-count?audience=junk", nil).want(t, http.StatusOK).
		contains(t, "can&#39;t be counted")
}

func Test_broadcasts_testSendAndPreview(t *testing.T) {
	hs := newHarness(t)
	ann := testutil.InsertContact(t, hs.app.DB, "ann@example.org", core.StatusActive)
	c := hs.session()
	id := createBroadcast(t, c, "Tested")
	path := fmt.Sprintf("/admin/broadcasts/%d", id)

	// Problems come back as a flash on the page.
	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"to": {"me@example.org, not-an-address"}}, "address 2 is not a valid email address"},
		{url.Values{"to": {"a@x.org b@x.org c@x.org d@x.org e@x.org f@x.org"}}, "at most 5"},
		{url.Values{"to": {""}}, "enter at least one address"},
		{url.Values{"to": {"me@example.org"}, "contact": {"99999"}}, "does not exist"},
		{url.Values{"to": {"me@example.org"}, "contact": {"12abc"}}, "must be a contact&#39;s email address"},
		{url.Values{"to": {"me@example.org"}, "contact": {"nobody@example.org"}}, "no contact has the address nobody@example.org"},
	} {
		c.post(path+"/test", tc.form).wantRedirect(t, path)
		c.get(path).want(t, http.StatusOK).contains(t, "The test was not sent", tc.want)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'test'`); n != 0 {
		t.Fatalf("%d test messages after invalid posts", n)
	}
	// The contact is given by its email address (the UI shows no IDs), or by ID.
	c.post(path+"/test", url.Values{"to": {"me@example.org\nyou@example.org"}, "contact": {" Ann@Example.org "}}).
		wantRedirect(t, path)
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'test' AND json_extract(data, '$.contact_id') = ?`, ann.ID); n != 2 {
		t.Fatalf("%d test messages with ann's data, want 2", n)
	}
	c.get(path).want(t, http.StatusOK).contains(t, "Test queued to me@example.org, you@example.org")
	c.post(path+"/test", url.Values{"to": {"me@example.org"}, "contact": {strconv.FormatInt(ann.ID, 10)}}).wantRedirect(t, path)

	// Preview of the saved email, with and without a contact's data.
	p := c.htmx(http.MethodPost, path+"/preview", url.Values{}).want(t, http.StatusOK)
	p.contains(t, `<iframe class="preview-frame"`, `sandbox="allow-same-origin"`, "Autumn news for you").lacks(t, "<html")
	c.htmx(http.MethodPost, path+"/preview", url.Values{"preview_contact": {"ann@example.org"}}).want(t, http.StatusOK).
		contains(t, "ann@example.org")
	c.htmx(http.MethodPost, path+"/preview", url.Values{"preview_contact": {strconv.FormatInt(ann.ID, 10)}}).want(t, http.StatusOK).
		contains(t, "ann@example.org")
	c.htmx(http.MethodPost, path+"/preview", url.Values{"preview_contact": {"424242"}}).want(t, http.StatusOK).
		contains(t, "preview-error", "contact 424242 does not exist")
	c.htmx(http.MethodPost, path+"/preview", url.Values{"preview_contact": {"who@example.org"}}).want(t, http.StatusOK).
		contains(t, "preview-error", "no contact has the address who@example.org")
}

func Test_broadcasts_scheduleSendPauseResume(t *testing.T) {
	hs := newHarness(t)
	testutil.InsertContact(t, hs.app.DB, "ann@example.org", core.StatusActive)
	testutil.InsertContact(t, hs.app.DB, "bob@example.org", core.StatusActive)
	testutil.InsertContact(t, hs.app.DB, "gone@example.org", core.StatusUnsubscribed)
	c := hs.session()
	id := createBroadcast(t, c, "Big send")
	path := fmt.Sprintf("/admin/broadcasts/%d", id)

	// The schedule form is prefilled with tomorrow 09:00 (Europe/London);
	// the page has the checklist and the editor's preview and test box.
	show := c.get(path).want(t, http.StatusOK)
	show.contains(t, `name="at" type="datetime-local" value="2026-01-06T09:00"`, "Europe/London", "Before you send",
		"2 active contacts", "Not sent yet", `hx-post="/admin/editor/test"`).lacks(t, "Cancel broadcast…")

	// Schedule: a past time is an inline error, a future one schedules.
	c.post(path+"/schedule", url.Values{"at": {"2026-01-01T10:00"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-at-error"`, "must be in the future")
	c.post(path+"/schedule", url.Values{"at": {"garbage"}}).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-at-error"`)
	// Scheduling asks first, with the recipient count, and changes nothing.
	c.post(path+"/schedule", url.Values{"at": {"2026-07-01T09:30"}}).want(t, http.StatusOK).
		contains(t, "Schedule for 2 recipients", `name="confirm" value="yes"`)
	if bcStatus(t, hs, id).Status != "draft" {
		t.Fatal("the schedule confirmation scheduled it")
	}
	c.post(path+"/schedule", url.Values{"at": {"2026-07-01T09:30"}, "confirm": {"yes"}}).wantRedirect(t, path)
	b := bcStatus(t, hs, id)
	if b.Status != "scheduled" || b.ScheduledAt == nil || !b.ScheduledAt.Equal(time.Date(2026, 7, 1, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("scheduled %+v (BST is UTC+1)", b)
	}
	c.get(path).want(t, http.StatusOK).contains(t, "Scheduled for 1 Jul 2026, 09:30", "Unschedule")
	c.post(path+"/unschedule", url.Values{"version": {"1"}}).wantRedirect(t, path) // stale version
	c.get(path).want(t, http.StatusOK).contains(t, "Someone else changed this")
	if bcStatus(t, hs, id).Status != "scheduled" {
		t.Fatal("stale unschedule applied")
	}
	c.post(path+"/unschedule", nil).wantRedirect(t, path)
	if bcStatus(t, hs, id).Status != "draft" {
		t.Fatal("not unscheduled")
	}

	// Send now: the first step is a confirmation with the dry-run count
	// and changes nothing.
	ver := c.get(path).want(t, http.StatusOK).field(t, "version")
	conf := c.post(path+"/send", url.Values{"version": {ver}}).want(t, http.StatusOK)
	conf.contains(t, "Please confirm", "2 recipients: every active contact whose address isn&#39;t suppressed", "Send anyway to 2 recipients", "No test of this email has been delivered yet",
		`name="confirm" value="yes"`, `name="version" value="`+ver+`"`, "btn-danger")
	if bcStatus(t, hs, id).Status != "draft" {
		t.Fatal("dry run changed the status")
	}
	if n := hs.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("dry run left %d jobs", n)
	}
	c.post(path+"/send", url.Values{"version": {ver}, "confirm": {"yes"}}).wantRedirect(t, path+"/stats")
	b = bcStatus(t, hs, id)
	if b.Status != "sending" || b.SnapshotJobID == nil {
		t.Fatalf("after send %+v", b)
	}
	c.get(path+"/stats").want(t, http.StatusOK).contains(t, "Sending started: 2 recipients are being queued", `hx-trigger="every 10s"`, "Clicks per link")
	// Sending again is a conflict, shown as a flash.
	c.post(path+"/send", url.Values{"confirm": {"yes"}}).wantRedirect(t, path)
	c.get(path).want(t, http.StatusOK).contains(t, "flash-error", "send needs a draft or scheduled broadcast").lacks(t, "Send now…")

	// Pause and resume by hand.
	c.post(path+"/pause", nil).wantRedirect(t, path)
	if b := bcStatus(t, hs, id); b.Status != "paused" || b.PausedReason != "manual" {
		t.Fatalf("after pause %+v", b)
	}
	c.get(path).want(t, http.StatusOK).contains(t, "Paused by hand", "Resume", "Cancel broadcast…")
	c.post(path+"/resume", nil).wantRedirect(t, path)
	if b := bcStatus(t, hs, id); b.Status != "sending" {
		t.Fatalf("after resume %+v", b)
	}

	// A circuit-breaker pause needs the confirm checkbox.
	testutil.Exec(t, hs.app.DB, `UPDATE broadcasts SET status = 'paused', paused_reason = 'circuit_breaker' WHERE id = ?`, id)
	c.get(path).want(t, http.StatusOK).contains(t, "Paused by the circuit breaker", `name="confirm_resume"`, "Bounce rate")
	c.post(path+"/resume", nil).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-confirm_resume-error"`, "Tick this box")
	if bcStatus(t, hs, id).Status != "paused" {
		t.Fatal("breaker resume without confirm applied")
	}
	c.post(path+"/resume", url.Values{"confirm_resume": {"1"}}).wantRedirect(t, path)
	if b := bcStatus(t, hs, id); b.Status != "sending" {
		t.Fatalf("after confirmed resume %+v", b)
	}
}

func Test_broadcasts_cancelDeleteDuplicate(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	id := createBroadcast(t, c, "Short-lived")
	path := fmt.Sprintf("/admin/broadcasts/%d", id)

	// Duplicate opens the copy's edit form.
	r := c.post(path+"/duplicate", nil).wantRedirect(t, "/admin/broadcasts/")
	loc := r.header.Get("Location")
	if !regexp.MustCompile(`^/admin/broadcasts/\d+/edit$`).MatchString(loc) {
		t.Fatalf("duplicate redirect %q", loc)
	}
	c.get(loc).want(t, http.StatusOK).contains(t, "Copy created", `value="Short-lived (copy)"`)

	// Cancel: confirmation first, then cancelled.
	c.post(path+"/cancel", nil).want(t, http.StatusOK).contains(t, "Please confirm", "Cancel Short-lived?", "Status now: draft", "Cancel broadcast")
	if bcStatus(t, hs, id).Status != "draft" {
		t.Fatal("cancel without confirm applied")
	}
	c.post(path+"/cancel", url.Values{"confirm": {"yes"}}).wantRedirect(t, path)
	if bcStatus(t, hs, id).Status != "cancelled" {
		t.Fatal("not cancelled")
	}
	c.get(path).want(t, http.StatusOK).contains(t, "Broadcast cancelled.", "Delete…").lacks(t, "Send now…", `href="/admin/broadcasts/`+strconv.FormatInt(id, 10)+`/edit"`)
	// The edit page of a cancelled broadcast sends back to the detail page.
	c.get(path+"/edit").wantRedirect(t, path)

	// Delete: confirmation, then gone.
	c.post(path+"/delete", nil).want(t, http.StatusOK).contains(t, "Delete Short-lived?", "can&#39;t be undone")
	c.post(path+"/delete", url.Values{"confirm": {"yes"}}).wantRedirect(t, "/admin/broadcasts")
	c.get(path).want(t, http.StatusNotFound)
	if n := hs.count(`SELECT count(*) FROM broadcasts WHERE id = ?`, id); n != 0 {
		t.Fatal("not deleted")
	}
	c.get("/admin/broadcasts/abc").want(t, http.StatusNotFound)
}

func Test_broadcasts_statsPage(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	id := createBroadcast(t, c, "Counted")
	path := fmt.Sprintf("/admin/broadcasts/%d/stats", id)
	c.get(path).want(t, http.StatusOK).contains(t, "Stats: Counted", "Not sent yet.", "No clicks yet.", "No recipients yet").
		lacks(t, `hx-trigger="every 10s"`)

	ann := testutil.InsertContact(t, hs.app.DB, "ann@example.org", core.StatusActive)
	bob := testutil.InsertContact(t, hs.app.DB, "bob@example.org", core.StatusActive)
	now := db.FormatTime(testutil.Epoch)
	testutil.Exec(t, hs.app.DB, `UPDATE broadcasts SET status = 'sent', started_at = ?, finished_at = ?, recipients_total = 2, snapshot_done_at = ? WHERE id = ?`, now, now, now, id)
	b := bcStatus(t, hs, id)
	testutil.Exec(t, hs.app.DB, `INSERT INTO messages (kind, broadcast_id, email_id, contact_id, to_email, status, queued_at, next_attempt_at, sent_at, delivered_at, updated_at)
		VALUES ('broadcast', ?, ?, ?, ?, 'delivered', ?, ?, ?, ?, ?)`, id, b.Email.ID, ann.ID, ann.Email, now, now, now, now, now)
	testutil.Exec(t, hs.app.DB, `INSERT INTO messages (kind, broadcast_id, email_id, contact_id, to_email, status, error, queued_at, next_attempt_at, updated_at)
		VALUES ('broadcast', ?, ?, ?, ?, 'failed', 'rejected <by> provider', ?, ?, ?)`, id, b.Email.ID, bob.ID, bob.Email, now, now, now)

	r := c.get(path).want(t, http.StatusOK)
	r.contains(t, "Finished 5 Jan 2026, 09:00", `<p class="label">Delivered</p>`, "ann@example.org", "bob@example.org",
		"rejected &lt;by&gt; provider", fmt.Sprintf(`href="/admin/contacts/%d"`, ann.ID))
	c.get(path+"?status=failed").want(t, http.StatusOK).contains(t, "bob@example.org").lacks(t, "ann@example.org")
	c.get(path+"?status=bounced").want(t, http.StatusOK).contains(t, "No recipients with this status.")
	c.get(path+"?limit=1").want(t, http.StatusOK).contains(t, "ann@example.org", "Next page").lacks(t, "bob@example.org")
	// The detail page of a sent broadcast shows the numbers and no send controls.
	c.get(fmt.Sprintf("/admin/broadcasts/%d", id)).want(t, http.StatusOK).
		contains(t, "Sent to 2 recipients", "All stats and recipients").lacks(t, "Send now…", "Cancel broadcast…")
}
