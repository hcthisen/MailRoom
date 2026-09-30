package app_test

// End-to-end tests (ARCHITECTURE.md §9) through the real HTTP handler with a
// fake email provider and a fake clock. Background workers are driven
// deterministically by calling their step functions (sequences RunDue,
// sending SendDue, broadcasts Tick); only the jobs runner runs on its own,
// and tests wait for its jobs.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/app"
	"mailroom/internal/auth"
	"mailroom/internal/core"
	"mailroom/internal/forms"
	"mailroom/internal/sequences"
)

// TestE2EBootstrap: the first admin and API token are created with the
// functions "mailroom admin create" and "mailroom token create" use; the
// token then works on the API and the admin can sign in and use the API
// with a session (CSRF-protected for writes).
func TestE2EBootstrap(t *testing.T) {
	hs := newHarness(t, nil)
	ctx := context.Background()

	user, password, err := app.CreateAdmin(ctx, hs.app.DB, "Owner@Example.org", "")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "owner@example.org" || len(password) < 16 {
		t.Fatalf("admin = %+v, generated password of %d chars", user, len(password))
	}
	tok, secret, err := app.CreateToken(ctx, hs.app.DB, auth.TokenInput{Name: "setup-agent", Scope: api.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, api.TokenPrefix) || tok.Scope != api.ScopeAdmin {
		t.Fatalf("token = %+v", tok)
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE actor_type = 'cli'`); n < 2 {
		t.Fatalf("CLI audit entries = %d, want the user and the token", n)
	}

	var me struct {
		Scope string `json:"scope"`
		Token *struct {
			Name string `json:"name"`
		} `json:"token"`
		User *struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	hs.raw("GET", api.Prefix+"/me", "", "", "Authorization", "Bearer "+secret).want(t, http.StatusOK).decode(t, &me)
	if me.Scope != string(api.ScopeAdmin) || me.Token == nil || me.Token.Name != "setup-agent" {
		t.Fatalf("me (token) = %+v", me)
	}
	hs.raw("GET", api.Prefix+"/me", "", "", "Authorization", "Bearer "+secret+"x").want(t, http.StatusUnauthorized)

	sess, err := hs.app.Auth.Login(ctx, auth.LoginInput{Email: "owner@example.org", Password: password, IP: "192.0.2.10"})
	if err != nil {
		t.Fatalf("login with the generated password: %v", err)
	}
	cookie := api.SessionCookie + "=" + sess.Token
	me.User, me.Token = nil, nil
	hs.raw("GET", api.Prefix+"/me", "", "", "Cookie", cookie).want(t, http.StatusOK).decode(t, &me)
	if me.User == nil || me.User.Email != "owner@example.org" {
		t.Fatalf("me (session) = %+v", me)
	}
	patch := `{"company_footer":"Example Ltd, 2 Street, Town"}`
	hs.raw("PATCH", api.Prefix+"/settings", "application/json", patch, "Cookie", cookie).want(t, http.StatusForbidden)
	hs.raw("PATCH", api.Prefix+"/settings", "application/json", patch, "Cookie", cookie, api.CSRFHeader, sess.CSRFToken).want(t, http.StatusOK)
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE actor_type = 'user' AND operation LIKE 'settings%'`); n != 1 {
		t.Fatalf("settings audit entries by the user = %d", n)
	}
}

// dripStep is one step of a sequence created through the API.
type dripStep struct {
	subject string
	delay   int64 // seconds
}

// createSequence creates a sequence and its steps through the API (not
// YAML): create-sequence, then add-sequence-step with If-Match.
func (hs *harness) createSequence(body string, steps ...dripStep) sequences.Sequence {
	hs.t.Helper()
	var seq sequences.Sequence
	r := hs.api("POST", "/sequences", body).want(hs.t, http.StatusCreated)
	r.decode(hs.t, &seq)
	etag := r.header.Get("ETag")
	for i, st := range steps {
		body := fmt.Sprintf(`{"email":{"subject":%q,"body":"Hi {{first_name | \"there\"}}, this is email %d. [Start here](https://example.org/start-%d)"},"delay_seconds":%d}`,
			st.subject, i+1, i+1, st.delay)
		r := hs.api("POST", fmt.Sprintf("/sequences/%d/steps", seq.ID), body, "If-Match", etag).want(hs.t, http.StatusCreated)
		etag = r.header.Get("ETag")
	}
	hs.api("GET", fmt.Sprintf("/sequences/%d", seq.ID), "").want(hs.t, http.StatusOK).decode(hs.t, &seq)
	if len(seq.Steps) != len(steps) {
		hs.t.Fatalf("sequence has %d steps, want %d", len(seq.Steps), len(steps))
	}
	return seq
}

// createForm creates a form through the API.
func (hs *harness) createForm(body string) forms.Form {
	hs.t.Helper()
	var f forms.Form
	hs.api("POST", "/forms", body).want(hs.t, http.StatusCreated).decode(hs.t, &f)
	return f
}

// TestE2EFormSequenceDrip: a base layout, a single opt-in form and a
// three-step sequence with exit tag "customer" are set up through the API.
// Sign-ups through the public form become active and enrolled; as the
// clock advances the scheduler and sender deliver the steps in order with
// the layout and the RFC 8058 headers, and the exit tag added mid-way
// stops the remaining steps for that contact only.
func TestE2EFormSequenceDrip(t *testing.T) {
	hs := newHarness(t, nil)
	hs.api("PUT", "/layout", `{"logo_url":"https://example.org/logo.png","brand_color":"#aa3300"}`).want(t, http.StatusOK)
	seq := hs.createSequence(`{"name":"Community welcome","exit_tags":["customer"]}`,
		dripStep{"Welcome, {{first_name | \"friend\"}}", 0},
		dripStep{"Day one, {{first_name | \"friend\"}}", 86400},
		dripStep{"Day three, {{first_name | \"friend\"}}", 2 * 86400},
	)
	f := hs.createForm(fmt.Sprintf(`{"name":"Community","fields":[{"key":"first_name","label":"First name"}],"tags":["joined"],"sequence_id":%d}`, seq.ID))
	if f.DoubleOptin {
		t.Fatal("double opt-in must be off by default")
	}

	// A script posts JSON; a plain HTML form posts form-encoded and is
	// redirected to the thank-you page.
	var ok struct {
		OK bool `json:"ok"`
	}
	hs.raw("POST", "/f/"+f.PublicID, "application/json", `{"email":"Anna@Example.org","first_name":"Anna"}`).want(t, http.StatusOK).decode(t, &ok)
	if !ok.OK {
		t.Fatal("JSON sign-up not accepted")
	}
	form := url.Values{"email": {"bob@example.org"}, "first_name": {"Bob"}}.Encode()
	if r := hs.raw("POST", "/f/"+f.PublicID, "application/x-www-form-urlencoded", form); r.code != http.StatusSeeOther {
		t.Fatalf("form-encoded sign-up = %d %s", r.code, r.body)
	}

	anna, bob := hs.mustContact("anna@example.org"), hs.mustContact("bob@example.org")
	for _, c := range []struct {
		id   int64
		tags []string
		st   core.ContactStatus
	}{{anna.ID, anna.Tags, anna.Status}, {bob.ID, bob.Tags, bob.Status}} {
		if c.st != core.StatusActive || len(c.tags) != 1 || c.tags[0] != "joined" {
			t.Fatalf("contact %d: %s %v", c.id, c.st, c.tags)
		}
		if st, _ := hs.enrollment(seq.ID, c.id); st != sequences.EnrollmentActive {
			t.Fatalf("contact %d enrollment = %s", c.id, st)
		}
	}
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE source = ?`, "form:"+f.PublicID); n != 2 {
		t.Fatalf("form enrollments = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE type = 'signup' AND source = ?`, "form:"+f.PublicID); n != 2 {
		t.Fatalf("sign-up consent events = %d", n)
	}

	hs.schedule()
	hs.sendAll()
	if got := hs.subjectsTo("anna@example.org"); len(got) != 1 || got[0] != "Welcome, Anna" {
		t.Fatalf("anna after step 1: %q", got)
	}
	if got := hs.subjectsTo("bob@example.org"); len(got) != 1 || got[0] != "Welcome, Bob" {
		t.Fatalf("bob after step 1: %q", got)
	}

	// Bob becomes a customer: his drip ends before step 2.
	hs.api("POST", fmt.Sprintf("/contacts/%d/tags", bob.ID), `{"tags":["customer"]}`).want(t, http.StatusOK)
	if st, reason := hs.enrollment(seq.ID, bob.ID); st != sequences.EnrollmentExited || reason != sequences.ExitTag {
		t.Fatalf("bob after the exit tag: %s %s", st, reason)
	}

	hs.clock.Advance(day)
	hs.schedule()
	hs.sendAll()
	hs.clock.Advance(2 * day)
	hs.schedule()
	hs.sendAll()
	hs.clock.Advance(10 * day)
	hs.schedule()
	hs.sendAll()

	annaMail := hs.mailTo("anna@example.org")
	want := []string{"Welcome, Anna", "Day one, Anna", "Day three, Anna"}
	if len(annaMail) != len(want) {
		t.Fatalf("anna got %d emails, want %d", len(annaMail), len(want))
	}
	for i, m := range annaMail {
		if s := m.Header.Get("Subject"); s != want[i] {
			t.Errorf("email %d subject = %q, want %q", i+1, s, want[i])
		}
		m.unsubscribePath(t)
		if !strings.Contains(m.HTML, "#aa3300") || !strings.Contains(m.HTML, "https://example.org/logo.png") {
			t.Errorf("email %d is not wrapped in the base layout", i+1)
		}
		if !strings.Contains(m.HTML, "Example Ltd, 1 Street, Town") {
			t.Errorf("email %d lacks the company footer", i+1)
		}
		if links := m.links("/c/"); len(links) == 0 {
			t.Errorf("email %d: links are not rewritten for click tracking", i+1)
		}
		if strings.Contains(m.HTML, "https://example.org/start-") {
			t.Errorf("email %d contains an untracked link", i+1)
		}
		if from := m.Header.Get("From"); !strings.Contains(from, "news@example.com") {
			t.Errorf("email %d From = %q", i+1, from)
		}
	}
	if n := len(hs.mailTo("bob@example.org")); n != 1 {
		t.Fatalf("bob got %d emails after exiting, want 1", n)
	}
	if st, _ := hs.enrollment(seq.ID, anna.ID); st != sequences.EnrollmentCompleted {
		t.Fatalf("anna enrollment = %s", st)
	}
	var stats sequences.SequenceStats
	hs.api("GET", fmt.Sprintf("/sequences/%d/stats", seq.ID), "").want(t, http.StatusOK).decode(t, &stats)
	if stats.Completed != 1 || stats.Exited != 1 || stats.Active != 0 || len(stats.Steps) != 3 ||
		stats.Steps[0].Sent != 2 || stats.Steps[1].Sent != 1 || stats.Steps[2].Sent != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

// TestE2EDoubleOptIn: a double opt-in sign-up stays pending with no tags
// and no enrolment until the confirmation link is used. Opening the link
// shows a button and changes nothing (mail scanners); posting it activates
// the contact, applies the form's tags and enrols it. The link works once.
func TestE2EDoubleOptIn(t *testing.T) {
	hs := newHarness(t, nil)
	seq := hs.createSequence(`{"name":"Newsletter welcome"}`, dripStep{"Welcome aboard", 0})
	f := hs.createForm(fmt.Sprintf(`{"name":"Newsletter","tags":["newsletter"],"sequence_id":%d,"double_optin":true}`, seq.ID))

	hs.raw("POST", "/f/"+f.PublicID, "application/json", `{"email":"carol@example.org"}`).want(t, http.StatusOK)
	carol := hs.mustContact("carol@example.org")
	if carol.Status != core.StatusPending || len(carol.Tags) != 0 {
		t.Fatalf("before confirming: %s %v", carol.Status, carol.Tags)
	}
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE contact_id = ?`, carol.ID); n != 0 {
		t.Fatalf("enrolled before confirming: %d", n)
	}

	hs.schedule()
	hs.sendAll()
	sent := hs.mailTo("carol@example.org")
	if len(sent) != 1 {
		t.Fatalf("carol got %d emails before confirming, want the confirmation only", len(sent))
	}
	links := sent[0].links("/f/confirm/")
	if len(links) == 0 {
		t.Fatalf("no confirmation link in %q", sent[0].Text)
	}
	if strings.Contains(sent[0].HTML, testBaseURL+"/c/") {
		t.Fatal("the confirmation email has tracked links (K-3)")
	}
	confirm := links[0]

	page := hs.raw("GET", confirm, "", "").want(t, http.StatusOK)
	if !strings.Contains(string(page.body), "Confirm") {
		t.Fatalf("confirmation page = %s", page.body)
	}
	if st := hs.contactStatus(carol.ID); st != core.StatusPending {
		t.Fatalf("opening the link changed the status to %s", st)
	}
	hs.raw("POST", confirm, "application/x-www-form-urlencoded", "").want(t, http.StatusOK)

	carol = hs.mustContact("carol@example.org")
	if carol.Status != core.StatusActive || len(carol.Tags) != 1 || carol.Tags[0] != "newsletter" {
		t.Fatalf("after confirming: %s %v", carol.Status, carol.Tags)
	}
	if st, _ := hs.enrollment(seq.ID, carol.ID); st != sequences.EnrollmentActive {
		t.Fatalf("enrollment after confirming = %s", st)
	}
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ? AND type = 'optin_confirmed'`, carol.ID); n != 1 {
		t.Fatalf("optin_confirmed events = %d", n)
	}
	hs.schedule()
	hs.sendAll()
	if got := hs.subjectsTo("carol@example.org"); len(got) != 2 || got[1] != "Welcome aboard" {
		t.Fatalf("after confirming carol got %q", got)
	}
	if r := hs.raw("POST", confirm, "application/x-www-form-urlencoded", ""); r.code != http.StatusGone {
		t.Fatalf("second confirm = %d, want 410", r.code)
	}
}

// TestE2EFormCantRewriteSubscriber: the public form needs no login, so a
// stranger who knows a subscriber's address must not be able to change
// what real mail says about them or sign them up for more. On a single
// opt-in form the stored first name stays (only missing values are filled
// in). On a double opt-in form nothing happens until the subscriber clicks
// the confirmation: no tag (which could start a tag-triggered sequence),
// no enrolment, no signup consent. The click applies them, and still
// keeps the stored name.
func TestE2EFormCantRewriteSubscriber(t *testing.T) {
	hs := newHarness(t, nil)
	seq := hs.createSequence(`{"name":"Webinar","trigger_tag":"webinar-signup"}`, dripStep{"Webinar details", 0})
	single := hs.createForm(`{"name":"Lead magnet","fields":[{"key":"first_name"},{"key":"attr.company"}]}`)
	double := hs.createForm(`{"name":"Webinar","fields":[{"key":"first_name"}],"tags":["webinar-signup"],"double_optin":true}`)
	gina := hs.createContact(`{"email":"gina@example.org","first_name":"Gina"}`)

	hs.raw("POST", "/f/"+single.PublicID, "application/json",
		`{"email":"gina@example.org","first_name":"URGENT verify your account at evil.example","attr.company":"Ginaco"}`).want(t, http.StatusOK)
	got := hs.mustContact(gina.Email)
	if got.FirstName != "Gina" || got.Attributes["company"] != "Ginaco" {
		t.Fatalf("after a single opt-in submission: %q %v, want the stored name kept and the missing company filled in", got.FirstName, got.Attributes)
	}

	hs.raw("POST", "/f/"+double.PublicID, "application/json",
		`{"email":"gina@example.org","first_name":"Mallory-was-here"}`).want(t, http.StatusOK)
	got = hs.mustContact(gina.Email)
	if got.FirstName != "Gina" || len(got.Tags) != 0 || got.Status != core.StatusActive {
		t.Fatalf("before confirming: %+v", got)
	}
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE contact_id = ?`, gina.ID); n != 0 {
		t.Fatalf("enrolled before confirming: %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ? AND type = 'signup' AND form_id = ?`, gina.ID, double.ID); n != 0 {
		t.Fatal("a signup consent was recorded before the confirmation")
	}

	hs.schedule()
	hs.sendAll()
	sent := hs.mailTo(gina.Email)
	if len(sent) != 1 || len(sent[0].links("/f/confirm/")) == 0 {
		t.Fatalf("gina got %d emails, want only the confirmation", len(sent))
	}
	hs.raw("POST", sent[0].links("/f/confirm/")[0], "application/x-www-form-urlencoded", "").want(t, http.StatusOK)
	got = hs.mustContact(gina.Email)
	if got.FirstName != "Gina" || len(got.Tags) != 1 || got.Tags[0] != "webinar-signup" {
		t.Fatalf("after confirming: %q %v", got.FirstName, got.Tags)
	}
	if st, _ := hs.enrollment(seq.ID, gina.ID); st != sequences.EnrollmentActive {
		t.Fatalf("enrollment after confirming = %s", st)
	}
	hs.schedule()
	hs.sendAll()
	sent = hs.mailTo(gina.Email)
	if len(sent) != 2 || !strings.Contains(sent[1].Text, "Hi Gina") {
		t.Fatalf("mail after confirming = %d, want the drip addressed to Gina", len(sent))
	}
}
