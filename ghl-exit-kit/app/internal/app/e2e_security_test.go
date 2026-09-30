package app_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/app"
	"mailroom/internal/auth"
	"mailroom/internal/core"
	"mailroom/internal/sending"
)

// readToken creates a read-scope API token.
func (hs *harness) readToken() string {
	hs.t.Helper()
	_, token, err := app.CreateToken(hs.ctx, hs.app.DB, auth.TokenInput{Name: "reporting", Scope: api.ScopeRead})
	if err != nil {
		hs.t.Fatal(err)
	}
	return token
}

// TestE2EEventsNeverShowUnsubscribeURLs: SES copies the original headers,
// List-Unsubscribe included, into every event. A read-scope token (GET
// only) must not be able to collect working one-click unsubscribe URLs
// from GET /events, GET /events/{id} or the contact export.
func TestE2EEventsNeverShowUnsubscribeURLs(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	gina := hs.createContact(`{"email":"gina@example.org","first_name":"Gina"}`)
	b := hs.createBroadcast("June", "June news", `{"type":"all"}`)
	hs.sendBroadcast(b.ID)
	hs.sendAll()
	mails := hs.mailTo("gina@example.org")
	if len(mails) != 1 {
		t.Fatalf("gina got %d emails", len(mails))
	}
	lu := mails[0].Header.Get("List-Unsubscribe")
	unsub := mails[0].unsubscribePath(t)
	sesID := hs.sesMessageID(hs.messageIDs(gina.ID, core.KindBroadcast)[0])

	ev := hs.deliveryEvent(sesID, "gina@example.org")
	m := ev["mail"].(map[string]any)
	m["headersTruncated"] = false
	m["headers"] = []map[string]string{
		{"name": "List-Unsubscribe", "value": lu},
		{"name": "List-Unsubscribe-Post", "value": "List-Unsubscribe=One-Click"},
		{"name": "Subject", "value": "June news"},
	}
	m["commonHeaders"] = map[string]any{"subject": "June news", "to": []string{"gina@example.org"}}
	hs.notify("sns-delivery-1", ev, "2")

	read := hs.readToken()
	get := func(path string) resp {
		t.Helper()
		return hs.raw("GET", api.Prefix+path, "", "", "Authorization", "Bearer "+read).want(t, http.StatusOK)
	}
	var list struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	r := get("/events?type=delivery")
	r.decode(t, &list)
	if len(list.Items) == 0 {
		t.Fatalf("no delivery events: %s", r.body)
	}
	bodies := []resp{r, get(fmt.Sprintf("/events/%d", list.Items[0].ID)), get(fmt.Sprintf("/contacts/%d/export", gina.ID))}
	for _, b := range bodies {
		if s := string(b.body); strings.Contains(s, "/u/") || strings.Contains(s, strings.TrimPrefix(unsub, "/u/")) {
			t.Fatalf("a read token sees an unsubscribe URL: %s", s)
		}
	}
	if got := hs.tablesContaining(strings.TrimPrefix(unsub, "/u/")); len(got) != 0 {
		t.Fatalf("the unsubscribe token is stored in %v", got)
	}
}

// TestE2EConfirmationEmailCarriesNoSubmittedText: an anonymous visitor
// can't have their own text emailed from the verified domain: the
// confirmation email of a double opt-in form doesn't greet by the
// submitted first name (which may hold a phishing URL).
func TestE2EConfirmationEmailCarriesNoSubmittedText(t *testing.T) {
	hs := newHarness(t, nil)
	f := hs.createForm(`{"name":"Newsletter","fields":[{"key":"first_name"}],"double_optin":true}`)
	hs.raw("POST", "/f/"+f.PublicID, "application/json",
		`{"email":"victim@example.org","first_name":"Your account is locked, verify at https://evil.example now"}`).want(t, http.StatusOK)
	hs.schedule()
	hs.sendAll()
	sent := hs.mailTo("victim@example.org")
	if len(sent) != 1 {
		t.Fatalf("got %d emails, want the confirmation", len(sent))
	}
	for _, part := range []string{sent[0].HTML, sent[0].Text, sent[0].Header.Get("Subject")} {
		if strings.Contains(part, "evil.example") || strings.Contains(part, "account is locked") {
			t.Fatalf("the confirmation email carries the submitted text:\n%s", part)
		}
	}
}

// TestE2EMarketingNeedsCompanyFooter: no marketing email goes out without
// the sender's name and postal address (E-1). The footer can't be cleared
// through the API; when it is empty (COMPANY_FOOTER was never set), a
// broadcast can't be sent, a sequence can't be resumed, sequence emails
// wait (nothing is lost) and the owner is alerted; once it is set, they
// go out with it.
func TestE2EMarketingNeedsCompanyFooter(t *testing.T) {
	hs := newHarness(t, nil)
	etag := hs.api("GET", "/settings", "").want(t, http.StatusOK).header.Get("ETag")
	r := hs.api("PATCH", "/settings", `{"company_footer":"  "}`, "If-Match", etag)
	errorCode(t, r, "validation")

	// As if COMPANY_FOOTER had never been set.
	if _, err := hs.app.DB.Write.Exec(`UPDATE settings SET value = json_set(value, '$.company_footer', '') WHERE key = 'app'`); err != nil {
		t.Fatal(err)
	}
	hs.restart()
	seq := hs.createSequence(`{"name":"Drip","trigger_tag":"joined"}`, dripStep{"Drip one", 0})
	c := hs.createContact(`{"email":"reader@example.org","tags":["joined"]}`)
	hs.schedule()
	hs.sendAll()
	if got := hs.subjectsTo(c.Email); len(got) != 0 {
		t.Fatalf("marketing email without a footer: %q", got)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'sequence' AND status = 'queued'`); n != 1 {
		t.Fatalf("held sequence messages = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'alert' AND json_extract(data, '$.key') = ?`, sending.AlertNoFooter); n != 1 {
		t.Fatalf("footer alerts = %d", n)
	}
	b := hs.createBroadcast("News", "News", `{"type":"all"}`)
	errorCode(t, hs.api("POST", fmt.Sprintf("/broadcasts/%d/send", b.ID), ""), "validation")
	hs.api("POST", fmt.Sprintf("/sequences/%d/pause", seq.ID), "").want(t, http.StatusOK)
	errorCode(t, hs.api("POST", fmt.Sprintf("/sequences/%d/resume", seq.ID), ""), "validation")

	etag = hs.api("GET", "/settings", "").want(t, http.StatusOK).header.Get("ETag")
	hs.api("PATCH", "/settings", `{"company_footer":"Example Ltd, 1 Street, Town"}`, "If-Match", etag).want(t, http.StatusOK)
	hs.api("POST", fmt.Sprintf("/sequences/%d/resume", seq.ID), "").want(t, http.StatusOK)
	hs.clock.Advance(sending.FooterWait + time.Minute)
	hs.sendAll()
	mails := hs.mailTo(c.Email)
	if len(mails) != 1 || !strings.Contains(mails[0].Text, "Example Ltd, 1 Street, Town") {
		t.Fatalf("after the footer was set: %d emails", len(mails))
	}
}

// TestE2EUnknownQueryParameterWritesNothing: an agent that sends dry_run to
// an operation without dry runs, or misspells it, gets a 422 and nothing
// is written: no contact (and no tag-triggered enrolment), no broadcast
// send.
func TestE2EUnknownQueryParameterWritesNothing(t *testing.T) {
	hs := newHarness(t, nil)
	hs.createSequence(`{"name":"Welcome","trigger_tag":"joined"}`, dripStep{"Hello", 0})
	r := hs.api("POST", "/contacts?dry_run=true", `{"email":"dry@example.org","tags":["joined"]}`)
	errorCode(t, r, "validation")
	if !strings.Contains(string(r.body), `"field":"query.dry_run"`) {
		t.Fatalf("error = %s", r.body)
	}
	if _, ok := hs.contactByEmail("dry@example.org"); ok {
		t.Fatal("a dry_run the operation doesn't take created the contact")
	}
	hs.createContact(`{"email":"reader@example.org"}`)
	b := hs.createBroadcast("News", "News", `{"type":"all"}`)
	r = hs.api("POST", fmt.Sprintf("/broadcasts/%d/send?dryrun=true", b.ID), "")
	errorCode(t, r, "validation")
	if got := hs.getBroadcast(b.ID); got.Status != "draft" {
		t.Fatalf("a misspelled dry run sent the broadcast: %s", got.Status)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'broadcast'`); n != 0 {
		t.Fatalf("broadcast messages = %d", n)
	}
	var res struct {
		DryRun     bool  `json:"dry_run"`
		Recipients int64 `json:"recipients"`
	}
	hs.api("POST", fmt.Sprintf("/broadcasts/%d/send?dry_run=true", b.ID), "").want(t, http.StatusOK).decode(t, &res)
	if !res.DryRun || res.Recipients != 1 {
		t.Fatalf("dry run = %+v", res)
	}
}
