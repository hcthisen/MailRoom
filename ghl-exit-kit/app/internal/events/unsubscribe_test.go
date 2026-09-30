package events_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/events"
	"mailroom/internal/testutil"
	"mailroom/internal/tokens"
)

const formType = "application/x-www-form-urlencoded"

// sequenceFixture is an active contact enrolled in a sequence, with a
// sequence message and a broadcast message.
type sequenceFixture struct {
	contact      core.Contact
	enrollment   int64
	other        int64 // a second active enrollment of the same contact
	seqMessage   int64
	bcastMessage int64
}

func newSequenceFixture(t *testing.T, h *harness) sequenceFixture {
	t.Helper()
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	email := testutil.InsertEmail(t, h.db, "Welcome", "Hello")
	seq := testutil.InsertSequence(t, h.db, "Onboarding")
	step := testutil.InsertStep(t, h.db, seq, 1, 0, email)
	seq2 := testutil.InsertSequence(t, h.db, "Webinar")
	step2 := testutil.InsertStep(t, h.db, seq2, 1, 0, email)
	f := sequenceFixture{contact: c}
	f.enrollment = testutil.InsertEnrollment(t, h.db, seq, c.ID, step)
	f.other = testutil.InsertEnrollment(t, h.db, seq2, c.ID, step2)
	f.seqMessage = h.insertMessage("sequence", c.ID, c.Email, "", f.enrollment)
	f.bcastMessage = h.insertMessage("broadcast", c.ID, c.Email, "", 0)
	return f
}

func (h *harness) unsubURL(contactID, messageID int64) string {
	return "/u/" + h.token(tokens.PurposeUnsubscribe, tokens.Payload{ContactID: contactID, MessageID: messageID})
}

func TestUnsubscribePage(t *testing.T) {
	h := newHarness(t)
	f := newSequenceFixture(t, h)

	rec := h.do(http.MethodGet, h.unsubURL(f.contact.ID, f.bcastMessage), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Acme News", `method="post"`, ">Unsubscribe</button>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "Only stop these emails") {
		t.Error("broadcast page offers the sequence-only option")
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") || strings.Contains(body, "<script") || strings.Contains(body, "<img") {
		t.Error("page loads external assets or scripts")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if h.contactStatus(f.contact.ID) != core.StatusActive {
		t.Fatal("GET changed the status")
	}

	rec = h.do(http.MethodGet, h.unsubURL(f.contact.ID, f.seqMessage), "", "")
	if !strings.Contains(rec.Body.String(), "Only stop these emails") {
		t.Fatal("sequence page lacks the sequence-only option")
	}
}

func TestUnsubscribePageInvalidTokens(t *testing.T) {
	h := newHarness(t)
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	good := h.token(tokens.PurposeUnsubscribe, tokens.Payload{ContactID: c.ID})
	click := h.token(tokens.PurposeClick, tokens.Payload{ContactID: c.ID, LinkID: 1})
	for name, tok := range map[string]string{
		"garbage":       "not-a-token",
		"tampered":      good[:len(good)-2] + "AA",
		"wrong purpose": click,
		"no contact":    h.token(tokens.PurposeUnsubscribe, tokens.Payload{MessageID: 5}),
	} {
		t.Run(name, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				rec := h.do(method, "/u/"+tok, formType, "List-Unsubscribe=One-Click")
				if rec.Code != http.StatusNotFound {
					t.Fatalf("%s status = %d, want 404", method, rec.Code)
				}
				if !strings.Contains(rec.Body.String(), "isn’t valid") {
					t.Fatalf("%s: not the generic 404 page", method)
				}
			}
			if h.contactStatus(c.ID) != core.StatusActive {
				t.Fatal("status changed")
			}
		})
	}
}

func TestUnsubscribe(t *testing.T) {
	multipartBody := func() (string, string) {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		_ = w.WriteField("List-Unsubscribe", "One-Click")
		_ = w.Close()
		return w.FormDataContentType(), b.String()
	}
	mpType, mpBody := multipartBody()
	tests := []struct {
		name        string
		contentType string
		body        string
		wantSource  string
	}{
		{name: "one-click urlencoded", contentType: formType, body: "List-Unsubscribe=One-Click", wantSource: "one_click"},
		{name: "one-click multipart", contentType: mpType, body: mpBody, wantSource: "one_click"},
		{name: "one-click without content type", body: "List-Unsubscribe=One-Click\r\n", wantSource: "one_click"},
		{name: "page button", contentType: formType, body: "scope=all", wantSource: "link"},
		{name: "empty body", contentType: formType, body: "", wantSource: "link"},
		{name: "one-click beats sequence scope", contentType: formType, body: "scope=sequence&List-Unsubscribe=One-Click", wantSource: "one_click"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			f := newSequenceFixture(t, h)
			msg := f.seqMessage
			rec := h.do(http.MethodPost, h.unsubURL(f.contact.ID, msg), tt.contentType, tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), "You’re unsubscribed") {
				t.Fatalf("unexpected page: %s", rec.Body)
			}
			if got := h.contactStatus(f.contact.ID); got != core.StatusUnsubscribed {
				t.Fatalf("status = %s", got)
			}
			if n := h.count(`SELECT COUNT(*) FROM contact_status_changes WHERE contact_id = ? AND to_status = 'unsubscribed' AND source = ? AND message_id = ? AND actor = 'public'`,
				f.contact.ID, tt.wantSource, msg); n != 1 {
				t.Fatalf("status change rows = %d", n)
			}
			if n := h.count(`SELECT COUNT(*) FROM events WHERE type = 'unsubscribe' AND subtype = ? AND message_id = ?`, tt.wantSource, msg); n != 1 {
				t.Fatalf("unsubscribe events = %d", n)
			}
			if ch := h.changes(); len(ch) != 1 || ch[0].To != core.StatusUnsubscribed || ch[0].Source != tt.wantSource || ch[0].MessageID != msg {
				t.Fatalf("StatusChanged hooks = %+v", ch)
			}
		})
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	h := newHarness(t)
	f := newSequenceFixture(t, h)
	u := h.unsubURL(f.contact.ID, f.bcastMessage)
	for i := range 3 {
		rec := h.do(http.MethodPost, u, formType, "List-Unsubscribe=One-Click")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "You’re unsubscribed") {
			t.Fatalf("attempt %d: status %d", i, rec.Code)
		}
	}
	if n := h.count(`SELECT COUNT(*) FROM events WHERE type = 'unsubscribe'`); n != 1 {
		t.Fatalf("unsubscribe events = %d, want 1", n)
	}
	if n := h.count(`SELECT COUNT(*) FROM contact_status_changes`); n != 1 {
		t.Fatalf("status changes = %d, want 1", n)
	}
	rec := h.do(http.MethodGet, u, "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "You’re unsubscribed") || strings.Contains(rec.Body.String(), "<form") {
		t.Fatalf("GET after unsubscribe should show the result page: %s", rec.Body)
	}
}

func TestUnsubscribeSequenceOnly(t *testing.T) {
	h := newHarness(t)
	f := newSequenceFixture(t, h)
	u := h.unsubURL(f.contact.ID, f.seqMessage)
	for range 2 {
		rec := h.do(http.MethodPost, u, formType, "scope=sequence")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "this series") {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
		}
	}
	if got := h.contactStatus(f.contact.ID); got != core.StatusActive {
		t.Fatalf("contact status = %s, want active", got)
	}
	var status, reason string
	var finished *string
	if err := h.db.Read.QueryRow(`SELECT status, exit_reason, finished_at FROM enrollments WHERE id = ?`, f.enrollment).Scan(&status, &reason, &finished); err != nil {
		t.Fatal(err)
	}
	if status != "exited" || reason != events.ExitUnsubscribedSequence || finished == nil {
		t.Fatalf("enrollment = %s/%s finished %v", status, reason, finished)
	}
	if n := h.count(`SELECT COUNT(*) FROM enrollments WHERE id = ? AND status = 'active'`, f.other); n != 1 {
		t.Fatal("the other enrollment was touched")
	}
	if n := h.count(`SELECT COUNT(*) FROM events WHERE type = 'unsubscribe' AND subtype = 'link' AND message_id = ? AND json_extract(detail, '$.scope') = 'sequence'`, f.seqMessage); n != 1 {
		t.Fatalf("unsubscribe events = %d, want 1", n)
	}
	// The page no longer offers the sequence option.
	if strings.Contains(h.do(http.MethodGet, u, "", "").Body.String(), "Only stop these emails") {
		t.Fatal("sequence-only option shown for an exited enrollment")
	}
	// Sequence scope on a broadcast message unsubscribes fully.
	h.do(http.MethodPost, h.unsubURL(f.contact.ID, f.bcastMessage), formType, "scope=sequence")
	if got := h.contactStatus(f.contact.ID); got != core.StatusUnsubscribed {
		t.Fatalf("status = %s, want unsubscribed", got)
	}
}

func TestUnsubscribeEdgeCases(t *testing.T) {
	t.Run("bounced contact", func(t *testing.T) {
		// The status stays (bounced already stops all marketing email), but
		// the request is recorded once, so a later reactivation lands on
		// unsubscribed (contactstore.RecordOptOut).
		for _, st := range []core.ContactStatus{core.StatusBounced, core.StatusComplained, core.StatusSuppressedManual} {
			h := newHarness(t)
			c := testutil.InsertContact(t, h.db, "b@example.net", st)
			for range 2 {
				rec := h.do(http.MethodPost, h.unsubURL(c.ID, 0), formType, "List-Unsubscribe=One-Click")
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: status = %d", st, rec.Code)
				}
			}
			if got := h.contactStatus(c.ID); got != st {
				t.Fatalf("%s: status = %s", st, got)
			}
			if n := h.count(`SELECT COUNT(*) FROM events WHERE type = 'unsubscribe' AND subtype = 'one_click'
				AND json_extract(detail, '$.status_kept') = ?`, string(st)); n != 1 {
				t.Fatalf("%s: unsubscribe events = %d, want 1", st, n)
			}
			if n := h.count(`SELECT COUNT(*) FROM contacts WHERE id = ? AND unsubscribe_requested_at IS NOT NULL`, c.ID); n != 1 {
				t.Fatalf("%s: opt-out not recorded on the contact", st)
			}
		}
	})
	t.Run("erased contact", func(t *testing.T) {
		h := newHarness(t)
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			rec := h.do(m, h.unsubURL(4242, 0), formType, "List-Unsubscribe=One-Click")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "You’re unsubscribed") {
				t.Fatalf("%s: status %d", m, rec.Code)
			}
		}
	})
	t.Run("pending contact", func(t *testing.T) {
		h := newHarness(t)
		c := testutil.InsertContact(t, h.db, "p@example.net", core.StatusPending)
		h.do(http.MethodPost, h.unsubURL(c.ID, 0), formType, "scope=all")
		if got := h.contactStatus(c.ID); got != core.StatusUnsubscribed {
			t.Fatalf("status = %s", got)
		}
		if n := h.count(`SELECT COUNT(*) FROM events WHERE type = 'unsubscribe' AND message_id IS NULL`); n != 1 {
			t.Fatalf("events = %d", n)
		}
	})
	t.Run("message of another contact", func(t *testing.T) {
		h := newHarness(t)
		a := testutil.InsertContact(t, h.db, "a@example.net", core.StatusActive)
		b := testutil.InsertContact(t, h.db, "b@example.net", core.StatusActive)
		msg := h.insertMessage("broadcast", b.ID, b.Email, "", 0)
		rec := h.do(http.MethodPost, h.unsubURL(a.ID, msg), formType, "List-Unsubscribe=One-Click")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if h.contactStatus(a.ID) != core.StatusActive || h.contactStatus(b.ID) != core.StatusActive {
			t.Fatal("a token whose message belongs to someone else changed a contact")
		}
		if n := h.count(`SELECT COUNT(*) FROM events`); n != 0 {
			t.Fatalf("events = %d", n)
		}
	})
	t.Run("message gone", func(t *testing.T) {
		h := newHarness(t)
		c := testutil.InsertContact(t, h.db, "c@example.net", core.StatusActive)
		// The broadcast (and so the message) was deleted: the token still works.
		h.do(http.MethodPost, h.unsubURL(c.ID, 999), formType, "List-Unsubscribe=One-Click")
		if got := h.contactStatus(c.ID); got != core.StatusUnsubscribed {
			t.Fatalf("status = %s", got)
		}
		if n := h.count(`SELECT COUNT(*) FROM contact_status_changes WHERE contact_id = ? AND message_id IS NULL`, c.ID); n != 1 {
			t.Fatal("status change not recorded without message")
		}
	})
	t.Run("method not allowed", func(t *testing.T) {
		h := newHarness(t)
		if rec := h.do(http.MethodDelete, h.unsubURL(1, 0), "", ""); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}
