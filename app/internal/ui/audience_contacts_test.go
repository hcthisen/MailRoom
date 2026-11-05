package ui_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

func TestAudienceLoggedOut(t *testing.T) {
	hs := audHarness(t)
	audLoggedOut(t, hs, "/admin/contacts", "/admin/contacts/new", "/admin/contacts/1", "/admin/contacts/1/edit",
		"/admin/contacts/1/export", "/admin/tags", "/admin/tags/1", "/admin/suppressions", "/admin/imports",
		"/admin/imports/1", "/admin/segments", "/admin/segments/new", "/admin/segments/1", "/admin/segments/1/edit")
	// A logged-out POST goes to the login page too, and changes nothing.
	hs.anon().postRaw("/admin/contacts", url.Values{"email": {"x@example.org"}}).wantLogin(t)
	if n := hs.count(`SELECT COUNT(*) FROM contacts`); n != 0 {
		t.Fatalf("contacts = %d", n)
	}
}

func TestAudienceContactCreateAndList(t *testing.T) {
	hs := audHarness(t)
	c := hs.session()

	c.get("/admin/contacts").want(t, http.StatusOK).contains(t, "<h1>Contacts</h1>", "No contacts yet", `aria-current="page">Contacts`)
	c.get("/admin/contacts/new").want(t, http.StatusOK).contains(t, `name="email"`, `name="attributes"`, `name="consent.source"`)

	form := url.Values{"email": {" Ann@Example.org "}, "first_name": {"Ann"}, "last_name": {"Berg"}, "status": {"active"},
		"tags": {"customer, vip"}, "attributes": {"company: Acme\nseats: 5\nzip: \"01234\"\nbeta: true"},
		"consent.source": {"trade fair"}, "consent.text": {"Yes please"}}
	c.postNoCSRF("/admin/contacts", form).want(t, http.StatusForbidden)
	if n := hs.count(`SELECT COUNT(*) FROM contacts`); n != 0 {
		t.Fatalf("created without CSRF: %d", n)
	}
	r := c.post("/admin/contacts", form).want(t, http.StatusSeeOther)
	id := audLocationID(t, r, "/admin/contacts/")
	got, err := hs.app.Contacts.Get(hs.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "ann@example.org" || got.FirstName != "Ann" || len(got.Tags) != 2 {
		t.Fatalf("contact = %+v", got)
	}
	if got.Attributes["seats"] != float64(5) || got.Attributes["zip"] != "01234" || got.Attributes["beta"] != true || got.Attributes["company"] != "Acme" {
		t.Fatalf("attributes = %#v", got.Attributes)
	}
	if n := hs.count(`SELECT COUNT(*) FROM consent_events WHERE contact_id = ? AND source LIKE 'admin:%trade fair'`, id); n != 1 {
		t.Fatalf("consent events = %d", n)
	}
	c.get(fmt.Sprintf("/admin/contacts/%d", id)).want(t, http.StatusOK).
		contains(t, "Contact ann@example.org created.", "<h1>ann@example.org</h1>", "Ann Berg", "company", "Acme",
			`<span class="tl-type tl-consent">Consent</span>`, "Timeline", "Not in any sequence")

	// Validation and conflicts are shown on the form.
	c.post("/admin/contacts", url.Values{"email": {"not-an-email"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-email-error"`, `aria-invalid="true"`)
	c.post("/admin/contacts", url.Values{"email": {"ann@example.org"}}).want(t, http.StatusConflict).
		contains(t, `class="form-error"`)
	c.post("/admin/contacts", url.Values{"email": {"bo@example.org"}, "attributes": {"no colon here"}}).
		want(t, http.StatusUnprocessableEntity).contains(t, "line 1: write each attribute as key: value")

	// List, search and filters.
	testutil.InsertContact(t, hs.app.DB, "zed@example.org", core.StatusUnsubscribed)
	c.get("/admin/contacts").want(t, http.StatusOK).contains(t, "ann@example.org", "zed@example.org", `form="aud-bulk"`)
	c.get("/admin/contacts?q=berg").want(t, http.StatusOK).contains(t, "ann@example.org").lacks(t, "zed@example.org")
	c.get("/admin/contacts?tag=VIP").want(t, http.StatusOK).contains(t, "ann@example.org").lacks(t, "zed@example.org")
	c.get("/admin/contacts?status=unsubscribed").want(t, http.StatusOK).contains(t, "zed@example.org").lacks(t, "ann@example.org")
	c.get("/admin/contacts?q=nobody").want(t, http.StatusOK).contains(t, "No contacts match")
	c.get("/admin/contacts?created_after=2020-01-01&created_before=2099-12-31").want(t, http.StatusOK).contains(t, "ann@example.org")
	c.get("/admin/contacts?created_after=yesterday").want(t, http.StatusUnprocessableEntity).contains(t, "is not a date")
	c.get("/admin/contacts?status=weird").want(t, http.StatusUnprocessableEntity)
	c.get("/admin/contacts?limit=1").want(t, http.StatusOK).contains(t, "Next page")
}

func TestAudienceContactEditTagsStatus(t *testing.T) {
	hs := audHarness(t)
	c := hs.session()
	ct := testutil.InsertContact(t, hs.app.DB, "cy@example.org", core.StatusActive, "news")
	base := fmt.Sprintf("/admin/contacts/%d", ct.ID)
	if _, err := hs.app.Contacts.Update(hs.ctx, ct.ID, contacts.UpdateInput{Attributes: map[string]any{"code": "007", "n": 3.5, "gone": "x"}}, 0); err != nil {
		t.Fatal(err)
	}

	// Edit: the attributes round-trip with their types; a removed line
	// deletes the key.
	page := c.get(base+"/edit").want(t, http.StatusOK).contains(t, "code: 007\n", "n: 3.5", "gone: x")
	ver := page.field(t, "version")
	c.postNoCSRF(base+"/edit", url.Values{"first_name": {"Cy"}}).want(t, http.StatusForbidden)
	c.post(base+"/edit", url.Values{"first_name": {"Cy"}, "last_name": {""}, "version": {ver},
		"attributes": {"code: \"007\"\nn: 3.5\n"}}).wantRedirect(t, base)
	got, _ := hs.app.Contacts.Get(hs.ctx, ct.ID)
	if got.FirstName != "Cy" || got.Attributes["code"] != "007" || got.Attributes["n"] != 3.5 {
		t.Fatalf("after edit: %+v", got)
	}
	if _, ok := got.Attributes["gone"]; ok {
		t.Fatalf("removed attribute kept: %#v", got.Attributes)
	}
	// A stale version is refused (412) with a message above the form.
	c.post(base+"/edit", url.Values{"first_name": {"Old"}, "version": {ver}}).
		want(t, http.StatusPreconditionFailed).contains(t, "Someone else changed this")

	// Tags.
	c.post(base+"/tags", url.Values{"tags": {"alpha, beta"}}).wantRedirect(t, base)
	c.post(base+"/tags", url.Values{"tags": {""}}).want(t, http.StatusUnprocessableEntity).contains(t, "enter at least one tag")
	c.post(base+"/tags/remove", url.Values{"tag": {"news"}}).wantRedirect(t, base)
	got, _ = hs.app.Contacts.Get(hs.ctx, ct.ID)
	if strings.Join(got.Tags, ",") != "alpha,beta" {
		t.Fatalf("tags = %v", got.Tags)
	}
	c.get(base).want(t, http.StatusOK).contains(t, "Removed tag news.", `aria-label="Remove tag alpha"`)

	// Status with the service's rules: unsubscribe, then resubscribing
	// needs a note.
	c.postNoCSRF(base+"/status", url.Values{"status": {"unsubscribed"}}).want(t, http.StatusForbidden)
	c.post(base+"/status", url.Values{"status": {"unsubscribed"}}).wantRedirect(t, base)
	got, _ = hs.app.Contacts.Get(hs.ctx, ct.ID)
	if got.Status != core.StatusUnsubscribed {
		t.Fatalf("status = %s", got.Status)
	}
	r := c.post(base+"/status", url.Values{"status": {"active"}})
	if r.code != http.StatusUnprocessableEntity && r.code != http.StatusForbidden {
		t.Fatalf("resubscribe without note: %d", r.code)
	}
	r.contains(t, "Change status")
	c.post(base+"/status", url.Values{"status": {"active"}, "note": {"asked by email"}}).wantRedirect(t, base)
	c.get(base).want(t, http.StatusOK).contains(t, "Status changed to Active.")
}

func TestAudienceContactExportErase(t *testing.T) {
	hs := audHarness(t)
	c := hs.session()
	ct := testutil.InsertContact(t, hs.app.DB, "dee@example.org", core.StatusActive, "news")
	base := fmt.Sprintf("/admin/contacts/%d", ct.ID)

	r := c.get(base+"/export").want(t, http.StatusOK).contains(t, `"email": "dee@example.org"`, `"consent_log"`)
	if cd := r.header.Get("Content-Disposition"); cd != fmt.Sprintf(`attachment; filename="contact-%d.json"`, ct.ID) {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}

	c.postNoCSRF(base+"/erase", nil).want(t, http.StatusForbidden)
	// Step 1: the dry run's counts on a confirmation page; nothing changes.
	c.post(base+"/erase", url.Values{"version": {"1"}}).want(t, http.StatusOK).
		contains(t, "Erase dee@example.org?", "1 tag", "can&#39;t be undone", `name="confirm" value="yes"`, "Erase contact")
	if n := hs.count(`SELECT COUNT(*) FROM contacts WHERE id = ?`, ct.ID); n != 1 {
		t.Fatal("erased by the dry run")
	}
	// Step 2.
	c.post(base+"/erase", url.Values{"confirm": {"yes"}}).wantRedirect(t, "/admin/contacts")
	if n := hs.count(`SELECT COUNT(*) FROM contacts WHERE id = ?`, ct.ID); n != 0 {
		t.Fatal("not erased")
	}
	if n := hs.count(`SELECT COUNT(*) FROM suppressions WHERE email_hash = ? AND reason = 'erased'`, core.EmailHash("dee@example.org")); n != 1 {
		t.Fatalf("suppression = %d", n)
	}
	c.get("/admin/contacts").want(t, http.StatusOK).contains(t, "The contact was erased.")
	c.get(base).want(t, http.StatusNotFound)
}

func TestAudienceBulkTag(t *testing.T) {
	hs := audHarness(t)
	hs.audStartJobs()
	c := hs.session()
	a := testutil.InsertContact(t, hs.app.DB, "a@example.org", core.StatusActive)
	b := testutil.InsertContact(t, hs.app.DB, "b@example.org", core.StatusActive)
	testutil.InsertContact(t, hs.app.DB, "c@example.org", core.StatusActive)

	form := url.Values{"tag": {"promo"}, "action": {"add"}, "back": {"/admin/contacts?q=example"},
		"contact_ids": {fmt.Sprint(a.ID), fmt.Sprint(b.ID)}}
	c.postNoCSRF("/admin/contacts/bulk-tag", form).want(t, http.StatusForbidden)
	c.post("/admin/contacts/bulk-tag", form).want(t, http.StatusOK).
		contains(t, "Add tag &#34;promo&#34; to 2 contacts?", "2 contacts matched.", `name="contact_ids"`)
	if n := hs.count(`SELECT COUNT(*) FROM jobs`); n != 0 {
		t.Fatalf("dry run submitted %d jobs", n)
	}
	form.Set("confirm", "yes")
	c.post("/admin/contacts/bulk-tag", form).wantRedirect(t, "/admin/contacts?q=example")
	var jobID int64
	if err := hs.app.DB.Read.QueryRow(`SELECT id FROM jobs WHERE type = 'tags.bulk'`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	hs.audWaitJob(jobID)
	if n := hs.count(`SELECT COUNT(*) FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id WHERE t.name = 'promo'`); n != 2 {
		t.Fatalf("tagged = %d", n)
	}

	// Nothing selected: an error flash on the page it came from.
	c.post("/admin/contacts/bulk-tag", url.Values{"tag": {"promo"}, "action": {"add"}}).wantRedirect(t, "/admin/contacts")
	c.get("/admin/contacts").contains(t, "select at least one contact")
	// An outside return address is not followed.
	c.post("/admin/contacts/bulk-tag", url.Values{"tag": {"x"}, "back": {"https://evil.example/"}}).wantRedirect(t, "/admin/")
}
