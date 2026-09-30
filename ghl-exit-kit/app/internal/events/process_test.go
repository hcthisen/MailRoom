package events_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/events"
	"mailroom/internal/testutil"
)

// notify posts a signed Notification carrying the SES event.
func (h *harness) notify(snsID string, event map[string]any) {
	h.t.Helper()
	rec := h.postSNS(snsMessage(h.t, events.TypeNotification, snsID, sesJSON(h.t, event), "2"))
	if rec.Code != http.StatusOK {
		h.t.Fatalf("notify %s: status = %d (%s)", snsID, rec.Code, rec.Body)
	}
}

func TestBounceAndComplaintRules(t *testing.T) {
	tests := []struct {
		name        string
		event       func(sesID, addr string) map[string]any
		wantContact core.ContactStatus
		wantMessage string
		wantType    string
		wantSubtype string
		wantSource  string
	}{
		{name: "permanent bounce", event: func(id, a string) map[string]any { return bounceEvent(id, "Permanent", a) },
			wantContact: core.StatusBounced, wantMessage: "bounced", wantType: "bounce", wantSubtype: "permanent", wantSource: "bounce"},
		// Soft bounces are events rows only: the message stays sent, so
		// "bounced" in the stats means hard-bounced.
		{name: "transient bounce", event: func(id, a string) map[string]any { return bounceEvent(id, "Transient", a) },
			wantContact: core.StatusActive, wantMessage: "sent", wantType: "bounce", wantSubtype: "transient"},
		{name: "undetermined bounce", event: func(id, a string) map[string]any { return bounceEvent(id, "Undetermined", a) },
			wantContact: core.StatusActive, wantMessage: "sent", wantType: "bounce", wantSubtype: "undetermined"},
		{name: "complaint", event: func(id, a string) map[string]any { return complaintEvent(id, a) },
			wantContact: core.StatusComplained, wantMessage: "complained", wantType: "complaint", wantSubtype: "abuse", wantSource: "complaint"},
		// A "not-spam" feedback report is recorded but suppresses nobody.
		{name: "not-spam feedback", event: func(id, a string) map[string]any { return notSpamEvent(id, a) },
			wantContact: core.StatusActive, wantMessage: "sent", wantType: "complaint", wantSubtype: "not-spam"},
		{name: "delivery", event: func(id, a string) map[string]any { return deliveryEvent(id, a) },
			wantContact: core.StatusActive, wantMessage: "delivered", wantType: "delivery"},
		{name: "reject", event: func(id, a string) map[string]any { return rejectEvent(id, a) },
			wantContact: core.StatusActive, wantMessage: "failed", wantType: "reject"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
			msgID := h.insertMessage("broadcast", c.ID, c.Email, "ses-1", 0)
			h.notify("sns-1", tt.event("ses-1", "Alice@Example.net"))

			if got := h.contactStatus(c.ID); got != tt.wantContact {
				t.Errorf("contact status = %s, want %s", got, tt.wantContact)
			}
			if got := h.messageStatus(msgID); got != tt.wantMessage {
				t.Errorf("message status = %s, want %s", got, tt.wantMessage)
			}
			var (
				typ, subtype, pid, detail string
				mid                       int64
			)
			if err := h.db.Read.QueryRow(`SELECT type, subtype, provider_event_id, detail, message_id FROM events`).
				Scan(&typ, &subtype, &pid, &detail, &mid); err != nil {
				t.Fatal(err)
			}
			if typ != tt.wantType || subtype != tt.wantSubtype || mid != msgID {
				t.Errorf("event = %s/%s message %d, want %s/%s message %d", typ, subtype, mid, tt.wantType, tt.wantSubtype, msgID)
			}
			if want := "sns-1:" + core.EmailHash("alice@example.net"); pid != want {
				t.Errorf("provider_event_id = %q, want %q", pid, want)
			}
			if detail == "{}" || len(detail) < 20 {
				t.Errorf("detail should hold the raw event, got %q", detail)
			}
			changes := h.changes()
			if tt.wantSource == "" {
				if len(changes) != 0 {
					t.Fatalf("unexpected status changes %+v", changes)
				}
				return
			}
			if len(changes) != 1 || changes[0].ContactID != c.ID || changes[0].Source != tt.wantSource || changes[0].MessageID != msgID {
				t.Fatalf("StatusChanged hooks = %+v", changes)
			}
			if n := h.count(`SELECT COUNT(*) FROM contact_status_changes WHERE contact_id = ? AND source = ? AND message_id = ?`,
				c.ID, tt.wantSource, msgID); n != 1 {
				t.Fatalf("contact_status_changes rows = %d", n)
			}
		})
	}
}

func TestDeliveredAtFromEvent(t *testing.T) {
	h := newHarness(t)
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	msgID := h.insertMessage("sequence", c.ID, c.Email, "ses-1", 0)
	h.notify("sns-1", deliveryEvent("ses-1", c.Email))
	var at string
	if err := h.db.Read.QueryRow(`SELECT delivered_at FROM messages WHERE id = ?`, msgID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	want := db.FormatTime(time.Date(2026, 1, 5, 9, 0, 1, 500_000_000, time.UTC))
	if at != want {
		t.Fatalf("delivered_at = %s, want %s", at, want)
	}
}

func TestTransientBouncesWithinWindow(t *testing.T) {
	for _, bounceType := range []string{"Transient", "Undetermined"} {
		t.Run(bounceType, func(t *testing.T) {
			h := newHarness(t)
			c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
			// An old transient bounce falls out of the 30-day window.
			h.insertMessage("broadcast", c.ID, c.Email, "ses-0", 0)
			h.notify("sns-0", bounceEvent("ses-0", bounceType, c.Email))
			h.clock.Advance(31 * 24 * time.Hour)
			for i := 1; i <= 3; i++ {
				ses := fmt.Sprintf("ses-%d", i)
				h.insertMessage("broadcast", c.ID, c.Email, ses, 0)
				h.notify(fmt.Sprintf("sns-%d", i), bounceEvent(ses, bounceType, c.Email))
				want := core.StatusActive
				if i == 3 {
					want = core.StatusBounced
				}
				if got := h.contactStatus(c.ID); got != want {
					t.Fatalf("after %d recent transient bounces: status = %s, want %s", i, got, want)
				}
				h.clock.Advance(24 * time.Hour)
			}
			if n := h.count(`SELECT COUNT(*) FROM contact_status_changes WHERE contact_id = ? AND to_status = 'bounced' AND note LIKE '3 transient%'`, c.ID); n != 1 {
				t.Fatalf("status change rows = %d", n)
			}
		})
	}
}

func TestMultipleRecipients(t *testing.T) {
	h := newHarness(t)
	alice := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	bob := testutil.InsertContact(t, h.db, "bob@example.net", core.StatusActive)
	msgID := h.insertMessage("broadcast", alice.ID, alice.Email, "ses-1", 0)

	h.notify("sns-1", bounceEvent("ses-1", "Permanent", "alice@example.net", "bob@example.net", "carol@example.net"))
	if got := h.contactStatus(alice.ID); got != core.StatusBounced {
		t.Errorf("alice = %s", got)
	}
	if got := h.contactStatus(bob.ID); got != core.StatusBounced {
		t.Errorf("bob = %s (only the first recipient handled?)", got)
	}
	if n := h.count(`SELECT COUNT(*) FROM events WHERE type = 'bounce' AND message_id = ?`, msgID); n != 3 {
		t.Fatalf("bounce events = %d, want one per recipient (3)", n)
	}

	h.notify("sns-2", complaintEvent("ses-1", "alice@example.net", "bob@example.net"))
	if alice, bob := h.contactStatus(alice.ID), h.contactStatus(bob.ID); alice != core.StatusComplained || bob != core.StatusComplained {
		t.Fatalf("after complaint: alice %s, bob %s", alice, bob)
	}
}

func TestDuplicateDeliveryIsIdempotent(t *testing.T) {
	h := newHarness(t)
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	h.insertMessage("broadcast", c.ID, c.Email, "ses-1", 0)
	e := snsMessage(t, events.TypeNotification, "sns-dup", sesJSON(t, bounceEvent("ses-1", "Transient", c.Email)), "2")
	for range 5 {
		if rec := h.postSNS(e); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if n := h.count(`SELECT COUNT(*) FROM events`); n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
	if got := h.contactStatus(c.ID); got != core.StatusActive {
		t.Fatalf("a redelivered transient bounce was counted again: status %s", got)
	}
}

func TestUnknownMessageFallsBackToContact(t *testing.T) {
	h := newHarness(t)
	h.clock.Advance(events.UnknownMessageGrace) // the events' emails are not fresh
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	h.notify("sns-1", bounceEvent("ses-never-seen", "Permanent", "ALICE@example.net"))
	if got := h.contactStatus(c.ID); got != core.StatusBounced {
		t.Fatalf("status = %s, want bounced", got)
	}
	if n := h.count(`SELECT COUNT(*) FROM events WHERE message_id IS NULL AND type = 'bounce'`); n != 1 {
		t.Fatalf("unlinked events = %d", n)
	}
	// A GDPR erase can't find this row later (it scrubs the events of the
	// contact's messages), so it must not hold the address.
	var detail string
	if err := h.db.Read.QueryRow(`SELECT detail FROM events WHERE message_id IS NULL`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(detail), "alice") ||
		detail != `{"summary":true,"eventType":"Bounce","bounceType":"Permanent","bounceSubType":"General","sesMessageId":"ses-never-seen"}` {
		t.Fatalf("detail = %s, want an address-free summary", detail)
	}
	if n := h.count(`SELECT COUNT(*) FROM contact_status_changes WHERE contact_id = ? AND message_id IS NULL AND source = 'bounce'`, c.ID); n != 1 {
		t.Fatalf("status change rows = %d", n)
	}
	// Transient bounces of unknown messages count towards the limit too.
	d := testutil.InsertContact(t, h.db, "dave@example.net", core.StatusActive)
	for i := range 3 {
		h.notify(fmt.Sprintf("sns-t%d", i), bounceEvent(fmt.Sprintf("ses-unknown-%d", i), "Transient", d.Email))
	}
	if got := h.contactStatus(d.ID); got != core.StatusBounced {
		t.Fatalf("dave = %s, want bounced", got)
	}
}

func TestTestMessageAppliesToMatchingContact(t *testing.T) {
	h := newHarness(t)
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	msgID := h.insertMessage("test", 0, "alice@example.net", "ses-1", 0)
	h.notify("sns-1", complaintEvent("ses-1", "alice@example.net"))
	if got := h.contactStatus(c.ID); got != core.StatusComplained {
		t.Fatalf("status = %s", got)
	}
	if got := h.messageStatus(msgID); got != "complained" {
		t.Fatalf("message = %s", got)
	}
	// The message has no contact, so an erase of alice wouldn't scrub the row.
	if n := h.count(`SELECT COUNT(*) FROM events WHERE message_id = ? AND detail LIKE '%alice%'`, msgID); n != 0 {
		t.Fatal("event of a message without contact kept the raw address")
	}
}

func TestDetailOnlyKeptWhereEraseFindsIt(t *testing.T) {
	h := newHarness(t)
	alice := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	testutil.InsertContact(t, h.db, "bob@example.net", core.StatusActive)
	msgID := h.insertMessage("broadcast", alice.ID, alice.Email, "ses-1", 0)
	h.notify("sns-1", bounceEvent("ses-1", "Transient", "alice@example.net", "bob@example.net"))
	var aliceDetail, bobDetail string
	q := `SELECT detail FROM events WHERE message_id = ? AND provider_event_id = ?`
	if err := h.db.Read.QueryRow(q, msgID, "sns-1:"+core.EmailHash("alice@example.net")).Scan(&aliceDetail); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Read.QueryRow(q, msgID, "sns-1:"+core.EmailHash("bob@example.net")).Scan(&bobDetail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aliceDetail, "bouncedRecipients") {
		t.Errorf("the message contact's row lost its raw event: %s", aliceDetail)
	}
	if strings.Contains(bobDetail, "@") {
		t.Errorf("another contact's row on alice's message kept addresses: %s", bobDetail)
	}
}

func TestUnknownFreshMessageIsDeferredOnce(t *testing.T) {
	h := newHarness(t)
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	// mail.timestamp is a minute before the clock: the sender may not have
	// stored the SES MessageId yet.
	e := snsMessage(t, events.TypeNotification, "sns-1", sesJSON(t, bounceEvent("ses-late", "Permanent", c.Email)), "2")
	if rec := h.postSNS(e); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first delivery: status = %d, want 503", rec.Code)
	}
	if n := h.count(`SELECT COUNT(*) FROM events`); n != 0 {
		t.Fatalf("a deferred event wrote %d rows", n)
	}
	if got := h.contactStatus(c.ID); got != core.StatusActive {
		t.Fatalf("a deferred event changed the contact: %s", got)
	}
	// Meanwhile the sender records the id; SNS redelivers.
	msgID := h.insertMessage("broadcast", c.ID, c.Email, "ses-late", 0)
	h.clock.Advance(20 * time.Second)
	if rec := h.postSNS(e); rec.Code != http.StatusOK {
		t.Fatalf("redelivery: status = %d", rec.Code)
	}
	if n := h.count(`SELECT COUNT(*) FROM events WHERE message_id = ?`, msgID); n != 1 {
		t.Fatalf("linked events = %d", n)
	}
	if got := h.messageStatus(msgID); got != "bounced" {
		t.Fatalf("message = %s", got)
	}

	// Still unknown on redelivery: processed with the fallback, never lost.
	d := testutil.InsertContact(t, h.db, "dave@example.net", core.StatusActive)
	e2 := snsMessage(t, events.TypeNotification, "sns-2", sesJSON(t, withMailTimestamp(
		bounceEvent("ses-foreign", "Permanent", d.Email), h.clock.Now().Add(-time.Second))), "2")
	if rec := h.postSNS(e2); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first delivery: status = %d, want 503", rec.Code)
	}
	if rec := h.postSNS(e2); rec.Code != http.StatusOK {
		t.Fatalf("redelivery: status = %d, want 200", rec.Code)
	}
	if got := h.contactStatus(d.ID); got != core.StatusBounced {
		t.Fatalf("dave = %s, want bounced", got)
	}

	// Events without mail.timestamp, or older than the grace, are never deferred.
	for i, ev := range []map[string]any{
		complaintEvent("ses-none", "x@example.net"),
		withMailTimestamp(bounceEvent("ses-old", "Permanent", "y@example.net"), h.clock.Now().Add(-events.UnknownMessageGrace)),
	} {
		rec := h.postSNS(snsMessage(t, events.TypeNotification, fmt.Sprintf("sns-n%d", i), sesJSON(t, ev), "2"))
		if rec.Code != http.StatusOK {
			t.Fatalf("event %d: status = %d, want 200", i, rec.Code)
		}
	}
}

func withMailTimestamp(ev map[string]any, at time.Time) map[string]any {
	ev["mail"].(map[string]any)["timestamp"] = at.UTC().Format(time.RFC3339Nano)
	return ev
}

func TestMessageStatusNeverDowngrades(t *testing.T) {
	tests := []struct {
		name  string
		steps []func(ses, addr string) map[string]any
		want  string
	}{
		{name: "complaint then delivery", want: "complained", steps: []func(string, string) map[string]any{
			func(s, a string) map[string]any { return complaintEvent(s, a) },
			func(s, a string) map[string]any { return deliveryEvent(s, a) },
		}},
		{name: "bounce then delivery", want: "bounced", steps: []func(string, string) map[string]any{
			func(s, a string) map[string]any { return bounceEvent(s, "Permanent", a) },
			func(s, a string) map[string]any { return deliveryEvent(s, a) },
		}},
		{name: "complaint then bounce", want: "complained", steps: []func(string, string) map[string]any{
			func(s, a string) map[string]any { return complaintEvent(s, a) },
			func(s, a string) map[string]any { return bounceEvent(s, "Transient", a) },
		}},
		{name: "delivery then bounce", want: "bounced", steps: []func(string, string) map[string]any{
			func(s, a string) map[string]any { return deliveryEvent(s, a) },
			func(s, a string) map[string]any { return bounceEvent(s, "Permanent", a) },
		}},
		{name: "delivery then soft bounce", want: "delivered", steps: []func(string, string) map[string]any{
			func(s, a string) map[string]any { return deliveryEvent(s, a) },
			func(s, a string) map[string]any { return bounceEvent(s, "Transient", a) },
		}},
		{name: "complaint then not-spam", want: "complained", steps: []func(string, string) map[string]any{
			func(s, a string) map[string]any { return complaintEvent(s, a) },
			func(s, a string) map[string]any { return notSpamEvent(s, a) },
		}},
		{name: "delivery then reject", want: "delivered", steps: []func(string, string) map[string]any{
			func(s, a string) map[string]any { return deliveryEvent(s, a) },
			func(s, a string) map[string]any { return rejectEvent(s, a) },
		}},
		{name: "bounce then complaint", want: "complained", steps: []func(string, string) map[string]any{
			func(s, a string) map[string]any { return bounceEvent(s, "Transient", a) },
			func(s, a string) map[string]any { return complaintEvent(s, a) },
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
			msgID := h.insertMessage("broadcast", c.ID, c.Email, "ses-1", 0)
			for i, step := range tt.steps {
				h.notify(fmt.Sprintf("sns-%d", i), step("ses-1", c.Email))
			}
			if got := h.messageStatus(msgID); got != tt.want {
				t.Fatalf("message status = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestComplainedContactStaysComplained(t *testing.T) {
	h := newHarness(t)
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusComplained)
	h.insertMessage("broadcast", c.ID, c.Email, "ses-1", 0)
	h.notify("sns-1", bounceEvent("ses-1", "Permanent", c.Email))
	if got := h.contactStatus(c.ID); got != core.StatusComplained {
		t.Fatalf("status = %s", got)
	}
	// An unsubscribed contact still records the SES fact.
	u := testutil.InsertContact(t, h.db, "ursula@example.net", core.StatusUnsubscribed)
	h.insertMessage("broadcast", u.ID, u.Email, "ses-2", 0)
	h.notify("sns-2", bounceEvent("ses-2", "Permanent", u.Email))
	if got := h.contactStatus(u.ID); got != core.StatusBounced {
		t.Fatalf("unsubscribed contact after permanent bounce = %s, want bounced", got)
	}
}

func TestErasedContactKeepsNoAddress(t *testing.T) {
	h := newHarness(t)
	h.clock.Advance(events.UnknownMessageGrace)
	// An erased contact's message: contact_id and to_email are NULL.
	msgID := h.insertMessage("broadcast", 0, "", "ses-1", 0)
	h.notify("sns-1", bounceEvent("ses-1", "Permanent", "gone@example.net"))
	var detail, pid string
	if err := h.db.Read.QueryRow(`SELECT detail, provider_event_id FROM events WHERE message_id = ?`, msgID).Scan(&detail, &pid); err != nil {
		t.Fatal(err)
	}
	if detail != "{}" {
		t.Fatalf("detail = %q, want {}", detail)
	}
	if pid != "sns-1:"+core.EmailHash("gone@example.net") {
		t.Fatalf("provider_event_id = %q", pid)
	}
	if n := h.count(`SELECT COUNT(*) FROM contact_status_changes`); n != 0 {
		t.Fatalf("status changes = %d", n)
	}
	if got := h.messageStatus(msgID); got != "bounced" {
		t.Fatalf("message = %s", got)
	}

	// An unknown message to an erased (suppressed) address.
	testutil.InsertSuppression(t, h.db, "erased@example.net", "erased")
	h.notify("sns-2", complaintEvent("ses-unknown", "erased@example.net"))
	if n := h.count(`SELECT COUNT(*) FROM events WHERE provider_event_id = ? AND detail = '{}'`, "sns-2:"+core.EmailHash("erased@example.net")); n != 1 {
		t.Fatal("event of an erased address kept its raw detail")
	}
}

// TestDetailDropsMessageHeaders: SES copies the original headers into every
// event, including List-Unsubscribe with the recipient's working one-click
// URL. The stored detail keeps the event but never the headers.
func TestDetailDropsMessageHeaders(t *testing.T) {
	h := newHarness(t)
	alice := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	msgID := h.insertMessage("broadcast", alice.ID, alice.Email, "ses-1", 0)
	ev := deliveryEvent("ses-1", "alice@example.net")
	mail := ev["mail"].(map[string]any)
	mail["headersTruncated"] = false
	mail["headers"] = []map[string]string{
		{"name": "List-Unsubscribe", "value": "<https://mail.example.com/u/SECRET-TOKEN>"},
		{"name": "List-Unsubscribe-Post", "value": "List-Unsubscribe=One-Click"},
	}
	mail["commonHeaders"] = map[string]any{"subject": "Hi", "to": []string{"alice@example.net"}}
	h.notify("sns-1", ev)
	var detail string
	if err := h.db.Read.QueryRow(`SELECT detail FROM events WHERE message_id = ?`, msgID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, "SECRET-TOKEN") || strings.Contains(detail, "headers") || strings.Contains(detail, "commonHeaders") {
		t.Fatalf("detail kept the message headers: %s", detail)
	}
	if !strings.Contains(detail, `"messageId":"ses-1"`) || !strings.Contains(detail, `"delivery"`) {
		t.Fatalf("detail lost the event: %s", detail)
	}
}
