package app_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/sequences"
)

// TestE2EBounceAndComplaint: SES events arrive as signed SNS notifications
// (SignatureVersion 1 and 2, certificate from the injected fetcher). A
// permanent bounce makes the contact bounced and a complaint complained;
// their drips exit before the next step and no further mail goes to them.
// Redelivered notifications count once, and forged ones are refused.
func TestE2EBounceAndComplaint(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	seq := hs.createSequence(`{"name":"Onboarding","trigger_tag":"joined"}`,
		dripStep{"Step one", 0}, dripStep{"Step two", 86400}, dripStep{"Step three", 86400})
	dave := hs.createContact(`{"email":"dave@example.org","tags":["joined"]}`)
	erin := hs.createContact(`{"email":"erin@example.org","tags":["joined"]}`)
	hs.createContact(`{"email":"frank@example.org"}`)
	hs.schedule()
	hs.sendAll()

	daveMsg := hs.messageIDs(dave.ID, core.KindSequence)[0]
	bounce := hs.snsEnvelope("sns-1", hs.bounceEvent(hs.sesMessageID(daveMsg), "Permanent", "dave@example.org"), "1")
	hs.postSNS(bounce).want(t, http.StatusOK)
	hs.postSNS(bounce).want(t, http.StatusOK) // SNS redelivers: counted once (X-6)
	if n := hs.count(`SELECT count(*) FROM events WHERE message_id = ? AND type = 'bounce' AND subtype = 'permanent'`, daveMsg); n != 1 {
		t.Fatalf("bounce events = %d", n)
	}
	if st := hs.contactStatus(dave.ID); st != core.StatusBounced {
		t.Fatalf("dave = %s", st)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE id = ? AND status = 'bounced'`, daveMsg); n != 1 {
		t.Fatal("the bounced message is not marked bounced")
	}
	if n := hs.count(`SELECT count(*) FROM contact_status_changes WHERE contact_id = ? AND source = 'bounce' AND message_id = ?`, dave.ID, daveMsg); n != 1 {
		t.Fatalf("bounce status changes = %d", n)
	}
	if st, reason := hs.enrollment(seq.ID, dave.ID); st != sequences.EnrollmentExited || reason != sequences.ExitNotActive {
		t.Fatalf("dave's enrollment = %s %s", st, reason)
	}

	// A forged notification (changed after signing) is refused and changes
	// nothing.
	erinMsg := hs.messageIDs(erin.ID, core.KindSequence)[0]
	forged := hs.snsEnvelope("sns-2", hs.bounceEvent(hs.sesMessageID(erinMsg), "Transient", "erin@example.org"), "2")
	forged["Message"] = strings.Replace(forged["Message"].(string), "Transient", "Permanent", 1)
	hs.postSNS(forged).want(t, http.StatusForbidden)
	if st := hs.contactStatus(erin.ID); st != core.StatusActive {
		t.Fatalf("a forged notification changed erin to %s", st)
	}
	// A validly signed notification from another topic is refused too: only
	// the topic pin (X-2) can reject it.
	foreign := hs.snsEnvelope("sns-3", hs.complaintEvent(hs.sesMessageID(erinMsg), "erin@example.org"), "2",
		func(e map[string]any) { e["TopicArn"] = "arn:aws:sns:eu-west-1:999999999999:other" })
	hs.postSNS(foreign).want(t, http.StatusForbidden)
	if st := hs.contactStatus(erin.ID); st != core.StatusActive {
		t.Fatalf("a foreign topic's complaint changed erin to %s", st)
	}
	// The same notification from the pinned topic is accepted (below, as
	// sns-4), so the 403 came from the topic, not the signature.

	hs.notify("sns-4", hs.complaintEvent(hs.sesMessageID(erinMsg), "erin@example.org"), "2")
	if st := hs.contactStatus(erin.ID); st != core.StatusComplained {
		t.Fatalf("erin = %s", st)
	}
	if st, reason := hs.enrollment(seq.ID, erin.ID); st != sequences.EnrollmentExited || reason != sequences.ExitNotActive {
		t.Fatalf("erin's enrollment = %s %s", st, reason)
	}

	for i := 0; i < 3; i++ {
		hs.clock.Advance(day)
		hs.schedule()
		hs.sendAll()
	}
	b := hs.createBroadcast("News", "Monthly news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b.ID); n != 1 {
		t.Fatalf("broadcast recipients = %d, want only frank", n)
	}
	hs.sendAll()
	for _, addr := range []string{"dave@example.org", "erin@example.org"} {
		if got := hs.subjectsTo(addr); len(got) != 1 {
			t.Fatalf("%s got %q after the bounce/complaint", addr, got)
		}
	}
	if got := hs.subjectsTo("frank@example.org"); len(got) != 1 || got[0] != "Monthly news" {
		t.Fatalf("frank got %q", got)
	}
}

// TestE2EEverySuppressionPath: contacts who unsubscribed (one-click),
// bounced and complained (SNS), were suppressed manually or erased never
// get the sequence step or the broadcast that was already queued for them
// (the final check before the provider, D-6), are left out of the next
// broadcast, and an erased or manually suppressed address can't come back
// through the form, an import or the API.
func TestE2EEverySuppressionPath(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	seq := hs.createSequence(`{"name":"Drip","trigger_tag":"joined"}`,
		dripStep{"Drip one", 0}, dripStep{"Drip two", 86400}, dripStep{"Drip three", 86400})
	f := hs.createForm(`{"name":"Sign-up","tags":["joined"]}`)
	who := []string{"active", "unsub", "bounce", "complain", "manual", "erase"}
	ids := map[string]int64{}
	for _, w := range who {
		ids[w] = hs.createContact(fmt.Sprintf(`{"email":"%s@example.org","tags":["joined"]}`, w)).ID
	}
	addr := func(w string) string { return w + "@example.org" }

	hs.schedule()
	hs.sendAll()
	for _, w := range who {
		if n := len(hs.mailTo(addr(w))); n != 1 {
			t.Fatalf("%s got %d first steps", w, n)
		}
	}
	// Step two and a broadcast are queued for everyone while all are active.
	hs.clock.Advance(day)
	hs.schedule()
	b := hs.createBroadcast("Queued", "Queued news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b.ID); n != 6 {
		t.Fatalf("recipients = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE status = 'queued'`); n != 12 {
		t.Fatalf("queued = %d, want 12", n)
	}

	// Now every suppression path.
	first := func(w string) int64 { return hs.messageIDs(ids[w], core.KindSequence)[0] }
	unsub := hs.mailTo(addr("unsub"))[0].unsubscribePath(t)
	hs.raw("POST", unsub, "application/x-www-form-urlencoded", "List-Unsubscribe=One-Click").want(t, http.StatusOK)
	hs.notify("sns-b", hs.bounceEvent(hs.sesMessageID(first("bounce")), "Permanent", addr("bounce")), "1")
	hs.notify("sns-c", hs.complaintEvent(hs.sesMessageID(first("complain")), addr("complain")), "2")
	hs.api("POST", "/suppressions", `{"email":"manual@example.org","note":"asked by phone"}`).want(t, http.StatusCreated)
	var erased contacts.EraseResult
	hs.api("DELETE", fmt.Sprintf("/contacts/%d", ids["erase"]), "").want(t, http.StatusOK).decode(t, &erased)

	want := map[string]core.ContactStatus{
		"active": core.StatusActive, "unsub": core.StatusUnsubscribed, "bounce": core.StatusBounced,
		"complain": core.StatusComplained, "manual": core.StatusSuppressedManual,
	}
	for w, st := range want {
		if got := hs.contactStatus(ids[w]); got != st {
			t.Fatalf("%s = %s, want %s", w, got, st)
		}
	}

	hs.sendAll()
	if got := hs.subjectsTo(addr("active")); len(got) != 3 {
		t.Fatalf("the active contact got %q", got)
	}
	for _, w := range who[1:] {
		if got := hs.subjectsTo(addr(w)); len(got) != 1 {
			t.Fatalf("suppressed %s got %q", w, got)
		}
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE status = 'skipped'`); n != 10 {
		t.Fatalf("skipped = %d, want 10 (2 each for 5 suppressed contacts)", n)
	}
	for _, w := range []string{"unsub", "bounce", "complain", "manual"} {
		if st, reason := hs.enrollment(seq.ID, ids[w]); st != sequences.EnrollmentExited || reason != sequences.ExitNotActive {
			t.Fatalf("%s enrollment = %s %s", w, st, reason)
		}
	}
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE contact_id = ?`, ids["erase"]); n != 0 {
		t.Fatalf("the erased contact keeps %d enrollments", n)
	}
	// The last step goes to the active contact only.
	hs.clock.Advance(day)
	hs.schedule()
	hs.sendAll()
	if got := hs.subjectsTo(addr("active")); len(got) != 4 || got[3] != "Drip three" {
		t.Fatalf("the active contact got %q", got)
	}
	for _, w := range who[1:] {
		if got := hs.subjectsTo(addr(w)); len(got) != 1 {
			t.Fatalf("suppressed %s got %q", w, got)
		}
	}

	b2 := hs.createBroadcast("Next", "Next news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b2.ID); n != 1 {
		t.Fatalf("next broadcast recipients = %d, want 1", n)
	}
	hs.sendAll()
	if n := len(hs.broadcastMailTo("Next news")); n != 1 {
		t.Fatalf("next broadcast went to %d addresses", n)
	}

	// Test sends (B-7) carry no contact, yet they skip every address whose
	// contact opted out, bounced, complained or was suppressed.
	activeBefore := len(hs.mailTo(addr("active")))
	hs.api("POST", fmt.Sprintf("/broadcasts/%d/test", b2.ID),
		`{"to":["active@example.org","unsub@example.org","bounce@example.org","complain@example.org","manual@example.org"]}`).
		want(t, http.StatusAccepted)
	hs.sendAll()
	if n := len(hs.mailTo(addr("active"))); n != activeBefore+1 {
		t.Fatalf("the active contact got %d test sends", n-activeBefore)
	}
	for _, w := range who[1:] {
		if got := hs.subjectsTo(addr(w)); len(got) != 1 {
			t.Fatalf("suppressed %s got %q after a test send", w, got)
		}
	}

	// Coming back: the form answers as usual but adds nobody (F-7); an
	// unsubscribed address needs a confirmation; import and API refuse.
	for _, w := range []string{"erase", "manual", "bounce", "complain", "unsub"} {
		hs.raw("POST", "/f/"+f.PublicID, "application/json", fmt.Sprintf(`{"email":%q}`, addr(w))).want(t, http.StatusOK)
	}
	if _, ok := hs.contactByEmail(addr("erase")); ok {
		t.Fatal("the form re-added an erased address")
	}
	for w, st := range want {
		if got := hs.contactStatus(ids[w]); got != st {
			t.Fatalf("after the form %s = %s, want %s", w, got, st)
		}
	}
	if n := hs.count(`SELECT count(*) FROM form_submissions WHERE outcome = 'rejected'`); n != 4 {
		t.Fatalf("rejected submissions = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM form_submissions WHERE outcome = 'resubscribe_pending'`); n != 1 {
		t.Fatalf("resubscribe submissions = %d", n)
	}

	rep := hs.importCSV("email,status\nerase@example.org,active\nmanual@example.org,active\nnew@example.org,active\n", false)
	if rep.Created != 1 || rep.RejectedCount != 2 || len(rep.Rejected) != 2 {
		t.Fatalf("import = %+v", rep)
	}
	for _, r := range rep.Rejected {
		if !strings.Contains(r.Reason, "suppress") {
			t.Fatalf("rejection = %+v", r)
		}
	}
	hs.api("POST", "/contacts", `{"email":"erase@example.org"}`).want(t, http.StatusConflict)
	if _, ok := hs.contactByEmail(addr("erase")); ok {
		t.Fatal("an erased address came back")
	}
}

// TestE2EGDPRExportAndErase: a contact who signed up, got a drip email,
// clicked a link and had a delivery event is exported with all of it, then
// erased: afterwards no table holds the address or its attribute values,
// counts survive anonymised, the hash blocks the address, and an
// Idempotency-Key replay can't return the erased record.
func TestE2EGDPRExportAndErase(t *testing.T) {
	hs := newHarness(t, nil)
	const email = "ivy.gdpr@example.org"
	seq := hs.createSequence(`{"name":"Hello"}`, dripStep{"Hello {{first_name | \"there\"}}", 0})
	f := hs.createForm(fmt.Sprintf(`{"name":"Leads","fields":[{"key":"first_name"},{"key":"attr.company","label":"Company"}],"tags":["lead"],"sequence_id":%d}`, seq.ID))
	hs.raw("POST", "/f/"+f.PublicID, "application/json",
		fmt.Sprintf(`{"email":%q,"first_name":"Ivy","attr.company":"Quasar Widgets Ltd"}`, email)).want(t, http.StatusOK)
	ivy := hs.mustContact(email)
	upsert := fmt.Sprintf(`{"email":%q,"tags":["vip"]}`, email)
	hs.api("POST", "/contacts/upsert", upsert, "Idempotency-Key", "gdpr-upsert-1").want(t, http.StatusOK)

	hs.schedule()
	hs.sendAll()
	mails := hs.mailTo(email)
	if len(mails) != 1 {
		t.Fatalf("ivy got %d emails", len(mails))
	}
	click := mails[0].links("/c/")
	if len(click) == 0 {
		t.Fatal("no tracked link")
	}
	if r := hs.raw("GET", click[0], "", ""); r.code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), "https://example.org/start-1") {
		t.Fatalf("click = %d %s", r.code, r.header.Get("Location"))
	}
	msg := hs.messageIDs(ivy.ID, core.KindSequence)[0]
	hs.notify("sns-d", hs.deliveryEvent(hs.sesMessageID(msg), email), "2")

	var exp contacts.Export
	hs.api("GET", fmt.Sprintf("/contacts/%d/export", ivy.ID), "").want(t, http.StatusOK).decode(t, &exp)
	if exp.Contact.Email != email || exp.Contact.Attributes["company"] != "Quasar Widgets Ltd" ||
		len(exp.ConsentLog) == 0 || len(exp.Messages) != 1 || len(exp.Clicks) != 1 || len(exp.Tags) != 2 || len(exp.Enrollments) != 1 {
		t.Fatalf("export = %+v", exp)
	}
	if found := hs.tablesContaining(email); len(found) == 0 {
		t.Fatal("the scan finds nothing before the erase; it would prove nothing after it")
	}

	var dry contacts.EraseResult
	hs.api("DELETE", fmt.Sprintf("/contacts/%d?dry_run=true", ivy.ID), "").want(t, http.StatusOK).decode(t, &dry)
	if !dry.DryRun || dry.Deleted.Contact != 1 || dry.Anonymised.Messages != 1 || dry.Anonymised.Clicks != 1 {
		t.Fatalf("dry-run erase = %+v", dry)
	}
	hs.api("GET", fmt.Sprintf("/contacts/%d", ivy.ID), "").want(t, http.StatusOK)

	// Test sends of the step with Ivy's data, to her address and to the
	// owner's: neither carries her contact ID, both hold her data.
	hs.api("POST", fmt.Sprintf("/emails/%d/test", seq.Steps[0].EmailID),
		fmt.Sprintf(`{"to":[%q,"owner@example.org"],"contact_id":%d}`, email, ivy.ID)).want(t, http.StatusAccepted)
	hs.sendAll()
	if got := hs.subjectsTo(email); len(got) != 2 || got[1] != "Hello Ivy" {
		t.Fatalf("ivy got %q", got)
	}
	if found := hs.tablesContaining("Hello Ivy"); len(found) == 0 {
		t.Fatal("the rendered test subject is not stored; the scan below would prove nothing")
	}

	var res contacts.EraseResult
	hs.api("DELETE", fmt.Sprintf("/contacts/%d", ivy.ID), "").want(t, http.StatusOK).decode(t, &res)
	if res.Anonymised.Messages != 3 {
		t.Errorf("anonymised messages = %d, want the drip email and both test sends", res.Anonymised.Messages)
	}
	for _, needle := range []string{email, "Quasar Widgets Ltd", "Hello Ivy"} {
		if found := hs.tablesContaining(needle); len(found) > 0 {
			t.Errorf("%q is still in %v after the erase", needle, found)
		}
	}
	if n := hs.count(`SELECT count(*) FROM suppressions WHERE email_hash = ? AND reason = 'erased'`, core.EmailHash(email)); n != 1 {
		t.Fatal("no erased suppression for the address")
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE id = ? AND contact_id IS NULL AND to_email IS NULL`, msg); n != 1 {
		t.Fatal("the message row is not kept anonymised")
	}
	if n := hs.count(`SELECT count(*) FROM clicks WHERE message_id = ? AND contact_id IS NULL`, msg); n != 1 {
		t.Fatal("the click is not kept anonymised")
	}
	hs.api("GET", fmt.Sprintf("/contacts/%d", ivy.ID), "").want(t, http.StatusNotFound)
	hs.api("GET", fmt.Sprintf("/contacts/%d/export", ivy.ID), "").want(t, http.StatusNotFound)
	hs.api("POST", "/contacts/upsert", upsert, "Idempotency-Key", "gdpr-upsert-1").want(t, http.StatusConflict)
	hs.raw("POST", "/f/"+f.PublicID, "application/json", fmt.Sprintf(`{"email":%q}`, email)).want(t, http.StatusOK)
	if _, ok := hs.contactByEmail(email); ok {
		t.Fatal("the form re-added the erased address")
	}
}
