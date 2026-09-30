package forms_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/forms"
	"mailroom/internal/testutil"
)

func TestExpire(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	ctx := context.Background()

	stale, _ := pendingSignup(h, f, "stale@example.com")
	_, confirmPath := pendingSignup(h, f, "confirmed@example.com")
	h.publicReq("POST", confirmPath, "", "").wantStatus(t, http.StatusOK)
	unsub := testutil.InsertContact(t, h.d, "unsub@example.com", core.StatusUnsubscribed)
	h.submit(f.PublicID, vals("email", "unsub@example.com")).wantStatus(t, http.StatusSeeOther)
	apiPending := testutil.InsertContact(t, h.d, "api-pending@example.com", core.StatusPending)

	// Nothing is due yet.
	res, err := h.svc.Expire(ctx)
	if err != nil || res != (forms.ExpiryResult{}) {
		t.Fatalf("early Expire = %+v, %v", res, err)
	}

	// A late sign-up is refreshed within the window and survives.
	h.clock.Advance(3 * 24 * time.Hour)
	fresh, _ := pendingSignup(h, f, "fresh@example.com")
	h.clock.Advance(forms.OptinTTL - 3*24*time.Hour + time.Minute)
	// SES reported the confirmation email delivered; the raw event names
	// the recipient.
	var staleMsg int64
	if err := h.d.Read.QueryRow(`SELECT id FROM messages WHERE kind = 'optin' AND contact_id = ?`, stale.ID).Scan(&staleMsg); err != nil {
		t.Fatal(err)
	}
	testutil.Exec(t, h.d, `INSERT INTO events (message_id, type, detail, created_at) VALUES (?, 'delivery', ?, ?)`,
		staleMsg, `{"mail":{"destination":["stale@example.com"]}}`, db.FormatTime(h.clock.Now()))

	res, err = h.svc.Expire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// stale (optin) and unsub (resubscribe) expired; only the never-confirmed contact goes.
	if res.PendingOptins != 2 || res.Contacts != 1 {
		t.Fatalf("Expire = %+v", res)
	}
	if n := h.count(`SELECT count(*) FROM contacts WHERE id = ?`, stale.ID); n != 0 {
		t.Fatal("a never-confirmed sign-up must be deleted")
	}
	for _, email := range []string{"confirmed@example.com", "fresh@example.com", "unsub@example.com", "api-pending@example.com"} {
		if n := h.count(`SELECT count(*) FROM contacts WHERE email = ?`, email); n != 1 {
			t.Errorf("%s was deleted", email)
		}
	}
	if got := h.contact("unsub@example.com"); got.Status != core.StatusUnsubscribed || got.ID != unsub.ID {
		t.Fatalf("unsubscribed contact = %+v", got)
	}
	if got := h.contact("api-pending@example.com"); got.ID != apiPending.ID {
		t.Fatal("pending contacts without a form opt-in are never touched")
	}
	if n := h.count(`SELECT count(*) FROM pending_optins WHERE contact_id = ?`, fresh.ID); n != 1 {
		t.Fatal("an open opt-in must survive")
	}
	// The confirmation message stays for the counts, without the address.
	if n := h.count(`SELECT count(*) FROM messages WHERE kind = 'optin' AND contact_id IS NULL AND to_email IS NULL`); n != 1 {
		t.Fatalf("anonymised messages = %d", n)
	}
	if n := h.count(`SELECT count(*) FROM events WHERE message_id = ? AND detail = '{}'`, staleMsg); n != 1 {
		t.Fatal("the confirmation email's SES event keeps the raw JSON")
	}
	if n := h.count(`SELECT count(*) FROM events WHERE instr(detail, 'stale@example.com') > 0`); n != 0 {
		t.Fatal("an event still names the expired sign-up")
	}
	if n := h.count(`SELECT count(*) FROM audit_log WHERE operation = 'form.optin_expire' AND actor_type = 'system'`); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	// Idempotent.
	if res, err := h.svc.Expire(ctx); err != nil || res != (forms.ExpiryResult{}) {
		t.Fatalf("second Expire = %+v, %v", res, err)
	}
	// The same address can sign up again.
	h.submit(f.PublicID, vals("email", "stale@example.com")).wantStatus(t, http.StatusSeeOther)
	if got := h.contact("stale@example.com"); got.Status != core.StatusPending {
		t.Fatalf("re-signup = %+v", got)
	}
}

// TestExpireBatches: more expired sign-ups than one batch are all
// removed, one transaction (and audit entry) per batch.
func TestExpireBatches(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	const n = 1001 // two full batches of 500 and one of 1
	now := db.FormatTime(h.clock.Now())
	testutil.Exec(t, h.d, `
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < ?)
		INSERT INTO contacts (email, status, status_changed_at, created_at, updated_at)
		SELECT 'b' || i || '@example.com', 'pending', ?2, ?2, ?2 FROM seq`, n, now)
	testutil.Exec(t, h.d, `
		INSERT INTO pending_optins (form_id, contact_id, kind, token_hash, created_at, expires_at)
		SELECT ?, id, 'optin', 'h' || id, ?, ? FROM contacts WHERE email LIKE 'b%@example.com'`,
		f.ID, now, db.FormatTime(h.clock.Now().Add(-time.Minute)))
	testutil.Exec(t, h.d, `
		INSERT INTO consent_events (contact_id, type, source, form_id, occurred_at, created_at)
		SELECT id, 'optin_requested', ?, ?, ?, ? FROM contacts WHERE email LIKE 'b%@example.com'`,
		"form:"+f.PublicID, f.ID, now, now)
	open, _ := pendingSignup(h, f, "open@example.com")

	res, err := h.svc.Expire(context.Background())
	if err != nil || res.PendingOptins != n || res.Contacts != n {
		t.Fatalf("Expire = %+v, %v", res, err)
	}
	if left := h.count(`SELECT count(*) FROM contacts WHERE email LIKE 'b%@example.com'`); left != 0 {
		t.Fatalf("%d expired sign-ups left", left)
	}
	if left := h.count(`SELECT count(*) FROM pending_optins WHERE contact_id != ?`, open.ID); left != 0 {
		t.Fatalf("%d expired opt-ins left", left)
	}
	if h.count(`SELECT count(*) FROM pending_optins WHERE contact_id = ?`, open.ID) != 1 {
		t.Fatal("an open opt-in must survive")
	}
	if runs := h.count(`SELECT count(*) FROM audit_log WHERE operation = 'form.optin_expire'`); runs != 3 {
		t.Fatalf("batches = %d, want 3", runs)
	}
}

func TestExpiryWorker(t *testing.T) {
	h := newHarness(t)
	w := h.svc.ExpiryWorker()
	if w.Name() != "optin-expiry" {
		t.Fatalf("name = %s", w.Name())
	}
	f, _ := signupForm(h, `,"double_optin":true`)
	pendingSignup(h, f, "w@example.com")
	h.clock.Advance(forms.OptinTTL + time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for h.count(`SELECT count(*) FROM contacts`) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker did not expire the sign-up at start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestExpireKeepsContactsCreatedElsewhere: a pending contact created by
// the API, an admin or an import that submits a form and never confirms
// loses only its expired opt-in; the contact, its tags and its consent
// log stay (C-3). A contact the form created is deleted.
func TestExpireKeepsContactsCreatedElsewhere(t *testing.T) {
	for _, consent := range []struct{ typ, source string }{
		{"api", "api:zapier"}, {"admin", "admin:owner@example.com"}, {"import", "import:ghl"},
	} {
		t.Run(consent.typ, func(t *testing.T) {
			h := newHarness(t)
			f, _ := signupForm(h, `,"double_optin":true`)
			now := db.FormatTime(h.clock.Now())
			c := testutil.InsertContact(t, h.d, "api@example.com", core.StatusPending, "crm")
			testutil.Exec(t, h.d, `INSERT INTO consent_events (contact_id, type, source, occurred_at, created_at)
				VALUES (?, ?, ?, ?, ?)`, c.ID, consent.typ, consent.source, now, now)
			h.submit(f.PublicID, vals("email", "api@example.com")).wantStatus(t, http.StatusSeeOther)
			formMade, _ := pendingSignup(h, f, "form@example.com")
			if n := h.count(`SELECT count(*) FROM pending_optins WHERE contact_id = ?`, c.ID); n != 1 {
				t.Fatalf("pending opt-ins = %d", n)
			}
			h.clock.Advance(forms.OptinTTL + time.Minute)

			res, err := h.svc.Expire(context.Background())
			if err != nil || res != (forms.ExpiryResult{PendingOptins: 2, Contacts: 1}) {
				t.Fatalf("Expire = %+v, %v", res, err)
			}
			got := h.contact("api@example.com")
			if got.ID != c.ID || got.Status != core.StatusPending || strings.Join(got.Tags, ",") != "crm" {
				t.Fatalf("contact = %+v", got)
			}
			if cs := h.consents(c.ID); len(cs) != 2 || cs[0].Type != consent.typ || cs[1].Type != "optin_requested" {
				t.Fatalf("consent log = %+v", cs)
			}
			if n := h.count(`SELECT count(*) FROM contacts WHERE id = ?`, formMade.ID); n != 0 {
				t.Fatal("the never-confirmed form sign-up must be deleted")
			}
		})
	}
}
