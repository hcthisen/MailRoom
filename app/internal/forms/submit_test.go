package forms_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/forms"
	"mailroom/internal/testutil"
)

// signupForm creates a single opt-in form with a first name field, a tag
// and a sequence, and returns it with the sequence ID.
func signupForm(h *harness, extra string) (forms.Form, int64) {
	h.t.Helper()
	seq := testutil.InsertSequence(h.t, h.d, fmt.Sprintf("Welcome %d", h.count(`SELECT count(*) FROM sequences`)))
	f := h.createForm(fmt.Sprintf(`{"name":"Community","fields":[{"key":"first_name"},{"key":"attr.company"}],"tags":["community"],"sequence_id":%d%s}`, seq, extra))
	return f, seq
}

func TestSubmitNewSingleOptin(t *testing.T) {
	h := newHarness(t)
	f, seq := signupForm(h, "")
	r := h.submit(f.PublicID, vals("email", " Alice@Example.com ", "first_name", "Alice", "attr.company", "Acme", "attr.ignored", "x"),
		"RemoteAddr", "203.0.113.7:4444").wantStatus(t, http.StatusSeeOther)
	if loc := r.header.Get("Location"); loc != f.HostedURL+"?submitted=1" {
		t.Fatalf("Location = %q", loc)
	}
	c := h.contact("alice@example.com")
	if c.Status != core.StatusActive || c.FirstName != "Alice" || c.Attributes["company"] != "Acme" || strings.Join(c.Tags, ",") != "community" {
		t.Fatalf("contact = %+v", c)
	}
	if _, ok := c.Attributes["ignored"]; ok {
		t.Fatal("fields the form doesn't have must be ignored")
	}
	if got := h.lastOutcome(f.ID); got != forms.OutcomeCreated {
		t.Fatalf("outcome = %s", got)
	}
	cs := h.consents(c.ID)
	want := consentEvent{Type: "signup", Source: "form:" + f.PublicID, IP: "203.0.113.7", UserAgent: "TestBrowser/1.0", ConsentText: f.ConsentText}
	if len(cs) != 1 || cs[0].Type != want.Type || cs[0].Source != want.Source || cs[0].IP != want.IP ||
		cs[0].UserAgent != want.UserAgent || cs[0].ConsentText != want.ConsentText || cs[0].FormID.Int64 != f.ID {
		t.Fatalf("consent = %+v", cs)
	}
	if calls := h.enroller.Calls(); len(calls) != 1 || calls[0] != (enrollCall{seq, c.ID, "form:" + f.PublicID}) {
		t.Fatalf("enroll calls = %+v", calls)
	}
	if ev := h.tagEvents(); len(ev) != 1 || ev[0].Source != "form:"+f.PublicID || ev[0].TagName != "community" {
		t.Fatalf("tag events = %+v", ev)
	}
	if n := h.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("single opt-in must send no confirmation, messages = %d", n)
	}
}

func TestDoubleOptinIsOffByDefault(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"x"}`)
	if f.DoubleOptin {
		t.Fatal("double opt-in must default to off")
	}
	h.submit(f.PublicID, vals("email", "a@example.com")).wantStatus(t, http.StatusSeeOther)
	if c := h.contact("a@example.com"); c.Status != core.StatusActive {
		t.Fatalf("status = %s", c.Status)
	}
}

func TestSubmitNewDoubleOptin(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	h.submit(f.PublicID, vals("email", "bob@example.com", "first_name", "Bob", "attr.company", "Bobco"),
		"RemoteAddr", "203.0.113.8:1").wantStatus(t, http.StatusSeeOther)
	c := h.contact("bob@example.com")
	if c.Status != core.StatusPending || c.FirstName != "Bob" || len(c.Tags) != 0 {
		t.Fatalf("contact = %+v", c)
	}
	if got := h.lastOutcome(f.ID); got != forms.OutcomeOptinPending {
		t.Fatalf("outcome = %s", got)
	}
	if len(h.enroller.Calls()) != 0 || len(h.tagEvents()) != 0 {
		t.Fatal("tags and sequence must wait for the confirmation")
	}
	cs := h.consents(c.ID)
	if len(cs) != 1 || cs[0].Type != "optin_requested" || cs[0].IP != "203.0.113.8" || cs[0].Source != "form:"+f.PublicID || cs[0].ConsentText != f.ConsentText {
		t.Fatalf("consent = %+v", cs)
	}
	ms := h.optinMessages(c.ID)
	if len(ms) != 1 || ms[0].Template != core.TemplateOptinConfirm || ms[0].ToEmail != "bob@example.com" || ms[0].FormName != "Community" ||
		!strings.HasPrefix(ms[0].ConfirmURL, baseURL+"/f/confirm/") {
		t.Fatalf("messages = %+v", ms)
	}
	var kind, payload, expires string
	if err := h.d.Read.QueryRow(`SELECT kind, payload, expires_at FROM pending_optins WHERE contact_id = ?`, c.ID).Scan(&kind, &payload, &expires); err != nil {
		t.Fatal(err)
	}
	var p struct {
		PublicID   string            `json:"public_id"`
		Fields     map[string]string `json:"fields"`
		Tags       []string          `json:"tags"`
		SequenceID int64             `json:"sequence_id"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatal(err)
	}
	if kind != forms.KindOptin || p.PublicID != f.PublicID || p.Fields["first_name"] != "Bob" || p.Fields["attr.company"] != "Bobco" ||
		strings.Join(p.Tags, ",") != "community" || p.SequenceID != *f.SequenceID {
		t.Fatalf("pending = %s %s", kind, payload)
	}
	if !strings.HasPrefix(expires, h.clock.Now().Add(forms.OptinTTL).Format("2006-01-02T15:04:05")) {
		t.Fatalf("expires_at = %s", expires)
	}
}

// TestSubmitExistingActive: on a single opt-in form, a submission for a
// subscriber adds the form's tags, consent and sequence, but only fills in
// names and attributes the contact doesn't have: the public form needs no
// login, so it must never overwrite what is stored (merged into real mail).
func TestSubmitExistingActive(t *testing.T) {
	h := newHarness(t)
	f, seq := signupForm(h, "")
	c := testutil.InsertContact(t, h.d, "carol@example.com", core.StatusActive, "old")
	testutil.Exec(t, h.d, `UPDATE contacts SET first_name = 'Carol' WHERE id = ?`, c.ID)
	h.submit(f.PublicID, vals("email", "carol@example.com", "first_name", "URGENT verify at evil.example",
		"attr.company", "Carolco")).wantStatus(t, http.StatusSeeOther)
	got := h.contact("carol@example.com")
	if got.Status != core.StatusActive || got.FirstName != "Carol" || got.Attributes["company"] != "Carolco" ||
		strings.Join(got.Tags, ",") != "community,old" {
		t.Fatalf("contact = %+v", got)
	}
	// A stored attribute isn't overwritten either.
	h.submit(f.PublicID, vals("email", "carol@example.com", "attr.company", "Evilco")).wantStatus(t, http.StatusSeeOther)
	if got := h.contact("carol@example.com"); got.Attributes["company"] != "Carolco" {
		t.Fatalf("attributes = %v", got.Attributes)
	}
	if o := h.lastOutcome(f.ID); o != forms.OutcomeUpdated {
		t.Fatalf("outcome = %s", o)
	}
	if cs := h.consents(c.ID); len(cs) != 2 || cs[0].Type != "signup" {
		t.Fatalf("consent = %+v", cs)
	}
	if calls := h.enroller.Calls(); len(calls) == 0 || calls[0] != (enrollCall{seq, c.ID, "form:" + f.PublicID}) {
		t.Fatalf("enroll = %+v", calls)
	}
	if len(h.optinMessages(c.ID)) != 0 {
		t.Fatal("a single opt-in form sends no confirmation")
	}
	// Empty values never blank existing names.
	h.submit(f.PublicID, vals("email", "carol@example.com", "first_name", "  ")).wantStatus(t, http.StatusSeeOther)
	if got := h.contact("carol@example.com"); got.FirstName != "Carol" {
		t.Fatalf("first name = %q", got.FirstName)
	}
}

// TestSubmitExistingActiveDoubleOptin: on a double opt-in form, a
// submission for a subscriber changes nothing until the confirmation is
// clicked (F-6): no tags, no sequence, no signup consent, no new names.
// The click applies the tags and the sequence, and fills in only missing
// names and attributes.
func TestSubmitExistingActiveDoubleOptin(t *testing.T) {
	h := newHarness(t)
	f, seq := signupForm(h, `,"double_optin":true`)
	c := testutil.InsertContact(t, h.d, "carol@example.com", core.StatusActive, "old")
	testutil.Exec(t, h.d, `UPDATE contacts SET first_name = 'Carol' WHERE id = ?`, c.ID)
	h.submit(f.PublicID, vals("email", "carol@example.com", "first_name", "Mallory-was-here", "attr.company", "Carolco"),
		"RemoteAddr", "203.0.113.66:1").wantStatus(t, http.StatusSeeOther)
	got := h.contact("carol@example.com")
	if got.Status != core.StatusActive || got.FirstName != "Carol" || len(got.Attributes) != 0 || strings.Join(got.Tags, ",") != "old" || got.Version != c.Version {
		t.Fatalf("an active contact must not change before the confirmation: %+v", got)
	}
	if o := h.lastOutcome(f.ID); o != forms.OutcomeOptinPending {
		t.Fatalf("outcome = %s", o)
	}
	if len(h.enroller.Calls()) != 0 {
		t.Fatal("enrolled before the confirmation")
	}
	cs := h.consents(c.ID)
	if len(cs) != 1 || cs[0].Type != "optin_requested" {
		t.Fatalf("consents before the click = %+v, want only optin_requested", cs)
	}
	ms := h.optinMessages(c.ID)
	if len(ms) != 1 || ms[0].Template != core.TemplateOptinConfirm {
		t.Fatalf("messages = %+v", ms)
	}

	h.publicReq("POST", h.confirmPath(c.ID), "", "").wantStatus(t, http.StatusOK)
	got = h.contact("carol@example.com")
	if got.Status != core.StatusActive || got.FirstName != "Carol" || got.Attributes["company"] != "Carolco" ||
		strings.Join(got.Tags, ",") != "community,old" {
		t.Fatalf("after the click = %+v", got)
	}
	if calls := h.enroller.Calls(); len(calls) != 1 || calls[0] != (enrollCall{seq, c.ID, "form:" + f.PublicID}) {
		t.Fatalf("enroll = %+v", calls)
	}
	if cs := h.consents(c.ID); len(cs) != 2 || cs[1].Type != "optin_confirmed" {
		t.Fatalf("consents = %+v", cs)
	}
}

func TestSubmitUnsubscribedNeedsConfirmation(t *testing.T) {
	for _, double := range []bool{false, true} {
		t.Run(fmt.Sprintf("double_optin=%v", double), func(t *testing.T) {
			h := newHarness(t)
			f, _ := signupForm(h, fmt.Sprintf(`,"double_optin":%v`, double))
			c := testutil.InsertContact(t, h.d, "dave@example.com", core.StatusUnsubscribed)
			h.submit(f.PublicID, vals("email", "dave@example.com", "first_name", "Dave")).wantStatus(t, http.StatusSeeOther)
			got := h.contact("dave@example.com")
			if got.Status != core.StatusUnsubscribed || got.FirstName != "" || len(got.Tags) != 0 || got.Version != c.Version {
				t.Fatalf("an unsubscribed contact must not change before the confirmation: %+v", got)
			}
			if o := h.lastOutcome(f.ID); o != forms.OutcomeResubscribePending {
				t.Fatalf("outcome = %s", o)
			}
			ms := h.optinMessages(c.ID)
			if len(ms) != 1 || ms[0].Template != core.TemplateResubscribeConfirm {
				t.Fatalf("messages = %+v", ms)
			}
			if n := h.count(`SELECT count(*) FROM pending_optins WHERE contact_id = ? AND kind = 'resubscribe'`, c.ID); n != 1 {
				t.Fatalf("pending resubscribes = %d", n)
			}
			if len(h.consents(c.ID)) != 0 || len(h.enroller.Calls()) != 0 {
				t.Fatal("nothing may be recorded before the confirmation")
			}
		})
	}
}

func TestSubmitExistingPendingRefreshes(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	h.submit(f.PublicID, vals("email", "erin@example.com", "first_name", "Erin")).wantStatus(t, http.StatusSeeOther)
	c := h.contact("erin@example.com")
	first := h.confirmPath(c.ID)

	// Within ResendInterval: nothing is re-sent, and the pending opt-in is
	// left exactly as it is, so the link Erin already has still applies
	// what she submitted (not what a later submission says).
	var before [4]string
	pending := func() [4]string {
		t.Helper()
		var r [4]string
		if err := h.d.Read.QueryRow(`SELECT payload, ip, user_agent, consent_text FROM pending_optins WHERE contact_id = ?`, c.ID).
			Scan(&r[0], &r[1], &r[2], &r[3]); err != nil {
			t.Fatal(err)
		}
		return r
	}
	before = pending()
	h.clock.Advance(5 * time.Minute)
	h.submit(f.PublicID, vals("email", "erin@example.com", "first_name", "Mallory"), "RemoteAddr", "198.51.100.66:1",
		"User-Agent", "evil/1").wantStatus(t, http.StatusSeeOther)
	if ms := h.optinMessages(c.ID); len(ms) != 1 {
		t.Fatalf("resent within %v: %d messages", forms.ResendInterval, len(ms))
	}
	if o := h.lastOutcome(f.ID); o != forms.OutcomeOptinPending {
		t.Fatalf("outcome = %s", o)
	}
	if after := pending(); after != before {
		t.Fatalf("pending opt-in changed without a new email: %q, was %q", after, before)
	}
	// The first link still works (not re-sent, not replaced).
	h.publicReq("GET", first, "", "").wantStatus(t, http.StatusOK)

	// After ResendInterval: a new email with a new link; the old one is
	// replaced, and the pending opt-in carries the new submission.
	h.clock.Advance(6 * time.Minute)
	h.submit(f.PublicID, vals("email", "erin@example.com", "first_name", "Erina")).wantStatus(t, http.StatusSeeOther)
	if p := pending(); !strings.Contains(p[0], `"Erina"`) {
		t.Fatalf("payload not refreshed with the new email: %s", p[0])
	}
	if ms := h.optinMessages(c.ID); len(ms) != 2 {
		t.Fatalf("messages after the interval = %d", len(ms))
	}
	second := h.confirmPath(c.ID)
	if second == first {
		t.Fatal("a re-sent confirmation needs a new link")
	}
	r := h.publicReq("GET", first, "", "").wantStatus(t, http.StatusGone)
	if !strings.Contains(r.text(), "newer confirmation email") {
		t.Fatalf("superseded page: %s", r.body)
	}
	if n := h.count(`SELECT count(*) FROM pending_optins WHERE contact_id = ?`, c.ID); n != 1 {
		t.Fatalf("pending opt-ins = %d, want one refreshed row", n)
	}
	if n := h.count(`SELECT count(*) FROM consent_events WHERE contact_id = ? AND type = 'optin_requested'`, c.ID); n != 2 {
		t.Fatalf("optin_requested events = %d, want one per email sent", n)
	}
}

func TestSubmitRejectedSilently(t *testing.T) {
	tests := []struct {
		name   string
		status core.ContactStatus // "" = no contact
		supp   string             // suppressions reason, "" = none
	}{
		{"erased address", "", "erased"},
		{"manually suppressed address", "", "manual"},
		{"bounced contact", core.StatusBounced, ""},
		{"complained contact", core.StatusComplained, ""},
		{"suppressed_manual contact", core.StatusSuppressedManual, ""},
		{"active contact with suppression hash", core.StatusActive, "manual"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			f, _ := signupForm(h, "")
			var c core.Contact
			if tt.status != "" {
				c = testutil.InsertContact(t, h.d, "gone@example.com", tt.status)
			}
			if tt.supp != "" {
				testutil.InsertSuppression(t, h.d, "gone@example.com", tt.supp)
			}
			h.submit(f.PublicID, vals("email", "gone@example.com", "first_name", "X")).wantStatus(t, http.StatusSeeOther)
			if o := h.lastOutcome(f.ID); o != forms.OutcomeRejected {
				t.Fatalf("outcome = %s", o)
			}
			if tt.status == "" {
				if n := h.count(`SELECT count(*) FROM contacts`); n != 0 {
					t.Fatalf("contacts = %d", n)
				}
			} else if got := h.contact("gone@example.com"); got.Version != c.Version || got.Status != tt.status || len(got.Tags) != 0 {
				t.Fatalf("contact changed: %+v", got)
			}
			for _, q := range []string{`SELECT count(*) FROM messages`, `SELECT count(*) FROM consent_events`, `SELECT count(*) FROM pending_optins`} {
				if n := h.count(q); n != 0 {
					t.Fatalf("%s = %d", q, n)
				}
			}
			if len(h.enroller.Calls()) != 0 {
				t.Fatal("rejected sign-ups must not enrol")
			}
		})
	}
}

// TestIndistinguishableResponses: every accepted submission of a form
// gets byte-identical answers (F-7), for browsers and scripts.
func TestIndistinguishableResponses(t *testing.T) {
	for _, double := range []bool{false, true} {
		t.Run(fmt.Sprintf("double_optin=%v", double), func(t *testing.T) {
			h := newHarness(t)
			f, _ := signupForm(h, fmt.Sprintf(`,"double_optin":%v`, double))
			testutil.InsertContact(t, h.d, "active@example.com", core.StatusActive)
			testutil.InsertContact(t, h.d, "unsub@example.com", core.StatusUnsubscribed)
			testutil.InsertContact(t, h.d, "pending@example.com", core.StatusPending)
			testutil.InsertContact(t, h.d, "bounced@example.com", core.StatusBounced)
			testutil.InsertContact(t, h.d, "complained@example.com", core.StatusComplained)
			testutil.InsertSuppression(t, h.d, "erased@example.com", "erased")
			emails := []string{"new@example.com", "active@example.com", "unsub@example.com", "pending@example.com",
				"bounced@example.com", "complained@example.com", "erased@example.com"}
			var htmlRef, jsonRef *resp
			for _, e := range emails {
				r := h.submit(f.PublicID, vals("email", e))
				j := h.submitJSON(f.PublicID, fmt.Sprintf(`{"email":%q}`, e))
				for _, x := range []resp{r, j} {
					x.header.Del("RateLimit-Remaining")
					x.header.Del("RateLimit-Reset")
				}
				if htmlRef == nil {
					htmlRef, jsonRef = &r, &j
					continue
				}
				if r.status != htmlRef.status || !bytes.Equal(r.body, htmlRef.body) || fmt.Sprint(r.header) != fmt.Sprint(htmlRef.header) {
					t.Errorf("%s: form response differs: %d %v %s", e, r.status, r.header, r.body)
				}
				if j.status != jsonRef.status || !bytes.Equal(j.body, jsonRef.body) || fmt.Sprint(j.header) != fmt.Sprint(jsonRef.header) {
					t.Errorf("%s: JSON response differs: %d %s", e, j.status, j.body)
				}
			}
			htmlRef.wantStatus(t, http.StatusSeeOther)
			var body struct {
				OK          bool   `json:"ok"`
				RedirectURL string `json:"redirect_url"`
				Message     string `json:"message"`
			}
			jsonRef.wantStatus(t, http.StatusOK).decode(t, &body)
			if !body.OK || body.Message == "" {
				t.Fatalf("JSON body = %+v", body)
			}
			outcomes := map[string]int{}
			rows, err := h.d.Read.Query(`SELECT outcome FROM form_submissions`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var o string
				_ = rows.Scan(&o)
				outcomes[o]++
			}
			rows.Close()
			if outcomes[forms.OutcomeRejected] != 6 || outcomes[forms.OutcomeResubscribePending] != 2 {
				t.Fatalf("outcomes = %v", outcomes)
			}
		})
	}
}

func TestSubmitJSONAndRedirect(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"x","fields":[{"key":"attr.seats"},{"key":"attr.vip"}],"redirect_url":"https://example.com/thanks"}`)
	var body map[string]any
	h.submitJSON(f.PublicID, `{"email":"j@example.com","attr.seats":3,"attr.vip":true,"website":""}`).wantStatus(t, http.StatusOK).decode(t, &body)
	if body["ok"] != true || body["redirect_url"] != "https://example.com/thanks" {
		t.Fatalf("body = %v", body)
	}
	if c := h.contact("j@example.com"); c.Attributes["seats"] != "3" || c.Attributes["vip"] != "true" {
		t.Fatalf("attributes = %v", c.Attributes)
	}
	r := h.submit(f.PublicID, vals("email", "k@example.com")).wantStatus(t, http.StatusSeeOther)
	if r.header.Get("Location") != "https://example.com/thanks" {
		t.Fatalf("Location = %s", r.header.Get("Location"))
	}
	// XMLHttpRequest with a form-encoded body also gets JSON.
	h.submit(f.PublicID, vals("email", "l@example.com"), "X-Requested-With", "XMLHttpRequest").wantStatus(t, http.StatusOK).decode(t, &body)
	// Malformed JSON.
	h.submitJSON(f.PublicID, `{"email":`).wantError(t, http.StatusBadRequest, api.CodeValidation, "body")
	// Oversized body.
	h.submitJSON(f.PublicID, `{"email":"`+strings.Repeat("a", forms.MaxBodyBytes)+`"}`).wantError(t, http.StatusRequestEntityTooLarge, api.CodeValidation, "body")
}

func TestHoneypot(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, "")
	h.submit(f.PublicID, vals("email", "bot@example.com", "website", "https://spam.example")).wantStatus(t, http.StatusSeeOther)
	var body map[string]any
	h.submitJSON(f.PublicID, `{"email":"bot2@example.com","website":"x"}`).wantStatus(t, http.StatusOK).decode(t, &body)
	if body["ok"] != true {
		t.Fatalf("honeypot JSON = %v", body)
	}
	if n := h.count(`SELECT count(*) FROM contacts`); n != 0 {
		t.Fatalf("contacts = %d", n)
	}
	if n := h.count(`SELECT count(*) FROM form_submissions WHERE outcome = 'spam' AND contact_id IS NULL`); n != 2 {
		t.Fatalf("spam submissions = %d", n)
	}
}

func TestRateLimit(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, "")
	ip := "RemoteAddr"
	for i := range forms.SubmitBurst {
		h.submit(f.PublicID, vals("email", fmt.Sprintf("r%d@example.com", i)), ip, "203.0.113.50:1").wantStatus(t, http.StatusSeeOther)
	}
	r := h.submitJSON(f.PublicID, `{"email":"late@example.com"}`, ip, "203.0.113.50:2")
	r.wantError(t, http.StatusTooManyRequests, api.CodeRateLimited, "")
	if r.header.Get("Retry-After") == "" || r.header.Get("RateLimit-Limit") != "10" {
		t.Fatalf("headers = %v", r.header)
	}
	page := h.submit(f.PublicID, vals("email", "late2@example.com"), ip, "203.0.113.50:3").wantStatus(t, http.StatusTooManyRequests)
	if !strings.Contains(page.text(), "Too many requests") {
		t.Fatalf("page = %s", page.body)
	}
	if n := h.count(`SELECT count(*) FROM contacts WHERE email LIKE 'late%'`); n != 0 {
		t.Fatal("a rate-limited submission must not create a contact")
	}
	if n := h.count(`SELECT count(*) FROM form_submissions WHERE outcome = 'rate_limited'`); n != 2 {
		t.Fatalf("rate_limited rows = %d", n)
	}
	// Another address is unaffected; the same one recovers after a minute.
	h.submit(f.PublicID, vals("email", "other@example.com"), ip, "203.0.113.51:1").wantStatus(t, http.StatusSeeOther)
	h.clock.Advance(time.Minute)
	h.submit(f.PublicID, vals("email", "later@example.com"), ip, "203.0.113.50:4").wantStatus(t, http.StatusSeeOther)
}

func TestRequiredFieldsAndEmailValidation(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"Signup","fields":[{"key":"first_name","label":"First name","required":true},{"key":"attr.company"}]}`)
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{"missing email", `{"first_name":"A"}`, "email"},
		{"invalid email", `{"email":"not-an-email","first_name":"A"}`, "email"},
		{"missing required", `{"email":"a@example.com"}`, "first_name"},
		{"blank required", `{"email":"a@example.com","first_name":"   "}`, "first_name"},
		{"attribute too long", `{"email":"a@example.com","first_name":"A","attr.company":"` + strings.Repeat("x", forms.MaxValueLength+1) + `"}`, "attr.company"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.submitJSON(f.PublicID, tt.body).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, tt.field)
		})
	}
	// A browser gets the page again with the messages and its input kept.
	r := h.submit(f.PublicID, vals("email", "bad", "attr.company", "Acme <Inc>")).wantStatus(t, http.StatusUnprocessableEntity)
	page := r.text()
	for _, want := range []string{"Please enter a valid email address.", "Please fill in First name.", `value="Acme &lt;Inc&gt;"`, `aria-invalid="true"`, `role="alert"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if n := h.count(`SELECT count(*) FROM contacts`) + h.count(`SELECT count(*) FROM form_submissions`); n != 0 {
		t.Fatalf("invalid submissions must change nothing, rows = %d", n)
	}
	if errs := forms.ValidateSubmission(f, map[string]string{"email": "ok@example.com", "first_name": "A"}); len(errs) != 0 {
		t.Fatalf("valid submission errors = %v", errs)
	}
}

func TestCORS(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"x","allowed_origins":["https://example.com"]}`)
	path := "/f/" + f.PublicID

	ok := h.publicReq("OPTIONS", path, "", "", "Origin", "https://example.com", "Access-Control-Request-Method", "POST",
		"Access-Control-Request-Headers", "content-type").wantStatus(t, http.StatusNoContent)
	if ok.header.Get("Access-Control-Allow-Origin") != "https://example.com" || !strings.Contains(ok.header.Get("Access-Control-Allow-Headers"), "Content-Type") ||
		ok.header.Get("Access-Control-Allow-Methods") != "POST" {
		t.Fatalf("preflight headers = %v", ok.header)
	}
	denied := h.publicReq("OPTIONS", path, "", "", "Origin", "https://evil.example", "Access-Control-Request-Method", "POST")
	denied.wantError(t, http.StatusForbidden, api.CodeForbidden, "")
	if denied.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("a denied preflight must not carry CORS headers")
	}
	h.publicReq("OPTIONS", "/f/aaaaaaaaaa", "", "", "Origin", "https://example.com").wantStatus(t, http.StatusNotFound)

	// An allowed script reads the answer.
	r := h.submitJSON(f.PublicID, `{"email":"c1@example.com"}`, "Origin", "https://example.com", "Sec-Fetch-Mode", "cors").wantStatus(t, http.StatusOK)
	if r.header.Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Fatalf("POST headers = %v", r.header)
	}
	// A script on another site is refused.
	h.submit(f.PublicID, vals("email", "c2@example.com"), "Origin", "https://evil.example", "Sec-Fetch-Mode", "cors").
		wantError(t, http.StatusForbidden, api.CodeForbidden, "")
	if n := h.count(`SELECT count(*) FROM contacts WHERE email = 'c2@example.com'`); n != 0 {
		t.Fatal("a refused cross-origin script must not sign up")
	}
	// A plain HTML form on any site works (navigation, not CORS).
	nav := h.submit(f.PublicID, vals("email", "c3@example.com"), "Origin", "https://other.example", "Sec-Fetch-Mode", "navigate").wantStatus(t, http.StatusSeeOther)
	if nav.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no CORS headers for other origins")
	}
	h.contact("c3@example.com")
	// The hosted page's own origin is always allowed.
	self := h.submitJSON(f.PublicID, `{"email":"c4@example.com"}`, "Origin", baseURL, "Sec-Fetch-Mode", "cors").wantStatus(t, http.StatusOK)
	if self.header.Get("Access-Control-Allow-Origin") != baseURL {
		t.Fatalf("self headers = %v", self.header)
	}
}

func TestUnknownForm(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/f/aaaaaaaaaa", "/f/short", "/f/NOT-VALID!"} {
		r := h.publicReq("GET", p, "", "").wantStatus(t, http.StatusNotFound)
		if !strings.Contains(r.text(), "Form not found") || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %s", p, r.body)
		}
	}
	h.submitJSON("aaaaaaaaaa", `{"email":"a@example.com"}`).wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	h.submit("aaaaaaaaaa", vals("email", "a@example.com")).wantStatus(t, http.StatusNotFound)
}

func TestHostedPage(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"Join <us>","fields":[{"key":"first_name","required":true}],"consent_text":"We send email & count clicks."}`)
	r := h.publicReq("GET", "/f/"+f.PublicID, "", "").wantStatus(t, http.StatusOK)
	page := r.text()
	for _, want := range []string{
		`<html lang="en">`, `<h1>Join &lt;us&gt;</h1>`, `action="` + f.Endpoint + `"`, `<label for="f-email">`, `name="email"`, `type="email"`,
		`<label for="f-first_name">First name`, `required aria-required="true"`, `class="hp"`, `name="website"`,
		`.hp{position:absolute;left:-10000px`, `We send email &amp; count clicks.`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{"<script", "src=", "<link", "@import"} {
		if strings.Contains(page, bad) {
			t.Errorf("page must not load external assets or scripts (found %q)", bad)
		}
	}
	if csp := r.header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
	thanks := h.publicReq("GET", "/f/"+f.PublicID+"?submitted=1", "", "").wantStatus(t, http.StatusOK)
	if !strings.Contains(thanks.text(), "Thank you") || strings.Contains(thanks.text(), `name="email"`) {
		t.Fatalf("thank-you page = %s", thanks.body)
	}
	// HEAD works without a body.
	if head := h.publicReq("HEAD", "/f/"+f.PublicID, "", ""); head.status != http.StatusOK || len(head.body) != 0 {
		t.Fatalf("HEAD = %d %q", head.status, head.body)
	}
}

func TestEnrollErrorRollsBack(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, "")
	h.enroller.err = fmt.Errorf("sequence broken")
	h.submitJSON(f.PublicID, `{"email":"x@example.com"}`).wantError(t, http.StatusInternalServerError, api.CodeInternal, "")
	if n := h.count(`SELECT count(*) FROM contacts`) + h.count(`SELECT count(*) FROM form_submissions`); n != 0 {
		t.Fatalf("a failed sign-up must leave nothing, rows = %d", n)
	}
}

func TestNoEnrollerOrSequence(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"x","tags":["t"]}`)
	res, err := h.svc.Submit(t.Context(), f.ID, forms.SubmissionInput{Values: map[string]string{"email": "n@example.com"}, IP: "192.0.2.9"})
	if err != nil || res.Outcome != forms.OutcomeCreated || res.ContactID == 0 {
		t.Fatalf("Submit = %+v, %v", res, err)
	}
	if len(h.enroller.Calls()) != 0 {
		t.Fatal("no sequence: no enrolment")
	}
	if _, err := h.svc.Submit(t.Context(), f.ID, forms.SubmissionInput{Values: map[string]string{"email": "bad"}}); err == nil {
		t.Fatal("invalid email must fail")
	}
}

// TestConfirmationEmailsCapped: however often a form is submitted for a
// pending address, at most MaxConfirmationsPerDay confirmation emails go
// out in 24 hours and MaxConfirmationsPerMonth in 30 days (also across the
// expiry of the pending contact), the answer never changes (F-7) and the
// pending opt-in still expires OptinTTL after the first request.
func TestConfirmationEmailsCapped(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	first := h.clock.Now()
	var ref *resp
	for i := range 8 {
		r := h.submitJSON(f.PublicID, `{"email":"target@example.com"}`)
		r.header.Del("RateLimit-Remaining")
		r.header.Del("RateLimit-Reset")
		if ref == nil {
			ref = &r
		} else if r.status != ref.status || !bytes.Equal(r.body, ref.body) || fmt.Sprint(r.header) != fmt.Sprint(ref.header) {
			t.Fatalf("submission %d answered differently: %d %s", i, r.status, r.body)
		}
		h.clock.Advance(forms.ResendInterval + time.Minute)
	}
	c := h.contact("target@example.com")
	if ms := h.optinMessages(c.ID); len(ms) != forms.MaxConfirmationsPerDay {
		t.Fatalf("confirmation emails = %d, want %d", len(ms), forms.MaxConfirmationsPerDay)
	}
	var expires string
	if err := h.d.Read.QueryRow(`SELECT expires_at FROM pending_optins WHERE contact_id = ?`, c.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if want := db.FormatTime(first.Add(forms.OptinTTL).Truncate(time.Second)); expires != want {
		t.Fatalf("expires_at = %s, want %s (never extended)", expires, want)
	}
	// The newest link works.
	h.publicReq("GET", h.confirmPath(c.ID), "", "").wantStatus(t, http.StatusOK)

	// A day after the first email, one more may go out.
	h.clock.Set(first.Add(24*time.Hour + time.Minute))
	h.submitJSON(f.PublicID, `{"email":"target@example.com"}`).wantStatus(t, http.StatusOK)
	if ms := h.optinMessages(c.ID); len(ms) != forms.MaxConfirmationsPerDay+1 {
		t.Fatalf("confirmation emails after a day = %d", len(ms))
	}
	if n := h.count(`SELECT count(*) FROM pending_optins WHERE contact_id = ?`, c.ID); n != 1 {
		t.Fatalf("pending opt-ins = %d", n)
	}

	// The pending contact expires and is deleted; a new sign-up of the
	// address still counts the earlier emails (by address, not contact).
	h.clock.Set(first.Add(forms.OptinTTL + 2*time.Hour))
	if _, err := h.svc.Expire(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		h.submitJSON(f.PublicID, `{"email":"target@example.com"}`).wantStatus(t, http.StatusOK)
		h.clock.Advance(forms.ResendInterval + time.Minute)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE kind = 'optin'`); n != forms.MaxConfirmationsPerMonth {
		t.Fatalf("confirmation emails in 30 days = %d, want %d", n, forms.MaxConfirmationsPerMonth)
	}
}

// TestResubscribeConfirmationCooldown: an unsubscribed address gets one
// resubscribe confirmation email per ResubscribeCooldown, however often
// someone submits it; the cooldown starts again after a status change.
func TestResubscribeConfirmationCooldown(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, "")
	c := testutil.InsertContact(t, h.d, "gone@example.com", core.StatusUnsubscribed)
	first := h.clock.Now()
	for range 6 {
		h.submitJSON(f.PublicID, `{"email":"gone@example.com"}`).wantStatus(t, http.StatusOK)
		h.clock.Advance(forms.ResendInterval + time.Hour)
	}
	h.clock.Set(first.Add(forms.OptinTTL + time.Hour)) // the first request expired unanswered
	h.submitJSON(f.PublicID, `{"email":"gone@example.com"}`).wantStatus(t, http.StatusOK)
	if ms := h.optinMessages(c.ID); len(ms) != 1 {
		t.Fatalf("resubscribe emails within the cooldown = %d, want 1", len(ms))
	}
	h.clock.Set(first.Add(forms.ResubscribeCooldown + time.Minute))
	h.submitJSON(f.PublicID, `{"email":"gone@example.com"}`).wantStatus(t, http.StatusOK)
	if ms := h.optinMessages(c.ID); len(ms) != 2 {
		t.Fatalf("resubscribe emails after the cooldown = %d, want 2", len(ms))
	}
}

// TestConfirmationHourlyCaps: a double opt-in form can't be used to spray
// confirmation emails at many addresses: after MaxFormConfirmationsPerHour
// in an hour, sign-ups are still recorded but no email goes out, the owner
// is alerted, and sending resumes once the hour has passed.
func TestConfirmationHourlyCaps(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	submit := func(i int) {
		t.Helper()
		if _, err := h.svc.Submit(t.Context(), f.ID, forms.SubmissionInput{Values: map[string]string{"email": fmt.Sprintf("x%d@example.com", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range forms.MaxFormConfirmationsPerHour + 5 {
		submit(i)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE kind = 'optin'`); n != forms.MaxFormConfirmationsPerHour {
		t.Fatalf("confirmation emails = %d, want %d", n, forms.MaxFormConfirmationsPerHour)
	}
	if n := h.count(`SELECT count(*) FROM contacts WHERE status = 'pending'`); n != forms.MaxFormConfirmationsPerHour+5 {
		t.Fatalf("pending contacts = %d (sign-ups must still be recorded)", n)
	}
	keys := h.alerts.Keys()
	if len(keys) == 0 || keys[0] != fmt.Sprintf("%s:form:%d", forms.AlertOptinCap, f.ID) {
		t.Fatalf("alerts = %q", keys)
	}
	h.clock.Advance(time.Hour + time.Minute)
	submit(1000)
	if n := h.count(`SELECT count(*) FROM messages WHERE kind = 'optin'`); n != forms.MaxFormConfirmationsPerHour+1 {
		t.Fatalf("confirmation emails after an hour = %d", n)
	}

	// The cap of all forms together.
	g := h.createForm(`{"name":"Other","double_optin":true}`)
	g2 := h.createForm(`{"name":"Third","double_optin":true}`)
	for i := range forms.MaxConfirmationsPerHour + 10 {
		form := g.ID
		if i%2 == 1 {
			form = g2.ID
		}
		if _, err := h.svc.Submit(t.Context(), form, forms.SubmissionInput{Values: map[string]string{"email": fmt.Sprintf("y%d@example.com", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE kind = 'optin' AND queued_at > ?`, db.FormatTime(h.clock.Now().Add(-time.Hour))); n != forms.MaxConfirmationsPerHour {
		t.Fatalf("confirmation emails of all forms in an hour = %d, want %d", n, forms.MaxConfirmationsPerHour)
	}
	if keys := h.alerts.Keys(); keys[len(keys)-1] != forms.AlertOptinCap {
		t.Fatalf("alerts = %q", keys)
	}
}
