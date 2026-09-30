package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/app"
	"mailroom/internal/auth"
	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/imports"
)

// migrationCSV is a GHL export in the import format: every status, tags,
// consent data, an attribute, an invalid address and a duplicate row.
const migrationCSV = `email,first_name,last_name,status,tags,consent_source,consent_date,attr.company
alice@example.org,Alice,Ames,active,vip;newsletter,ghl-form,2025-03-01T10:00:00Z,Acme
ulla@example.org,Ulla,,unsubscribed,newsletter,ghl-form,2025-03-02T10:00:00Z,
bert@example.org,Bert,,bounced,,ghl-import,2025-03-03T10:00:00Z,
cleo@example.org,Cleo,,complained,,ghl-import,2025-03-04T10:00:00Z,
not-an-address,Nobody,,active,,ghl-form,2025-03-05T10:00:00Z,
ALICE@example.org,,,,customer,ghl-form,2025-03-01T10:00:00Z,
dora@example.org,Dora,,,,ghl-form,2025-03-06T10:00:00Z,
`

// TestE2EImportDryRunThenCommit: the dry run reports what the import
// would do and writes nothing (no contact, no job); the real import runs
// as a job and does exactly that, with consent events from the file, and
// only the active contacts are sendable afterwards.
func TestE2EImportDryRunThenCommit(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()

	dry := hs.importCSV(migrationCSV, true)
	check := func(label string, r imports.ImportReport) {
		t.Helper()
		if r.Rows != 7 || r.Created != 5 || r.RejectedCount != 1 || r.DuplicatesMerged != 1 || len(r.Rejected) != 1 || r.Rejected[0].Line != 6 {
			t.Fatalf("%s report = %+v", label, r)
		}
		wantStatus := map[string]int{"active": 2, "unsubscribed": 1, "bounced": 1, "complained": 1}
		for k, v := range wantStatus {
			if r.StatusCounts[k] != v {
				t.Fatalf("%s status counts = %v", label, r.StatusCounts)
			}
		}
		if r.TagCounts["newsletter"] != 2 || r.TagCounts["vip"] != 1 || r.TagCounts["customer"] != 1 {
			t.Fatalf("%s tag counts = %v", label, r.TagCounts)
		}
	}
	check("dry run", dry)
	if !dry.DryRun {
		t.Fatal("the dry run says it wrote")
	}
	if n := hs.count(`SELECT count(*) FROM contacts`); n != 0 {
		t.Fatalf("the dry run created %d contacts", n)
	}
	if n := hs.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("the dry run submitted %d jobs", n)
	}

	done := hs.importCSV(migrationCSV, false)
	check("import", done)
	if done.DryRun {
		t.Fatal("the import says it was a dry run")
	}
	for email, st := range map[string]core.ContactStatus{
		"alice@example.org": core.StatusActive, "ulla@example.org": core.StatusUnsubscribed,
		"bert@example.org": core.StatusBounced, "cleo@example.org": core.StatusComplained, "dora@example.org": core.StatusActive,
	} {
		c := hs.mustContact(email)
		if c.Status != st {
			t.Fatalf("%s = %s, want %s", email, c.Status, st)
		}
		if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ? AND type = 'import' AND source LIKE 'import:ghl-%'`, c.ID); n != 1 {
			t.Fatalf("%s consent events = %d", email, n)
		}
	}
	alice := hs.mustContact("alice@example.org")
	if alice.FirstName != "Alice" || alice.Attributes["company"] != "Acme" || len(alice.Tags) != 3 {
		t.Fatalf("alice (merged duplicate) = %+v", alice)
	}
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ? AND occurred_at LIKE '2025-03-01T10:00:00%'`, alice.ID); n != 1 {
		t.Fatal("the consent date is not the file's")
	}
	var list struct{ Items []contacts.Contact }
	hs.api("GET", "/contacts?status=active", "").want(t, http.StatusOK).decode(t, &list)
	if len(list.Items) != 2 {
		t.Fatalf("active contacts = %d", len(list.Items))
	}

	// Importing the same file again changes nothing.
	again := hs.importCSV(migrationCSV, false)
	if again.Created != 0 || again.Unchanged+again.Updated != 5 {
		t.Fatalf("second import = %+v", again)
	}
	b := hs.createBroadcast("Welcome to the new list", "We moved", `{"type":"all"}`)
	if n := hs.sendBroadcast(b.ID); n != 2 {
		t.Fatalf("broadcast recipients = %d, want the 2 active contacts", n)
	}
}

// TestE2EAPISafety: an Idempotency-Key replay returns the stored response
// without a second write (and a different body under the same key is
// refused), If-Match with a stale ETag gets 412, and a read-scope token
// can read but not write.
func TestE2EAPISafety(t *testing.T) {
	hs := newHarness(t, nil)
	body := `{"email":"jack@example.org","first_name":"Jack"}`
	first := hs.api("POST", "/contacts", body, api.IdempotencyHeader, "create-jack").want(t, http.StatusCreated)
	replay := hs.api("POST", "/contacts", body, api.IdempotencyHeader, "create-jack").want(t, http.StatusCreated)
	if string(replay.body) != string(first.body) || replay.header.Get(api.IdempotencyReplayHeader) != "true" {
		t.Fatalf("replay = %s (%q)", replay.body, replay.header.Get(api.IdempotencyReplayHeader))
	}
	if n := hs.count(`SELECT count(*) FROM contacts`); n != 1 {
		t.Fatalf("contacts = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'contact.create'`); n != 1 {
		t.Fatalf("contact.create audit entries = %d", n)
	}
	errorCode(t, hs.api("POST", "/contacts", `{"email":"jill@example.org"}`, api.IdempotencyHeader, "create-jack").want(t, http.StatusUnprocessableEntity), api.CodeValidation)
	// Without a key the same POST is a new request: the address exists.
	errorCode(t, hs.api("POST", "/contacts", body).want(t, http.StatusConflict), api.CodeConflict)

	var jack contacts.Contact
	first.decode(t, &jack)
	get := hs.api("GET", fmt.Sprintf("/contacts/%d", jack.ID), "").want(t, http.StatusOK)
	etag := get.header.Get("ETag")
	if etag != api.ETag(jack.Version) {
		t.Fatalf("ETag = %q, want %q", etag, api.ETag(jack.Version))
	}
	upd := hs.api("PATCH", fmt.Sprintf("/contacts/%d", jack.ID), `{"last_name":"Jones"}`, "If-Match", etag).want(t, http.StatusOK)
	if upd.header.Get("ETag") == etag {
		t.Fatal("the ETag did not change")
	}
	errorCode(t, hs.api("PATCH", fmt.Sprintf("/contacts/%d", jack.ID), `{"last_name":"Smith"}`, "If-Match", etag).
		want(t, http.StatusPreconditionFailed), api.CodePreconditionFailed)
	var got contacts.Contact
	hs.api("GET", fmt.Sprintf("/contacts/%d", jack.ID), "").decode(t, &got)
	if got.LastName != "Jones" {
		t.Fatalf("the stale update overwrote the change: %q", got.LastName)
	}

	_, read, err := app.CreateToken(context.Background(), hs.app.DB, auth.TokenInput{Name: "reporting", Scope: api.ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	asReader := func(method, path, body string) resp {
		ct := ""
		if body != "" {
			ct = "application/json"
		}
		return hs.raw(method, api.Prefix+path, ct, body, "Authorization", "Bearer "+read)
	}
	asReader("GET", "/contacts", "").want(t, http.StatusOK)
	asReader("GET", fmt.Sprintf("/contacts/%d/timeline", jack.ID), "").want(t, http.StatusOK)
	errorCode(t, asReader("POST", "/contacts", `{"email":"kim@example.org"}`).want(t, http.StatusForbidden), api.CodeForbidden)
	errorCode(t, asReader("PATCH", fmt.Sprintf("/contacts/%d", jack.ID), `{"first_name":"J"}`).want(t, http.StatusForbidden), api.CodeForbidden)
	errorCode(t, asReader("DELETE", fmt.Sprintf("/contacts/%d", jack.ID), "").want(t, http.StatusForbidden), api.CodeForbidden)
	if n := hs.count(`SELECT count(*) FROM contacts`); n != 1 {
		t.Fatalf("contacts after the read token's writes = %d", n)
	}
	errorCode(t, hs.raw("GET", api.Prefix+"/contacts", "", "").want(t, http.StatusUnauthorized), api.CodeUnauthorized)
}

// errorCode checks the API error format and code.
func errorCode(t *testing.T, r resp, code string) {
	t.Helper()
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil || e.Error.Code != code || e.Error.Message == "" {
		t.Fatalf("error body = %s, want code %s", r.body, code)
	}
}

// TestE2EDashboardAfterImport: AGENTS.md phase 7 checks contacts_total and
// contacts_active on the dashboard right after an import. A dashboard read
// just before (the admin UI, a baseline) must not hide the import behind
// the one-minute cache, and neither may an erase.
func TestE2EDashboardAfterImport(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	type dash struct {
		Total  int64 `json:"contacts_total"`
		Active int64 `json:"contacts_active"`
	}
	read := func() dash {
		t.Helper()
		var d dash
		hs.api("GET", "/stats/dashboard", "").want(t, http.StatusOK).decode(t, &d)
		return d
	}
	if d := read(); d.Total != 0 {
		t.Fatalf("baseline = %+v", d)
	}
	rep := hs.importCSV(migrationCSV, false)
	if d := read(); d.Total != int64(rep.Created) || d.Active != 2 {
		t.Fatalf("dashboard right after the import = %+v, want %d contacts, 2 active", d, rep.Created)
	}
	alice := hs.mustContact("alice@example.org")
	hs.api("DELETE", fmt.Sprintf("/contacts/%d", alice.ID), "").want(t, http.StatusOK)
	if d := read(); d.Total != int64(rep.Created)-1 || d.Active != 1 {
		t.Fatalf("dashboard right after an erase = %+v", d)
	}
}
