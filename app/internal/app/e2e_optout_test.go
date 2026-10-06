package app_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"mailroom/internal/app"
	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/events"
	"mailroom/internal/sequences"
)

// TestE2EOptOutWhileSuppressedOrBounced: a one-click unsubscribe from a
// contact that is manually suppressed or bounced keeps its status but is
// recorded (U-4), and lifting the suppression (remove-suppression) or the
// bounce (set-contact-status with a note) lands on unsubscribed, so the
// next broadcast doesn't reach them (U-3).
func TestE2EOptOutWhileSuppressedOrBounced(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	manual := hs.createContact(`{"email":"manual@example.org"}`)
	bounced := hs.createContact(`{"email":"bounced@example.org"}`)
	b1 := hs.createBroadcast("First", "First news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b1.ID); n != 2 {
		t.Fatalf("recipients = %d", n)
	}
	hs.sendAll()

	oneClick := func(addr string) {
		t.Helper()
		path := hs.mailTo(addr)[0].unsubscribePath(t)
		hs.raw("POST", path, "application/x-www-form-urlencoded", "List-Unsubscribe=One-Click").want(t, http.StatusOK)
	}
	unsubEvents := func(contactID int64) int {
		t.Helper()
		return hs.count(`SELECT count(*) FROM events e JOIN messages m ON m.id = e.message_id
			WHERE m.contact_id = ? AND e.type = 'unsubscribe' AND e.subtype = 'one_click'`, contactID)
	}

	hs.api("POST", "/suppressions", `{"email":"manual@example.org","note":"asked by phone"}`).want(t, http.StatusCreated)
	oneClick("manual@example.org")
	oneClick("manual@example.org") // a repeat records nothing more
	msg := hs.messageIDs(bounced.ID, core.KindBroadcast)[0]
	hs.notify("sns-optout-1", hs.bounceEvent(hs.sesMessageID(msg), "Permanent", "bounced@example.org"), "2")
	oneClick("bounced@example.org")
	for _, c := range []contacts.Contact{manual, bounced} {
		if n := unsubEvents(c.ID); n != 1 {
			t.Fatalf("%s: unsubscribe events = %d, want 1", c.Email, n)
		}
	}
	if st := hs.contactStatus(manual.ID); st != core.StatusSuppressedManual {
		t.Fatalf("manual = %s", st)
	}
	if st := hs.contactStatus(bounced.ID); st != core.StatusBounced {
		t.Fatalf("bounced = %s", st)
	}

	var rm contacts.UnsuppressResult
	hs.api("POST", "/suppressions/remove", `{"email":"manual@example.org"}`).want(t, http.StatusOK).decode(t, &rm)
	if !rm.StatusChanged || rm.Status != core.StatusUnsubscribed {
		t.Fatalf("remove-suppression = %+v", rm)
	}
	var set struct {
		Contact contacts.Contact `json:"contact"`
		Changed bool             `json:"changed"`
	}
	hs.api("POST", fmt.Sprintf("/contacts/%d/status", bounced.ID), `{"status":"active","note":"mailbox fixed"}`).
		want(t, http.StatusOK).decode(t, &set)
	if !set.Changed || set.Contact.Status != core.StatusUnsubscribed {
		t.Fatalf("reactivated bounced contact = %+v, want unsubscribed", set)
	}

	b2 := hs.createBroadcast("Second", "Second news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b2.ID); n != 0 {
		t.Fatalf("second broadcast recipients = %d, want 0", n)
	}
	hs.sendAll()
	for _, addr := range []string{"manual@example.org", "bounced@example.org"} {
		if got := hs.subjectsTo(addr); len(got) != 1 {
			t.Fatalf("%s got %q after opting out", addr, got)
		}
	}
}

// TestE2ESequenceStopCoversQueuedLastStep: "Only stop these emails" after
// the scheduler queued the last step (and so completed the enrollment)
// still stops that step, is recorded, and keeps the contact out of the
// sequence even with re-entry "always"; other mail still reaches them.
func TestE2ESequenceStopCoversQueuedLastStep(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	seq := hs.createSequence(`{"name":"Drip","trigger_tag":"joined","reentry":"always"}`,
		dripStep{"Drip one", 0}, dripStep{"Drip two", 86400})
	c := hs.createContact(`{"email":"stop@example.org","tags":["joined"]}`)
	hs.schedule()
	hs.sendAll()

	hs.clock.Advance(day)
	hs.schedule() // queues "Drip two" and completes the enrollment
	if st, _ := hs.enrollment(seq.ID, c.ID); st != sequences.EnrollmentCompleted {
		t.Fatalf("enrollment = %s, want completed", st)
	}
	path := hs.mailTo(c.Email)[0].unsubscribePath(t)
	hs.raw("GET", path, "", "").want(t, http.StatusOK)
	hs.raw("POST", path, "application/x-www-form-urlencoded", "scope="+events.ScopeSequence).want(t, http.StatusOK)
	if st, reason := hs.enrollment(seq.ID, c.ID); st != sequences.EnrollmentExited || reason != sequences.ExitUnsubscribed {
		t.Fatalf("enrollment = %s %s", st, reason)
	}
	if n := hs.count(`SELECT count(*) FROM events WHERE type = 'unsubscribe' AND subtype = 'link'`); n != 1 {
		t.Fatalf("unsubscribe events = %d", n)
	}
	hs.sendAll()
	if got := hs.subjectsTo(c.Email); len(got) != 1 || got[0] != "Drip one" {
		t.Fatalf("got %q, want only the first step", got)
	}

	// Re-adding the trigger tag doesn't start the drip again.
	hs.api("DELETE", fmt.Sprintf("/contacts/%d/tags/joined", c.ID), "").want(t, http.StatusOK)
	hs.api("POST", fmt.Sprintf("/contacts/%d/tags", c.ID), `{"tags":["joined"]}`).want(t, http.StatusOK)
	hs.clock.Advance(day)
	hs.schedule()
	hs.sendAll()
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE contact_id = ?`, c.ID); n != 1 {
		t.Fatalf("enrollments = %d, want 1", n)
	}
	if st := hs.contactStatus(c.ID); st != core.StatusActive {
		t.Fatalf("status = %s: a sequence stop must not unsubscribe", st)
	}
	b := hs.createBroadcast("News", "Still news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b.ID); n != 1 {
		t.Fatalf("broadcast recipients = %d", n)
	}
	hs.sendAll()
	if got := hs.subjectsTo(c.Email); len(got) != 2 || got[1] != "Still news" {
		t.Fatalf("got %q", got)
	}
}

// TestE2EOptOutIsNeverLost covers the other ways an opt-out reaches a
// contact that can't become unsubscribed, each followed by the admin
// lifting the block (U-3):
//   - the footer link (GET page, then its button) from a suppressed
//     contact: the page offers the button, and the POST records it;
//   - an unsubscribed contact that bounces later (a late SES bounce);
//   - an admin passing on a request received by phone for a bounced
//     contact (set-contact-status unsubscribed keeps bounced, records it);
//   - an unsubscribed contact that an import marks bounced.
//
// All three end unsubscribed, and the next broadcast reaches nobody.
func TestE2EOptOutIsNeverLost(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	link := hs.createContact(`{"email":"link@example.org"}`)
	late := hs.createContact(`{"email":"late@example.org"}`)
	phone := hs.createContact(`{"email":"phone@example.org"}`)
	b1 := hs.createBroadcast("First", "First news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b1.ID); n != 3 {
		t.Fatalf("recipients = %d", n)
	}
	hs.sendAll()
	type setStatus struct {
		Contact contacts.Contact `json:"contact"`
		Changed bool             `json:"changed"`
	}
	status := func(id int64, body string) setStatus {
		t.Helper()
		var out setStatus
		hs.api("POST", fmt.Sprintf("/contacts/%d/status", id), body).want(t, http.StatusOK).decode(t, &out)
		return out
	}

	// 1. The link path of a suppressed contact.
	hs.api("POST", "/suppressions", `{"email":"link@example.org","note":"blocked"}`).want(t, http.StatusCreated)
	path := hs.mailTo(link.Email)[0].unsubscribePath(t)
	page := hs.raw("GET", path, "", "").want(t, http.StatusOK)
	if !strings.Contains(string(page.body), `value="all"`) || strings.Contains(string(page.body), "Only stop these emails") {
		t.Fatalf("GET page of a suppressed contact must offer the Unsubscribe button: %s", page.body)
	}
	if n := hs.count(`SELECT count(*) FROM contacts WHERE id = ? AND unsubscribe_requested_at IS NOT NULL`, link.ID); n != 0 {
		t.Fatal("the GET page must not change anything")
	}
	hs.raw("POST", path, "application/x-www-form-urlencoded", "scope=all").want(t, http.StatusOK)
	if page := hs.raw("GET", path, "", "").want(t, http.StatusOK); strings.Contains(string(page.body), "<form") {
		t.Fatalf("GET after the request is recorded should show the result page: %s", page.body)
	}
	var rm contacts.UnsuppressResult
	hs.api("POST", "/suppressions/remove", `{"email":"link@example.org"}`).want(t, http.StatusOK).decode(t, &rm)
	if rm.Status != core.StatusUnsubscribed {
		t.Fatalf("lifted suppression = %+v, want unsubscribed", rm)
	}

	// 2. Unsubscribed, then a late permanent bounce of the first message.
	hs.raw("POST", hs.mailTo(late.Email)[0].unsubscribePath(t), "application/x-www-form-urlencoded",
		"List-Unsubscribe=One-Click").want(t, http.StatusOK)
	msg := hs.messageIDs(late.ID, core.KindBroadcast)[0]
	hs.notify("sns-late-bounce", hs.bounceEvent(hs.sesMessageID(msg), "Permanent", late.Email), "2")
	if st := hs.contactStatus(late.ID); st != core.StatusBounced {
		t.Fatalf("late = %s, want bounced", st)
	}
	if got := status(late.ID, `{"status":"active","note":"mailbox fixed"}`); got.Contact.Status != core.StatusUnsubscribed {
		t.Fatalf("reactivated late bouncer = %s, want unsubscribed", got.Contact.Status)
	}

	// 3. An admin records a phone request for a bounced contact.
	msg = hs.messageIDs(phone.ID, core.KindBroadcast)[0]
	hs.notify("sns-phone-bounce", hs.bounceEvent(hs.sesMessageID(msg), "Permanent", phone.Email), "2")
	got := status(phone.ID, `{"status":"unsubscribed","note":"asked by phone"}`)
	if !got.Changed || got.Contact.Status != core.StatusBounced || got.Contact.UnsubscribeRequestedAt == nil {
		t.Fatalf("unsubscribe of a bounced contact = %+v, want bounced with the request recorded", got)
	}
	if again := status(phone.ID, `{"status":"unsubscribed"}`); again.Changed {
		t.Fatal("a second request must change nothing")
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'contact.optout_recorded' AND target_id = ?`,
		fmt.Sprint(phone.ID)); n != 1 {
		t.Fatalf("audit entries = %d, want 1", n)
	}
	if got := status(phone.ID, `{"status":"active","note":"mailbox fixed"}`); got.Contact.Status != core.StatusUnsubscribed {
		t.Fatalf("reactivated phone contact = %s, want unsubscribed", got.Contact.Status)
	}

	// 4. Unsubscribed, then an import says bounced.
	imp := hs.createContact(`{"email":"import@example.org"}`)
	status(imp.ID, `{"status":"unsubscribed"}`)
	if rep := hs.importCSV("email,status\nimport@example.org,bounced\n", false); rep.Updated != 1 {
		t.Fatalf("import = %+v", rep)
	}
	if got := status(imp.ID, `{"status":"active","note":"mailbox fixed"}`); got.Contact.Status != core.StatusUnsubscribed {
		t.Fatalf("reactivated imported bouncer = %s, want unsubscribed", got.Contact.Status)
	}

	b2 := hs.createBroadcast("Second", "Second news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b2.ID); n != 0 {
		t.Fatalf("second broadcast recipients = %d, want 0", n)
	}
}

// TestE2EPublicRateLimits: the unsubscribe page and the media files are
// rate-limited per client address, but the unsubscribe POST never is:
// mail providers send one-click unsubscribes for many people from a few
// shared addresses, and not one may be refused.
func TestE2EPublicRateLimits(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	var paths []string
	csv := "email\n"
	for i := range events.DefaultPageBurst + 5 {
		csv += fmt.Sprintf("p%d@example.org\n", i)
	}
	hs.importCSV(csv, false)
	b := hs.createBroadcast("News", "News", `{"type":"all"}`)
	hs.sendBroadcast(b.ID)
	hs.sendAll()
	for i := range events.DefaultPageBurst + 5 {
		paths = append(paths, hs.mailTo(fmt.Sprintf("p%d@example.org", i))[0].unsubscribePath(t))
	}

	limited := false
	for _, p := range paths {
		if hs.raw("GET", p, "", "").code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatalf("%d unsubscribe page views from one address were never limited", len(paths))
	}
	for _, p := range paths {
		hs.raw("POST", p, "application/x-www-form-urlencoded", "List-Unsubscribe=One-Click").want(t, http.StatusOK)
	}
	if n := hs.count(`SELECT count(*) FROM contacts WHERE status = 'unsubscribed'`); n != len(paths) {
		t.Fatalf("unsubscribed = %d, want all %d", n, len(paths))
	}

	limited = false
	for range app.MediaBurst + 1 {
		if hs.raw("GET", "/media/0123456789abcdef0123456789abcdef.png", "", "").code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("media requests from one address were never limited")
	}
}
