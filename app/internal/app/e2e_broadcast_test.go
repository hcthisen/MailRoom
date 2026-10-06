package app_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"mailroom/internal/broadcasts"
	"mailroom/internal/core"
	"mailroom/internal/segments"
)

// contactsCSV is an import file of n active contacts named
// <prefix><i>@example.org with tags (semicolon-separated, may be empty).
func contactsCSV(prefix string, n int, tags string) string {
	var b strings.Builder
	b.WriteString("email,first_name,status,tags,consent_source,consent_date\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s%d@example.org,Reader %d,active,%s,ghl-form,2025-06-01T10:00:00Z\n", prefix, i, i, tags)
	}
	return b.String()
}

// broadcastMailTo counts the messages with subject the provider accepted
// for each address.
func (hs *harness) broadcastMailTo(subject string) map[string]int {
	hs.t.Helper()
	out := map[string]int{}
	for _, m := range hs.provider.Sent() {
		if parseMail(hs.t, m).Header.Get("Subject") == subject {
			out[m.To]++
		}
	}
	return out
}

// TestE2EBroadcastRestartNoDuplicates: a broadcast to a 200-contact segment
// is snapshotted, partly sent, the process restarts (a new App on the same
// database) and sending finishes: every member gets exactly one message,
// nobody outside the segment gets one, and the broadcast becomes sent.
func TestE2EBroadcastRestartNoDuplicates(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	rep := hs.importCSV(contactsCSV("vip", 200, "vip")+strings.TrimPrefix(contactsCSV("other", 30, ""), "email,first_name,status,tags,consent_source,consent_date\n"), false)
	if rep.Created != 230 || rep.RejectedCount != 0 {
		t.Fatalf("import = %+v", rep)
	}
	var seg segments.Segment
	hs.api("POST", "/segments", `{"name":"VIP","filter":{"rules":[{"field":"tag","op":"has","value":"vip"}]}}`).
		want(t, http.StatusCreated).decode(t, &seg)
	var counts segments.SegmentCounts
	hs.api("GET", fmt.Sprintf("/segments/%d/count", seg.ID), "").want(t, http.StatusOK).decode(t, &counts)
	if counts.Active != 200 {
		t.Fatalf("segment count = %+v", counts)
	}

	const subject = "VIP news"
	b := hs.createBroadcast("VIP news", subject, fmt.Sprintf(`{"type":"segment","segment_id":%d}`, seg.ID))
	// The dry run reports the audience and changes nothing.
	var dry struct {
		DryRun     bool  `json:"dry_run"`
		Recipients int64 `json:"recipients"`
	}
	hs.api("POST", fmt.Sprintf("/broadcasts/%d/send?dry_run=true", b.ID), "").want(t, http.StatusOK).decode(t, &dry)
	if !dry.DryRun || dry.Recipients != 200 || hs.getBroadcast(b.ID).Status != broadcasts.StatusDraft {
		t.Fatalf("dry run = %+v", dry)
	}
	if n := hs.sendBroadcast(b.ID); n != 200 {
		t.Fatalf("recipients = %d", n)
	}
	if got := hs.getBroadcast(b.ID); got.Status != broadcasts.StatusSending || got.RecipientsTotal != 200 {
		t.Fatalf("after the snapshot: %+v", got)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ? AND status = 'queued'`, b.ID); n != 200 {
		t.Fatalf("queued = %d", n)
	}

	hs.sendSome(80)
	hs.restart()
	hs.tick() // the starter after a restart must not snapshot again
	hs.sendAll()

	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ?`, b.ID); n != 200 {
		t.Fatalf("messages = %d, want 200", n)
	}
	if n := hs.count(`SELECT count(*) FROM (SELECT contact_id FROM messages WHERE broadcast_id = ? GROUP BY contact_id HAVING count(*) > 1)`, b.ID); n != 0 {
		t.Fatalf("%d contacts have more than one message", n)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ? AND status = 'sent'`, b.ID); n != 200 {
		t.Fatalf("sent = %d", n)
	}
	got := hs.broadcastMailTo(subject)
	if len(got) != 200 {
		t.Fatalf("%d addresses got the broadcast, want 200", len(got))
	}
	for addr, n := range got {
		if n != 1 || !strings.HasPrefix(addr, "vip") {
			t.Fatalf("%s got %d copies", addr, n)
		}
	}
	if st := hs.getBroadcast(b.ID); st.Status != broadcasts.StatusSent {
		t.Fatalf("broadcast status = %s", st.Status)
	}
	var stats broadcasts.BroadcastStats
	hs.api("GET", fmt.Sprintf("/broadcasts/%d/stats", b.ID), "").want(t, http.StatusOK).decode(t, &stats)
	if stats.Recipients != 200 || stats.Sent != 200 || stats.Queued != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

// TestE2EOneClickUnsubscribe: the RFC 8058 one-click POST to the
// List-Unsubscribe URL of a broadcast email unsubscribes the contact at
// once (the GET page changes nothing), records the source and message, and
// the next broadcast leaves the contact out.
func TestE2EOneClickUnsubscribe(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	gina := hs.createContact(`{"email":"gina@example.org","first_name":"Gina"}`)
	hs.createContact(`{"email":"hank@example.org","first_name":"Hank"}`)

	b1 := hs.createBroadcast("June", "June news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b1.ID); n != 2 {
		t.Fatalf("first broadcast recipients = %d", n)
	}
	hs.sendAll()
	mails := hs.mailTo("gina@example.org")
	if len(mails) != 1 {
		t.Fatalf("gina got %d emails", len(mails))
	}
	unsub := mails[0].unsubscribePath(t)

	hs.raw("GET", unsub, "", "").want(t, http.StatusOK)
	if st := hs.contactStatus(gina.ID); st != core.StatusActive {
		t.Fatalf("the unsubscribe page changed the status to %s", st)
	}
	hs.raw("POST", unsub, "application/x-www-form-urlencoded", "List-Unsubscribe=One-Click").want(t, http.StatusOK)
	if st := hs.contactStatus(gina.ID); st != core.StatusUnsubscribed {
		t.Fatalf("after one-click: %s", st)
	}
	msgIDs := hs.messageIDs(gina.ID, core.KindBroadcast)
	if n := hs.count(`SELECT count(*) FROM contact_status_changes WHERE contact_id = ? AND to_status = 'unsubscribed' AND source = 'one_click' AND message_id = ?`,
		gina.ID, msgIDs[0]); n != 1 {
		t.Fatalf("one-click status changes = %d", n)
	}
	// A repeated one-click (mail clients retry) is still 200 and changes nothing.
	hs.raw("POST", unsub, "application/x-www-form-urlencoded", "List-Unsubscribe=One-Click").want(t, http.StatusOK)
	if n := hs.count(`SELECT count(*) FROM contact_status_changes WHERE contact_id = ?`, gina.ID); n != 1 {
		t.Fatalf("status changes after a repeat = %d", n)
	}

	var stats broadcasts.BroadcastStats
	hs.api("GET", fmt.Sprintf("/broadcasts/%d/stats", b1.ID), "").want(t, http.StatusOK).decode(t, &stats)
	if stats.Unsubscribed != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	b2 := hs.createBroadcast("July", "July news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b2.ID); n != 1 {
		t.Fatalf("second broadcast recipients = %d, want 1", n)
	}
	hs.sendAll()
	if got := hs.subjectsTo("gina@example.org"); len(got) != 1 {
		t.Fatalf("gina got %q after unsubscribing", got)
	}
	if got := hs.subjectsTo("hank@example.org"); len(got) != 2 || got[1] != "July news" {
		t.Fatalf("hank got %q", got)
	}
}

// TestE2ECircuitBreaker: when the hard-bounce rate of a sending broadcast
// goes over the threshold after the minimum number of messages (SNS bounce
// notifications), the broadcast starter pauses it with reason
// circuit_breaker and alerts the owner; nothing more of it is sent until a
// confirmed resume, after which it finishes.
func TestE2ECircuitBreaker(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	etag := hs.api("GET", "/settings", "").want(t, http.StatusOK).header.Get("ETag")
	hs.api("PATCH", "/settings", `{"circuit_breaker":{"min_messages":20}}`, "If-Match", etag).want(t, http.StatusOK)
	if rep := hs.importCSV(contactsCSV("reader", 40, ""), false); rep.Created != 40 {
		t.Fatalf("import = %+v", rep)
	}
	b := hs.createBroadcast("Risky list", "Risky news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b.ID); n != 40 {
		t.Fatalf("recipients = %d", n)
	}
	hs.sendSome(20)
	hs.tick()
	if st := hs.getBroadcast(b.ID).Status; st != broadcasts.StatusSending {
		t.Fatalf("paused without bounces: %s", st)
	}

	// 3 of 20 hard-bounce: 15 % is over the 5 % default.
	rows, err := hs.app.DB.Read.Query(`SELECT id, to_email FROM messages WHERE broadcast_id = ? AND status = 'sent' ORDER BY id LIMIT 3`, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	type sentMsg struct {
		id int64
		to string
	}
	var bounced []sentMsg
	for rows.Next() {
		var m sentMsg
		if err := rows.Scan(&m.id, &m.to); err != nil {
			t.Fatal(err)
		}
		bounced = append(bounced, m)
	}
	rows.Close()
	for i, m := range bounced {
		hs.notify(fmt.Sprintf("sns-bounce-%d", i), hs.bounceEvent(hs.sesMessageID(m.id), "Permanent", m.to), "2")
	}
	hs.tick()
	got := hs.getBroadcast(b.ID)
	if got.Status != broadcasts.StatusPaused || got.PausedReason != broadcasts.PauseCircuitBreaker {
		t.Fatalf("after the bounces: %+v", got)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'alert' AND to_email = 'owner@example.org'`); n != 1 {
		t.Fatalf("alerts to the owner = %d", n)
	}

	before := hs.provider.Count()
	hs.sendAll()
	if n := hs.provider.Count() - before; n != 1 {
		t.Fatalf("sent %d while paused, want only the alert", n)
	}
	if alert := hs.mailTo("owner@example.org"); len(alert) != 1 || alert[0].Header.Get("List-Unsubscribe") != "" {
		t.Fatalf("owner alert = %d emails", len(alert))
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ? AND status = 'queued'`, b.ID); n != 20 {
		t.Fatalf("queued while paused = %d", n)
	}

	hs.api("POST", fmt.Sprintf("/broadcasts/%d/resume", b.ID), "").want(t, http.StatusUnprocessableEntity)
	hs.clock.Advance(10 * time.Minute) // the owner looks into it
	hs.api("POST", fmt.Sprintf("/broadcasts/%d/resume", b.ID), `{"confirm":true}`).want(t, http.StatusOK)
	hs.tick() // judged only on messages sent after the resume: no new trip
	if st := hs.getBroadcast(b.ID).Status; st != broadcasts.StatusSending {
		t.Fatalf("after resume: %s", st)
	}
	hs.sendAll()
	if got := hs.getBroadcast(b.ID); got.Status != broadcasts.StatusSent {
		t.Fatalf("after finishing: %+v", got)
	}
	if n := len(hs.broadcastMailTo("Risky news")); n != 40 {
		t.Fatalf("%d recipients got the broadcast", n)
	}
	for _, m := range bounced {
		if c := hs.mustContact(m.to); c.Status != core.StatusBounced {
			t.Fatalf("%s = %s", m.to, c.Status)
		}
	}
}

// TestE2EBroadcastCrashMidSend: the process dies while a message is being
// handed to the provider (its row is left "sending"). After the restart the
// sender first marks that message failed, because the provider may have
// accepted it, and never sends it again; the rest of the broadcast goes out
// once and the broadcast completes.
func TestE2EBroadcastCrashMidSend(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	if rep := hs.importCSV(contactsCSV("reader", 20, ""), false); rep.Created != 20 {
		t.Fatalf("import = %+v", rep)
	}
	b := hs.createBroadcast("Crash test", "Crash news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b.ID); n != 20 {
		t.Fatalf("recipients = %d", n)
	}
	hs.sendSome(5)
	// The crash: the sender had claimed the next message (queued -> sending)
	// when the process died.
	var inFlight int64
	var inFlightTo string
	if err := hs.app.DB.Write.QueryRow(`
		UPDATE messages SET status = 'sending' WHERE id = (
			SELECT id FROM messages WHERE broadcast_id = ? AND status = 'queued' ORDER BY id LIMIT 1)
		RETURNING id, to_email`, b.ID).Scan(&inFlight, &inFlightTo); err != nil {
		t.Fatal(err)
	}

	hs.restart()
	// What the sender worker does first when it starts.
	if n, err := hs.app.Sending.RecoverInterrupted(hs.ctx); err != nil || n != 1 {
		t.Fatalf("recovered %d (%v), want 1", n, err)
	}
	hs.tick()
	hs.sendAll()

	var status, errText string
	if err := hs.app.DB.Read.QueryRow(`SELECT status, error FROM messages WHERE id = ?`, inFlight).Scan(&status, &errText); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || errText == "" {
		t.Fatalf("the interrupted message is %s (%q), want failed", status, errText)
	}
	got := hs.broadcastMailTo("Crash news")
	if len(got) != 19 || got[inFlightTo] != 0 {
		t.Fatalf("%d addresses got the broadcast (in flight: %d), want 19 and 0", len(got), got[inFlightTo])
	}
	for addr, n := range got {
		if n != 1 {
			t.Fatalf("%s got %d copies", addr, n)
		}
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ?`, b.ID); n != 20 {
		t.Fatalf("messages = %d", n)
	}
	if st := hs.getBroadcast(b.ID).Status; st != broadcasts.StatusSent {
		t.Fatalf("broadcast = %s", st)
	}
}
