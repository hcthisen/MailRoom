package contacts_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

// richContact creates a contact with consent, a status change, tags,
// messages, events, clicks and an enrollment, each at a distinct time.
func richContact(hs *harness) (c contacts.Contact, msgs []int64) {
	hs.t.Helper()
	t0 := testutil.Epoch
	hs.clock.Set(t0)
	c = hs.createContact(`{"email":"rich@example.com","first_name":"Rich","tags":["vip"]}`)
	m1 := hs.insertMessage(c.ID, c.Email, "broadcast", "delivered", "April news", t0.Add(1*time.Hour))
	hs.insertEvent(m1, "delivery", "", t0.Add(2*time.Hour))
	hs.insertClick(c.ID, m1, "https://example.com/a", t0.Add(3*time.Hour))
	m2 := hs.insertMessage(c.ID, c.Email, "sequence", "bounced", "Welcome", t0.Add(4*time.Hour))
	hs.insertEvent(m2, "bounce", "transient", t0.Add(5*time.Hour))
	m3 := hs.insertMessage(c.ID, c.Email, "broadcast", "queued", "May news", t0.Add(6*time.Hour))
	hs.clock.Set(t0.Add(7 * time.Hour))
	hs.do("POST", fmt.Sprintf("/contacts/%d/status", c.ID), `{"status":"unsubscribed"}`).wantStatus(hs.t, http.StatusOK)
	emailID := testutil.InsertEmail(hs.t, hs.d, "s", "b")
	seq := testutil.InsertSequence(hs.t, hs.d, "Welcome series")
	step := testutil.InsertStep(hs.t, hs.d, seq, 2, 60, emailID)
	testutil.InsertEnrollment(hs.t, hs.d, seq, c.ID, step)
	return c, []int64{m1, m2, m3}
}

func TestTimeline(t *testing.T) {
	hs := newHarness(t)
	c, _ := richContact(hs)
	var page contacts.TimelinePage
	hs.doAs(readToken, "GET", fmt.Sprintf("/contacts/%d/timeline", c.ID), "").wantStatus(t, http.StatusOK).decode(t, &page)
	var types []string
	for _, it := range page.Items {
		types = append(types, it.Type)
	}
	// Newest first; consent and tag share the creation time (tag ranks first).
	want := "status_change,message,bounce,message,click,delivery,message,tag_added,consent"
	if strings.Join(types, ",") != want {
		t.Fatalf("types = %s", strings.Join(types, ","))
	}
	if page.NextCursor != "" {
		t.Fatal("single page must have no cursor")
	}
	byType := map[string]contacts.TimelineItem{}
	for _, it := range page.Items {
		if _, ok := byType[it.Type]; !ok {
			byType[it.Type] = it
		}
	}
	checks := map[string]string{
		"status_change": "Status changed from active to unsubscribed (api)",
		"bounce":        "Bounce (transient): Welcome",
		"click":         "Clicked https://example.com/a",
		"tag_added":     "Tag added: vip",
		"message":       "Broadcast email queued: May news",
		"consent":       "Consent (api) from api:setup-agent",
	}
	for typ, summary := range checks {
		if byType[typ].Summary != summary {
			t.Errorf("%s summary = %q, want %q", typ, byType[typ].Summary, summary)
		}
	}
	if byType["click"].Detail["url"] != "https://example.com/a" || byType["message"].Detail["subject"] != "May news" {
		t.Fatalf("details = %+v / %+v", byType["click"].Detail, byType["message"].Detail)
	}

	// Pages of 2 return the same items in the same order.
	var paged []string
	cursor := ""
	for i := 0; i < 10; i++ {
		var p contacts.TimelinePage
		hs.do("GET", fmt.Sprintf("/contacts/%d/timeline?limit=2&cursor=%s", c.ID, cursor), "").wantStatus(t, http.StatusOK).decode(t, &p)
		for _, it := range p.Items {
			paged = append(paged, it.Type)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if strings.Join(paged, ",") != want {
		t.Fatalf("paged = %s", strings.Join(paged, ","))
	}
	hs.do("GET", fmt.Sprintf("/contacts/%d/timeline?cursor=xyz", c.ID), "").wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "query.cursor")
	hs.do("GET", "/contacts/9999/timeline", "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
}

func TestEnrollments(t *testing.T) {
	hs := newHarness(t)
	c, _ := richContact(hs)
	var body api.ListBody[contacts.Enrollment]
	hs.doAs(readToken, "GET", fmt.Sprintf("/contacts/%d/enrollments", c.ID), "").wantStatus(t, http.StatusOK).decode(t, &body)
	if len(body.Items) != 1 {
		t.Fatalf("enrollments = %+v", body.Items)
	}
	e := body.Items[0]
	if e.SequenceName != "Welcome series" || e.Status != "active" || e.NextStepPosition == nil || *e.NextStepPosition != 2 || e.NextRunAt == nil {
		t.Fatalf("enrollment = %+v", e)
	}
	hs.do("GET", "/contacts/9999/enrollments", "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
}

func TestExport(t *testing.T) {
	hs := newHarness(t)
	c, msgs := richContact(hs)
	r := hs.doAs(readToken, "GET", fmt.Sprintf("/contacts/%d/export", c.ID), "").wantStatus(t, http.StatusOK)
	if !strings.Contains(r.header.Get("Content-Disposition"), fmt.Sprintf("contact-%d.json", c.ID)) {
		t.Fatalf("Content-Disposition = %q", r.header.Get("Content-Disposition"))
	}
	var ex contacts.Export
	r.decode(t, &ex)
	if ex.Contact.Email != "rich@example.com" || len(ex.Tags) != 1 || ex.Tags[0].Name != "vip" ||
		len(ex.ConsentLog) != 1 || len(ex.StatusHistory) != 1 || len(ex.Messages) != 3 ||
		len(ex.Clicks) != 1 || len(ex.Enrollments) != 1 || ex.Opens == nil {
		t.Fatalf("export = %+v", ex)
	}
	if ex.Messages[0].ID != msgs[0] || ex.Messages[0].ToEmail != "rich@example.com" || len(ex.Messages[0].Events) != 1 ||
		ex.Messages[1].Events[0].Type != "bounce" || string(ex.Messages[1].Events[0].Detail) != `{"raw":"x@example.com"}` {
		t.Fatalf("messages = %+v", ex.Messages)
	}
	if ex.StatusHistory[0].To != "unsubscribed" || ex.Clicks[0].URL != "https://example.com/a" {
		t.Fatalf("history/clicks = %+v %+v", ex.StatusHistory, ex.Clicks)
	}
	hs.do("GET", "/contacts/9999/export", "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
}

// TestExportCoversErase: the export holds everything Erase removes or
// anonymises (C-5): messages to the address that carry no contact (test
// sends, alerts), test sends rendered with the contact's data, SES events
// of the address not linked to a message, pending opt-ins with the
// submitted values, IP and user agent, and form submissions.
func TestExportCoversErase(t *testing.T) {
	hs := newHarness(t)
	hs.clock.Set(testutil.Epoch)
	c := hs.createContact(`{"email":"una@example.com","first_name":"Una"}`)
	ts := db.FormatTime(testutil.Epoch)
	own := hs.insertMessage(c.ID, c.Email, "broadcast", "sent", "News", testutil.Epoch)
	toAddr := hs.insertMessage(c.ID, c.Email, "test", "sent", "Test to Una", testutil.Epoch)
	testutil.Exec(t, hs.d, `UPDATE messages SET contact_id = NULL WHERE id = ?`, toAddr)
	asExample := hs.insertMessage(c.ID, "owner@example.com", "test", "sent", "Hi Una", testutil.Epoch)
	testutil.Exec(t, hs.d, `UPDATE messages SET contact_id = NULL, data = json_object('contact_id', ?) WHERE id = ?`, c.ID, asExample)
	testutil.Exec(t, hs.d, `INSERT INTO events (message_id, type, subtype, provider_event_id, detail, created_at)
		VALUES (NULL, 'bounce', 'transient', ?, '{"raw":1}', ?)`, "sns-9:"+core.EmailHash(c.Email), ts)
	testutil.Exec(t, hs.d, `INSERT INTO forms (id, public_id, name, consent_text, created_at, updated_at) VALUES (5, 'abcdefghij', 'F', 'Yes', ?1, ?1)`, ts)
	testutil.Exec(t, hs.d, `INSERT INTO pending_optins (form_id, contact_id, kind, payload, token_hash, ip, user_agent, consent_text, created_at, expires_at)
		VALUES (5, ?, 'resubscribe', '{"fields":{"first_name":"Una B"}}', 'h1', '198.51.100.7', 'Agent/1', 'Yes', ?2, ?2)`, c.ID, ts)
	testutil.Exec(t, hs.d, `INSERT INTO form_submissions (form_id, contact_id, outcome, created_at) VALUES (5, ?, 'resubscribe_pending', ?)`, c.ID, ts)

	var ex contacts.Export
	hs.do("GET", fmt.Sprintf("/contacts/%d/export", c.ID), "").wantStatus(t, http.StatusOK).decode(t, &ex)
	ids := map[int64]bool{}
	for _, m := range ex.Messages {
		ids[m.ID] = true
	}
	if len(ex.Messages) != 3 || !ids[own] || !ids[toAddr] || !ids[asExample] {
		t.Fatalf("messages = %+v, want %d, %d and %d", ex.Messages, own, toAddr, asExample)
	}
	if len(ex.OtherEvents) != 1 || ex.OtherEvents[0].Type != "bounce" {
		t.Fatalf("other events = %+v", ex.OtherEvents)
	}
	if len(ex.PendingOptins) != 1 || ex.PendingOptins[0].IP != "198.51.100.7" || ex.PendingOptins[0].UserAgent != "Agent/1" ||
		!strings.Contains(string(ex.PendingOptins[0].Submitted), "Una B") {
		t.Fatalf("pending opt-ins = %+v", ex.PendingOptins)
	}
	if len(ex.FormSubmissions) != 1 || ex.FormSubmissions[0].Outcome != "resubscribe_pending" {
		t.Fatalf("form submissions = %+v", ex.FormSubmissions)
	}

	// Erase counts exactly what the export listed.
	var res contacts.EraseResult
	hs.do("DELETE", fmt.Sprintf("/contacts/%d?dry_run=true", c.ID), "").wantStatus(t, http.StatusOK).decode(t, &res)
	if res.Anonymised.Messages != len(ex.Messages) || res.Anonymised.Events != len(ex.OtherEvents) ||
		res.Deleted.PendingOptins != len(ex.PendingOptins) || res.Anonymised.FormSubmissions != len(ex.FormSubmissions) {
		t.Fatalf("erase counts %+v differ from the export", res)
	}
}

func TestErase(t *testing.T) {
	hs := newHarness(t)
	c, _ := richContact(hs)
	other := hs.createContact(`{"email":"other@example.com","tags":["vip"]}`)
	hs.insertMessage(other.ID, other.Email, "broadcast", "sent", "April news", testutil.Epoch)
	path := fmt.Sprintf("/contacts/%d", c.ID)
	hash := core.EmailHash("rich@example.com")

	var dry contacts.EraseResult
	hs.do("DELETE", path+"?dry_run=true", "").wantStatus(t, http.StatusOK).decode(t, &dry)
	want := contacts.EraseResult{
		ContactID: c.ID, EmailHash: hash, DryRun: true,
		Deleted:    contacts.EraseDeleted{Contact: 1, Tags: 1, ConsentEvents: 1, StatusChanges: 1, Enrollments: 1},
		Anonymised: contacts.EraseAnonymous{Messages: 3, QueuedSkipped: 1, Events: 2, Clicks: 1},
	}
	if dry != want {
		t.Fatalf("dry run = %+v\nwant      %+v", dry, want)
	}
	if hs.count(`SELECT count(*) FROM contacts WHERE id = ?`, c.ID) != 1 || hs.count(`SELECT count(*) FROM suppressions`) != 0 ||
		hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'contact.erase'`) != 0 {
		t.Fatal("dry run changed data")
	}

	hs.do("DELETE", path, "", "If-Match", `"v1"`).wantError(t, http.StatusPreconditionFailed, api.CodePreconditionFailed, "")
	hs.doAs(readToken, "DELETE", path, "").wantError(t, http.StatusForbidden, api.CodeForbidden, "")

	var res contacts.EraseResult
	hs.do("DELETE", path, "").wantStatus(t, http.StatusOK).decode(t, &res)
	want.DryRun = false
	if res != want {
		t.Fatalf("erase = %+v", res)
	}
	checks := map[string]int{
		`SELECT count(*) FROM contacts WHERE email = 'rich@example.com'`:                                     0,
		`SELECT count(*) FROM consent_events WHERE contact_id = ` + fmt.Sprint(c.ID):                         0,
		`SELECT count(*) FROM contact_status_changes WHERE contact_id = ` + fmt.Sprint(c.ID):                 0,
		`SELECT count(*) FROM enrollments`:                                                                   0,
		`SELECT count(*) FROM contact_tags WHERE contact_id = ` + fmt.Sprint(c.ID):                           0,
		`SELECT count(*) FROM messages`:                                                                      4,
		`SELECT count(*) FROM messages WHERE to_email = 'rich@example.com'`:                                  0,
		`SELECT count(*) FROM messages WHERE contact_id IS NULL AND to_email IS NULL AND data = '{}'`:        3,
		`SELECT count(*) FROM messages WHERE status = 'skipped' AND error = 'contact erased'`:                1,
		`SELECT count(*) FROM events WHERE detail = '{}'`:                                                    2,
		`SELECT count(*) FROM clicks WHERE contact_id IS NULL`:                                               1,
		`SELECT count(*) FROM suppressions WHERE email_hash = '` + hash + `' AND reason = 'erased'`:          1,
		`SELECT count(*) FROM contacts WHERE email = 'other@example.com' AND version = 1`:                    1,
		`SELECT count(*) FROM messages WHERE to_email = 'other@example.com' AND data = '{"k":"v"}'`:          1,
		`SELECT count(*) FROM audit_log WHERE operation = 'contact.erase' AND changes LIKE '%` + hash + `%'`: 1,
		`SELECT count(*) FROM audit_log WHERE changes LIKE '%rich@example.com%'`:                             0,
	}
	for q, n := range checks {
		if got := hs.count(q); got != n {
			t.Errorf("%s = %d, want %d", q, got, n)
		}
	}
	// The address can't come back.
	e := hs.do("POST", "/contacts", `{"email":"Rich@example.com"}`).wantError(t, http.StatusConflict, api.CodeConflict, "email")
	if e.Message != "address is suppressed" {
		t.Fatalf("message = %q", e.Message)
	}
	hs.do("DELETE", path, "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
}

func TestEraseUpgradesManualSuppression(t *testing.T) {
	hs := newHarness(t)
	c := hs.createContact(`{"email":"m@example.com"}`)
	hs.do("POST", "/suppressions", `{"email":"m@example.com","note":"by phone"}`).wantStatus(t, http.StatusCreated)
	hs.do("DELETE", fmt.Sprintf("/contacts/%d", c.ID), "").wantStatus(t, http.StatusOK)
	if n := hs.count(`SELECT count(*) FROM suppressions WHERE reason = 'erased' AND note = ''`); n != 1 {
		t.Fatalf("erased rows = %d", n)
	}
	hs.do("POST", "/suppressions/remove", `{"email":"m@example.com"}`).wantError(t, http.StatusForbidden, api.CodeForbidden, "")
}

// personalData lists every table cell that contains one of needles
// (case-insensitive), as "table.column".
func personalData(t *testing.T, hs *harness, needles ...string) []string {
	t.Helper()
	rows, err := hs.d.Read.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	var found []string
	for _, table := range tables {
		r, err := hs.d.Read.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				var s string
				switch v := v.(type) {
				case string:
					s = v
				case []byte:
					s = string(v)
				default:
					continue
				}
				for _, n := range needles {
					if strings.Contains(strings.ToLower(s), strings.ToLower(n)) {
						found = append(found, table+"."+cols[i])
					}
				}
			}
		}
		r.Close()
	}
	return found
}

// TestEraseLeavesNoTrace erases a contact whose address and name are in a
// personalised subject, a provider error, a provider event ID and stored
// Idempotency-Key responses, and checks that neither survives anywhere.
func TestEraseLeavesNoTrace(t *testing.T) {
	hs := newHarness(t)
	hs.clock.Set(testutil.Epoch)
	var c contacts.Contact
	hs.do("POST", "/contacts", `{"email":"zeb@example.com","first_name":"Zebulon","attributes":{"city":"Quahog"}}`,
		api.IdempotencyHeader, "create-1").wantStatus(t, http.StatusCreated).decode(t, &c)
	hs.do("PATCH", fmt.Sprintf("/contacts/%d", c.ID), `{"last_name":"Xylander"}`).wantStatus(t, http.StatusOK)
	hs.do("POST", fmt.Sprintf("/contacts/%d/tags", c.ID), `{"tags":["vip"]}`, api.IdempotencyHeader, "tags-1").wantStatus(t, http.StatusOK)
	other := hs.createContact(`{"email":"keep@example.com"}`)
	hs.do("POST", "/contacts", `{"email":"keep2@example.com"}`, api.IdempotencyHeader, "create-2").wantStatus(t, http.StatusCreated)

	emailID := testutil.InsertEmail(t, hs.d, "Hi {{first_name}}", "b")
	m := hs.insertMessage(c.ID, c.Email, "broadcast", "bounced", "Hi Zebulon", testutil.Epoch)
	testutil.Exec(t, hs.d, `UPDATE messages SET email_id = ?, error = ? WHERE id = ?`,
		emailID, "Address blacklisted: Zeb@Example.com", m)
	sys := hs.insertMessage(c.ID, c.Email, "optin", "failed", "Confirm, Zebulon", testutil.Epoch)
	testutil.Exec(t, hs.d, `UPDATE messages SET error = ? WHERE id = ?`, "throttled", sys)
	ev := hs.insertEvent(m, "bounce", "permanent", testutil.Epoch)
	testutil.Exec(t, hs.d, `UPDATE events SET provider_event_id = 'sns-1:Zeb@Example.com' WHERE id = ?`, ev)
	otherMsg := hs.insertMessage(other.ID, other.Email, "broadcast", "sent", "Hi Keep", testutil.Epoch)
	otherEv := hs.insertEvent(otherMsg, "delivery", "", testutil.Epoch)
	testutil.Exec(t, hs.d, `UPDATE events SET provider_event_id = 'sns-2:keep@example.com' WHERE id = ?`, otherEv)
	// An SES event the events package could not link to a message is keyed
	// by the recipient's hash and may hold the raw notification.
	testutil.Exec(t, hs.d, `INSERT INTO events (message_id, type, subtype, provider_event_id, detail, created_at)
		VALUES (NULL, 'bounce', 'transient', ?, '{"mail":{"destination":["zeb@example.com"]}}', ?)`,
		"sns-3:"+core.EmailHash("zeb@example.com"), db.FormatTime(testutil.Epoch))
	// Finished jobs that mention the address lose their payloads; running
	// ones are left alone.
	testutil.Exec(t, hs.d, `INSERT INTO jobs (type, status, params, result, created_at) VALUES
		('probe', 'succeeded', '{"to":"zeb@example.com"}', '{}', ?1),
		('probe', 'running', '{"id":1}', '{}', ?1)`, db.FormatTime(testutil.Epoch))

	if found := personalData(t, hs, "zeb@example.com", "Zebulon", "Xylander", "Quahog"); len(found) == 0 {
		t.Fatal("fixture holds no personal data")
	}
	var dry contacts.EraseResult
	hs.do("DELETE", fmt.Sprintf("/contacts/%d?dry_run=true", c.ID), "").wantStatus(t, http.StatusOK).decode(t, &dry)
	if dry.Deleted.StoredResponses != 2 || hs.count(`SELECT count(*) FROM idempotency_keys`) != 3 {
		t.Fatalf("dry run stored responses = %d", dry.Deleted.StoredResponses)
	}
	var res contacts.EraseResult
	hs.do("DELETE", fmt.Sprintf("/contacts/%d", c.ID), "").wantStatus(t, http.StatusOK).decode(t, &res)
	if res.Deleted.StoredResponses != 2 || res.Anonymised.Messages != 2 || res.Anonymised.Events != 2 || res.Anonymised.JobPayloads != 1 {
		t.Fatalf("erase = %+v", res)
	}
	if found := personalData(t, hs, "zeb@example.com", "Zebulon", "Xylander", "Quahog"); len(found) != 0 {
		t.Fatalf("personal data left in %v", found)
	}

	hash := core.EmailHash("zeb@example.com")
	checks := map[string]int{
		`SELECT count(*) FROM messages WHERE id = ` + fmt.Sprint(m) + ` AND subject = 'Hi {{first_name}}' AND error = 'redacted: contact erased'`: 1,
		`SELECT count(*) FROM messages WHERE id = ` + fmt.Sprint(sys) + ` AND subject = '' AND error = 'redacted: contact erased'`:                1,
		`SELECT count(*) FROM events WHERE provider_event_id = 'sns-1:` + hash + `'`:                                                              1,
		`SELECT count(*) FROM events WHERE provider_event_id = 'sns-2:keep@example.com'`:                                                          1,
		`SELECT count(*) FROM messages WHERE id = ` + fmt.Sprint(otherMsg) + ` AND subject = 'Hi Keep'`:                                           1,
		`SELECT count(*) FROM idempotency_keys WHERE key = 'create-2' AND status_code = 201`:                                                      1,
		`SELECT count(*) FROM idempotency_keys WHERE key IN ('create-1', 'tags-1') AND status_code = 0 AND length(response_body) = 0`:             2,
		`SELECT count(*) FROM events WHERE provider_event_id = 'sns-3:` + hash + `' AND detail = '{}'`:                                            1,
		`SELECT count(*) FROM jobs WHERE status = 'succeeded' AND params = '{}' AND result = '{}'`:                                                1,
		`SELECT count(*) FROM jobs WHERE status = 'running' AND params = '{"id":1}'`:                                                              1,
	}
	for q, n := range checks {
		if got := hs.count(q); got != n {
			t.Errorf("%s = %d, want %d", q, got, n)
		}
	}
	// Replaying the create key no longer returns the erased record, and
	// the request isn't run again either: the key answers 409.
	hs.do("POST", "/contacts", `{"email":"zeb@example.com","first_name":"Zebulon","attributes":{"city":"Quahog"}}`,
		api.IdempotencyHeader, "create-1").wantError(t, http.StatusConflict, api.CodeConflict, "header."+api.IdempotencyHeader)
}
