package contacts_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

func TestCreateAndGetContact(t *testing.T) {
	hs := newHarness(t)
	consentAt := testutil.Epoch.Add(-48 * time.Hour)
	r := hs.do("POST", "/contacts", fmt.Sprintf(`{"email":" Alice@Example.com ","first_name":"Alice","last_name":"Smith",
		"attributes":{"company":"Acme","seats":3},"tags":["newsletter","VIP"],
		"consent":{"source":"webinar","occurred_at":%q,"text":"Yes please"}}`, consentAt.Format(time.RFC3339)))
	r.wantStatus(t, http.StatusCreated)
	var c contacts.Contact
	r.decode(t, &c)
	if c.Email != "alice@example.com" || c.Status != core.StatusActive || c.Version != 1 ||
		strings.Join(c.Tags, ",") != "newsletter,VIP" || c.Attributes["company"] != "Acme" {
		t.Fatalf("created = %+v", c)
	}
	if r.header.Get("ETag") != `"v1"` {
		t.Fatalf("ETag = %q", r.header.Get("ETag"))
	}

	var typ, source, text, occurred string
	if err := hs.d.Read.QueryRow(`SELECT type, source, consent_text, occurred_at FROM consent_events WHERE contact_id = ?`, c.ID).
		Scan(&typ, &source, &text, &occurred); err != nil {
		t.Fatal(err)
	}
	if typ != "api" || source != "api:setup-agent:webinar" || text != "Yes please" || occurred != db.FormatTime(consentAt) {
		t.Fatalf("consent = %s %s %q %s", typ, source, text, occurred)
	}
	var changes string
	if err := hs.d.Read.QueryRow(`SELECT changes FROM audit_log WHERE operation = 'contact.create' AND target_id = ? AND actor_name = 'setup-agent'`,
		fmt.Sprint(c.ID)).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(changes, "@") {
		t.Fatalf("audit must not hold the address: %s", changes)
	}
	if n := len(hs.tagEvents()); n != 2 {
		t.Fatalf("TagAdded events = %d", n)
	}

	got := hs.doAs(readToken, "GET", fmt.Sprintf("/contacts/%d", c.ID), "").wantStatus(t, http.StatusOK)
	if got.header.Get("ETag") != `"v1"` {
		t.Fatalf("GET ETag = %q", got.header.Get("ETag"))
	}
	hs.do("GET", "/contacts/9999", "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")

	// A UI user records admin consent.
	u := hs.doAs("session", "POST", "/contacts", `{"email":"bob@example.com"}`).wantStatus(t, http.StatusCreated)
	var bob contacts.Contact
	u.decode(t, &bob)
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ? AND type = 'admin' AND source = 'admin:owner@example.com'`, bob.ID); n != 1 {
		t.Fatalf("admin consent rows = %d", n)
	}
}

func TestCreateContactErrors(t *testing.T) {
	hs := newHarness(t)
	hs.createContact(`{"email":"taken@example.com"}`)
	testutil.InsertSuppression(t, hs.d, "gone@example.com", "erased")
	future := testutil.Epoch.Add(time.Hour).Format(time.RFC3339)
	tests := []struct {
		name, body string
		status     int
		code       string
		field      string
		message    string
	}{
		{"duplicate", `{"email":"TAKEN@example.com"}`, 409, api.CodeConflict, "", ""},
		{"suppressed", `{"email":"gone@example.com"}`, 409, api.CodeConflict, "email", "address is suppressed"},
		{"invalid email", `{"email":"nope"}`, 422, api.CodeValidation, "email", ""},
		{"missing email", `{}`, 422, api.CodeValidation, "email", ""},
		{"bad status", `{"email":"a@example.com","status":"deleted"}`, 422, api.CodeValidation, "status", ""},
		{"bad attribute", `{"email":"a@example.com","attributes":{"a b":1}}`, 422, api.CodeValidation, "attributes", ""},
		{"empty tag", `{"email":"a@example.com","tags":[" "]}`, 422, api.CodeValidation, "tags", ""},
		{"future consent", `{"email":"a@example.com","consent":{"occurred_at":"` + future + `"}}`, 422, api.CodeValidation, "consent.occurred_at", ""},
		{"malformed json", `{"email":`, 400, api.CodeValidation, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := hs.do("POST", "/contacts", tt.body).wantError(t, tt.status, tt.code, tt.field)
			if tt.message != "" && e.Message != tt.message {
				t.Fatalf("message = %q", e.Message)
			}
		})
	}
	hs.doAs(readToken, "POST", "/contacts", `{"email":"r@example.com"}`).wantError(t, http.StatusForbidden, api.CodeForbidden, "")
	hs.doAs("", "GET", "/contacts", "").wantError(t, http.StatusUnauthorized, api.CodeUnauthorized, "")
	if n := hs.count(`SELECT count(*) FROM contacts`); n != 1 {
		t.Fatalf("contacts = %d", n)
	}
}

func TestListContacts(t *testing.T) {
	hs := newHarness(t)
	mk := func(body string, at time.Time) contacts.Contact {
		hs.clock.Set(at)
		return hs.createContact(body)
	}
	day := func(n int) time.Time { return testutil.Epoch.Add(time.Duration(n) * 24 * time.Hour) }
	alice := mk(`{"email":"alice@acme.com","first_name":"Alice","last_name":"Smith","attributes":{"company":"Acme","seats":3,"trial":true},"tags":["vip"]}`, day(0))
	bob := mk(`{"email":"bob@example.com","first_name":"Bob","last_name":"Jones","attributes":{"company":"Beta","seats":10},"tags":["newsletter"]}`, day(1))
	carol := mk(`{"email":"carol@example.com","first_name":"Carol","last_name":"Smith","status":"unsubscribed","tags":["VIP","newsletter"]}`, day(2))
	dan := mk(`{"email":"dan_x@example.com","attributes":{"company":"Acme"}}`, day(3))

	ids := func(r resp) string {
		var body api.ListBody[contacts.Contact]
		r.wantStatus(t, http.StatusOK).decode(t, &body)
		var s []string
		for _, c := range body.Items {
			s = append(s, fmt.Sprint(c.ID))
		}
		return strings.Join(s, ",")
	}
	join := func(cs ...contacts.Contact) string {
		var s []string
		for _, c := range cs {
			s = append(s, fmt.Sprint(c.ID))
		}
		return strings.Join(s, ",")
	}
	tests := []struct {
		query string
		want  string
	}{
		{"", join(dan, carol, bob, alice)},
		{"q=smith", join(carol, alice)},
		{"q=ALICE%20SMITH", join(alice)},
		{"q=acme.com", join(alice)},
		{"q=_x", join(dan)}, // _ is literal, not a wildcard
		{"q=%25", ""},
		{"email=bob@example.com", join(bob)},
		{"email=%20Carol@Example.COM%20", join(carol)},
		{"email=ob@example.com", ""}, // exact, not contains
		{"status=unsubscribed", join(carol)},
		{"status=active", join(dan, bob, alice)},
		{"tag=vip", join(carol, alice)},
		{"tag=NEWSLETTER&status=active", join(bob)},
		{"tag=unknown", ""},
		{"created_after=" + url.QueryEscape(day(1).Format(time.RFC3339)), join(dan, carol, bob)},
		{"created_before=" + url.QueryEscape(day(1).Format(time.RFC3339)), join(alice)},
		{"attr=company:Acme", join(dan, alice)},
		{"attr=company:Acme&attr=seats:3", join(alice)},
		{"attr=seats:10", join(bob)},
		{"attr=trial:true", join(alice)},
		{"attr=company:acme", ""},
		{"segment_id=1", join(dan, bob, alice)},
		{"segment_id=2&q=carol", join(carol)},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			if got := ids(hs.doAs(readToken, "GET", "/contacts?"+tt.query, "")); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}

	// Items include tags.
	var body api.ListBody[contacts.Contact]
	hs.do("GET", "/contacts?tag=vip", "").decode(t, &body)
	// carol's "VIP" joined the existing tag "vip".
	if strings.Join(body.Items[0].Tags, ",") != "newsletter,vip" {
		t.Fatalf("tags = %v", body.Items[0].Tags)
	}

	// Pagination walks every contact exactly once.
	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		var b api.ListBody[contacts.Contact]
		hs.do("GET", "/contacts?limit=3&cursor="+cursor, "").wantStatus(t, http.StatusOK).decode(t, &b)
		for _, c := range b.Items {
			seen = append(seen, fmt.Sprint(c.ID))
		}
		if b.NextCursor == "" {
			break
		}
		cursor = b.NextCursor
	}
	if strings.Join(seen, ",") != join(dan, carol, bob, alice) {
		t.Fatalf("paged = %v", seen)
	}

	errs := []struct{ query, field string }{
		{"cursor=bogus", "query.cursor"},
		{"attr=nocolon", "query.attr"},
		{"attr=bad-key:1", "query.attr"},
		{"status=gone", "query.status"},
		{"email=not-an-address", "query.email"},
		{"segment_id=99", ""},
		{"limit=501", "query.limit"},
	}
	for _, e := range errs {
		r := hs.do("GET", "/contacts?"+e.query, "")
		if e.field == "" {
			r.wantError(t, http.StatusNotFound, api.CodeNotFound, "")
			continue
		}
		r.wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, e.field)
	}
}

func TestListWithoutSegments(t *testing.T) {
	d := testutil.NewDB(t)
	clock := testutil.NewClock(time.Time{})
	svc := contacts.New(contacts.Deps{DB: d, Clock: clock})
	_, err := svc.List(t.Context(), contacts.ListFilter{SegmentID: 1})
	var ve *core.ValidationError
	if !errors.As(err, &ve) || ve.Field != "segment_id" {
		t.Fatalf("err = %v", err)
	}
}

func TestUpsertContact(t *testing.T) {
	hs := newHarness(t)
	var out struct {
		Contact contacts.Contact `json:"contact"`
		Created bool             `json:"created"`
	}
	hs.do("POST", "/contacts/upsert", `{"email":"u@example.com","first_name":"U","attributes":{"a":1,"gone":null},"tags":["one"]}`).
		wantStatus(t, http.StatusOK).decode(t, &out)
	if !out.Created || out.Contact.FirstName != "U" || out.Contact.Status != core.StatusActive || len(out.Contact.Attributes) != 1 {
		t.Fatalf("created = %+v", out)
	}
	id := out.Contact.ID
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ?`, id); n != 1 {
		t.Fatalf("consent rows = %d", n)
	}

	// Existing and unsubscribed: names/attributes/tags change, status doesn't.
	testutil.Exec(t, hs.d, `UPDATE contacts SET status = 'unsubscribed' WHERE id = ?`, id)
	out.Contact = contacts.Contact{} // json.Unmarshal merges into existing maps
	hs.do("POST", "/contacts/upsert", `{"email":"U@EXAMPLE.com","last_name":"Last","attributes":{"a":null,"b":"x"},"tags":["two"]}`).
		wantStatus(t, http.StatusOK).decode(t, &out)
	c := out.Contact
	if out.Created || c.ID != id || c.FirstName != "U" || c.LastName != "Last" || c.Status != core.StatusUnsubscribed ||
		strings.Join(c.Tags, ",") != "one,two" || c.Attributes["b"] != "x" || c.Attributes["a"] != nil {
		t.Fatalf("updated = %+v", out)
	}
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ?`, id); n != 1 {
		t.Fatal("update without consent must not log consent")
	}
	// With consent, it is logged.
	hs.do("POST", "/contacts/upsert", `{"email":"u@example.com","consent":{"text":"again"}}`).wantStatus(t, http.StatusOK)
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ?`, id); n != 2 {
		t.Fatal("consent on update must be logged")
	}
	// No-op upsert writes no audit entry.
	before := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'contact.upsert'`)
	hs.do("POST", "/contacts/upsert", `{"email":"u@example.com","tags":["one"]}`).wantStatus(t, http.StatusOK)
	if after := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'contact.upsert'`); after != before {
		t.Fatalf("no-op upsert audited: %d -> %d", before, after)
	}

	testutil.InsertSuppression(t, hs.d, "blocked@example.com", "manual")
	e := hs.do("POST", "/contacts/upsert", `{"email":"blocked@example.com"}`).wantError(t, http.StatusConflict, api.CodeConflict, "email")
	if e.Message != "address is suppressed" {
		t.Fatalf("message = %q", e.Message)
	}
}

func TestUpdateContact(t *testing.T) {
	hs := newHarness(t)
	c := hs.createContact(`{"email":"p@example.com","first_name":"P","attributes":{"keep":1,"drop":2}}`)
	path := fmt.Sprintf("/contacts/%d", c.ID)

	r := hs.do("PATCH", path, `{"first_name":"Pat","attributes":{"drop":null,"new":"n"}}`, "If-Match", `"v1"`).wantStatus(t, http.StatusOK)
	var got contacts.Contact
	r.decode(t, &got)
	if got.FirstName != "Pat" || got.Version != 2 || got.Attributes["keep"] != float64(1) || got.Attributes["new"] != "n" {
		t.Fatalf("patched = %+v", got)
	}
	if _, ok := got.Attributes["drop"]; ok || r.header.Get("ETag") != `"v2"` {
		t.Fatalf("drop not deleted or ETag %q", r.header.Get("ETag"))
	}
	var changes string
	if err := hs.d.Read.QueryRow(`SELECT changes FROM audit_log WHERE operation = 'contact.update'`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if changes != `{"fields":["first_name","attributes.drop","attributes.new"]}` {
		t.Fatalf("audit changes = %s", changes)
	}

	hs.do("PATCH", path, `{"last_name":"X"}`, "If-Match", `"v1"`).wantError(t, http.StatusPreconditionFailed, api.CodePreconditionFailed, "")
	hs.do("PATCH", path, `{"last_name":"X"}`, "If-Match", `garbage`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "header.If-Match")
	hs.do("PATCH", path, `{"attributes":{"bad key":1}}`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "attributes")
	hs.do("PATCH", "/contacts/9999", `{"last_name":"X"}`).wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	// Without If-Match the update applies.
	hs.do("PATCH", path, `{"last_name":"X"}`).wantStatus(t, http.StatusOK)
}

func TestSetContactStatus(t *testing.T) {
	hs := newHarness(t)
	c := hs.createContact(`{"email":"s@example.com"}`)
	path := fmt.Sprintf("/contacts/%d/status", c.ID)
	type out struct {
		Contact contacts.Contact `json:"contact"`
		Changed bool             `json:"changed"`
	}
	var o out
	hs.do("POST", path, `{"status":"unsubscribed","note":"asked"}`, "If-Match", `"v1"`).wantStatus(t, http.StatusOK).decode(t, &o)
	if !o.Changed || o.Contact.Status != core.StatusUnsubscribed || o.Contact.Version != 2 {
		t.Fatalf("unsubscribe = %+v", o)
	}
	var source, actor, note string
	if err := hs.d.Read.QueryRow(`SELECT source, actor, note FROM contact_status_changes WHERE contact_id = ?`, c.ID).Scan(&source, &actor, &note); err != nil {
		t.Fatal(err)
	}
	if source != "api" || actor != "token:1" || note != "asked" {
		t.Fatalf("status change = %s %s %s", source, actor, note)
	}
	hs.do("POST", path, `{"status":"unsubscribed"}`).wantStatus(t, http.StatusOK).decode(t, &o)
	if o.Changed {
		t.Fatal("same status must be a no-op")
	}
	// Resubscribe needs a note.
	hs.do("POST", path, `{"status":"active"}`).wantError(t, http.StatusForbidden, api.CodeForbidden, "")
	hs.do("POST", path, `{"status":"active","note":"customer asked"}`, "If-Match", `"v1"`).wantError(t, http.StatusPreconditionFailed, api.CodePreconditionFailed, "")
	hs.doAs("session", "POST", path, `{"status":"active","note":"customer asked"}`).wantStatus(t, http.StatusOK)
	if n := hs.count(`SELECT count(*) FROM contact_status_changes WHERE contact_id = ? AND source = 'admin' AND actor = 'user:7'`, c.ID); n != 1 {
		t.Fatal("UI change must be recorded with source admin")
	}
	// Complained is final; pending is unreachable; unknown values are rejected.
	testutil.Exec(t, hs.d, `UPDATE contacts SET status = 'complained' WHERE id = ?`, c.ID)
	hs.do("POST", path, `{"status":"active","note":"please"}`).wantError(t, http.StatusForbidden, api.CodeForbidden, "")
	hs.do("POST", path, `{"status":"pending"}`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "status")
	hs.do("POST", path, `{"status":"active","note":"`+strings.Repeat("n", 501)+`"}`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "note")
	hs.do("POST", "/contacts/9999/status", `{"status":"active","note":"x"}`).wantError(t, http.StatusNotFound, api.CodeNotFound, "")
}

func TestContactTags(t *testing.T) {
	hs := newHarness(t)
	c := hs.createContact(`{"email":"t@example.com","tags":["lead"]}`)
	var got contacts.Contact
	hs.do("POST", fmt.Sprintf("/contacts/%d/tags", c.ID), `{"tags":["customer","LEAD"]}`).wantStatus(t, http.StatusOK).decode(t, &got)
	if strings.Join(got.Tags, ",") != "customer,lead" || got.Version != 2 {
		t.Fatalf("after add = %+v", got)
	}
	ev := hs.tagEvents()
	if len(ev) != 2 || ev[1].TagName != "customer" || ev[1].Source != "api" {
		t.Fatalf("TagAdded = %+v", ev)
	}
	r := hs.do("DELETE", fmt.Sprintf("/contacts/%d/tags/%s", c.ID, url.PathEscape("LEAD")), "").wantStatus(t, http.StatusOK)
	r.decode(t, &got)
	if strings.Join(got.Tags, ",") != "customer" || r.header.Get("ETag") != `"v3"` {
		t.Fatalf("after remove = %+v", got)
	}
	// Removing a missing tag is fine and changes nothing.
	hs.do("DELETE", fmt.Sprintf("/contacts/%d/tags/nothere", c.ID), "").wantStatus(t, http.StatusOK).decode(t, &got)
	if got.Version != 3 {
		t.Fatal("no-op removal bumped the version")
	}
	hs.do("POST", fmt.Sprintf("/contacts/%d/tags", c.ID), `{"tags":[]}`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "tags")
	hs.do("POST", "/contacts/9999/tags", `{"tags":["x"]}`).wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation IN ('contact.tags.add', 'contact.tags.remove')`); n != 2 {
		t.Fatalf("tag audit rows = %d", n)
	}
}
