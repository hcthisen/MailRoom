package contacts_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

func TestSuppressions(t *testing.T) {
	hs := newHarness(t)
	active := hs.createContact(`{"email":"active@example.com"}`)
	bounced := testutil.InsertContact(t, hs.d, "bounced@example.com", core.StatusBounced)

	// An active contact becomes suppressed_manual.
	var add contacts.SuppressResult
	hs.do("POST", "/suppressions", `{"email":" Active@Example.com ","note":"asked by phone"}`).wantStatus(t, http.StatusCreated).decode(t, &add)
	hash := core.EmailHash("active@example.com")
	if !add.Created || !add.StatusChanged || add.ContactID != active.ID || add.Suppression.EmailHash != hash ||
		add.Suppression.Reason != "manual" || add.Suppression.Note != "asked by phone" {
		t.Fatalf("add = %+v", add)
	}
	c, _ := hs.svc.Get(t.Context(), active.ID)
	if c.Status != core.StatusSuppressedManual {
		t.Fatalf("status = %s", c.Status)
	}
	// Again: 200, nothing changes.
	hs.do("POST", "/suppressions", `{"email":"active@example.com"}`).wantStatus(t, http.StatusOK).decode(t, &add)
	if add.Created || add.StatusChanged {
		t.Fatalf("repeat = %+v", add)
	}
	// Other statuses are kept; only the hash row is added.
	hs.do("POST", "/suppressions", `{"email":"bounced@example.com"}`).wantStatus(t, http.StatusCreated).decode(t, &add)
	if add.StatusChanged || add.ContactID != bounced.ID {
		t.Fatalf("bounced = %+v", add)
	}
	// No contact at all: the address can't be created afterwards.
	hs.do("POST", "/suppressions", `{"email":"nobody@example.com"}`).wantStatus(t, http.StatusCreated)
	hs.do("POST", "/contacts", `{"email":"nobody@example.com"}`).wantError(t, http.StatusConflict, api.CodeConflict, "email")
	testutil.InsertSuppression(t, hs.d, "erased@example.com", "erased")

	var list api.ListBody[contacts.Suppression]
	hs.doAs(readToken, "GET", "/suppressions", "").wantStatus(t, http.StatusOK).decode(t, &list)
	if len(list.Items) != 4 || list.Items[0].Reason != "erased" {
		t.Fatalf("list = %+v", list.Items)
	}
	hs.do("GET", "/suppressions?reason=manual&limit=2", "").decode(t, &list)
	if len(list.Items) != 2 || list.NextCursor == "" {
		t.Fatalf("page = %+v", list)
	}
	hs.do("GET", "/suppressions?reason=manual&limit=2&cursor="+list.NextCursor, "").decode(t, &list)
	if len(list.Items) != 1 || list.NextCursor != "" {
		t.Fatalf("page 2 = %+v", list)
	}
	hs.do("GET", "/suppressions?email_hash="+hash, "").decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].EmailHash != hash {
		t.Fatalf("by hash = %+v", list.Items)
	}
	hs.do("GET", "/suppressions?email_hash=zz", "").wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "query.email_hash")

	// Remove: the contact is reactivated with the note.
	var rm contacts.UnsuppressResult
	hs.do("POST", "/suppressions/remove", `{"email":"active@example.com","note":"wants mail again"}`).wantStatus(t, http.StatusOK).decode(t, &rm)
	if !rm.StatusChanged || rm.ContactID != active.ID || rm.EmailHash != hash {
		t.Fatalf("remove = %+v", rm)
	}
	c, _ = hs.svc.Get(t.Context(), active.ID)
	if c.Status != core.StatusActive {
		t.Fatalf("status after remove = %s", c.Status)
	}
	if n := hs.count(`SELECT count(*) FROM contact_status_changes WHERE contact_id = ? AND to_status = 'active' AND note = 'wants mail again' AND source = 'api'`, active.ID); n != 1 {
		t.Fatal("reactivation not recorded")
	}
	// Bounced stays bounced when its hash row is removed.
	hs.do("POST", "/suppressions/remove", `{"email":"bounced@example.com"}`).wantStatus(t, http.StatusOK).decode(t, &rm)
	if rm.StatusChanged {
		t.Fatal("bounced contact must not be reactivated")
	}
	hs.do("POST", "/suppressions/remove", `{"email":"active@example.com"}`).wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	hs.do("POST", "/suppressions/remove", `{"email":"erased@example.com"}`).wantError(t, http.StatusForbidden, api.CodeForbidden, "")
	hs.do("POST", "/suppressions", `{"email":"bad"}`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "email")
	hs.doAs(readToken, "POST", "/suppressions", `{"email":"r@example.com"}`).wantError(t, http.StatusForbidden, api.CodeForbidden, "")

	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation LIKE 'suppression.%' AND changes LIKE '%@%'`); n != 0 {
		t.Fatal("suppression audit entries must not hold addresses")
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'suppression.add' AND target_id = ?`, hash); n != 1 {
		t.Fatalf("suppression.add audit rows = %d", n)
	}
}

func TestIdempotentCreate(t *testing.T) {
	hs := newHarness(t)
	body := `{"email":"idem@example.com"}`
	first := hs.do("POST", "/contacts", body, api.IdempotencyHeader, "key-1").wantStatus(t, http.StatusCreated)
	second := hs.do("POST", "/contacts", body, api.IdempotencyHeader, "key-1").wantStatus(t, http.StatusCreated)
	if string(first.body) != string(second.body) || second.header.Get(api.IdempotencyReplayHeader) == "" {
		t.Fatalf("replay = %s / %s", first.body, second.body)
	}
	if n := hs.count(`SELECT count(*) FROM contacts`); n != 1 {
		t.Fatalf("contacts = %d", n)
	}
}

// TestStatusFollowsSuppression checks that set-contact-status and
// create-contact keep the suppressions table in step with the
// suppressed_manual status, in both directions.
func TestStatusFollowsSuppression(t *testing.T) {
	hs := newHarness(t)
	manualRows := func(email string) int {
		return hs.count(`SELECT count(*) FROM suppressions WHERE email_hash = ? AND reason = 'manual'`, core.EmailHash(email))
	}
	setStatus := func(id int64, body string) resp {
		return hs.do("POST", fmt.Sprintf("/contacts/%d/status", id), body)
	}

	// Suppressed through add-suppression, reactivated through the status
	// endpoint: the manual row goes too, so the sender won't skip it.
	a := hs.createContact(`{"email":"a@example.com"}`)
	hs.do("POST", "/suppressions", `{"email":"a@example.com"}`).wantStatus(t, http.StatusCreated)
	setStatus(a.ID, `{"status":"active"}`).wantStatus(t, http.StatusOK)
	if manualRows("a@example.com") != 0 {
		t.Fatal("reactivating suppressed_manual must remove the manual suppression")
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'contact.status' AND target_id = ? AND changes LIKE '%suppression_removed%'`, fmt.Sprint(a.ID)); n != 1 {
		t.Fatal("removal not in the audit entry")
	}

	// Suppressed through the status endpoint: the hash row is written, so
	// remove-suppression finds the address.
	setStatus(a.ID, `{"status":"suppressed_manual","note":"by phone"}`).wantStatus(t, http.StatusOK)
	if n := hs.count(`SELECT count(*) FROM suppressions WHERE email_hash = ? AND reason = 'manual' AND note = 'by phone'`, core.EmailHash("a@example.com")); n != 1 {
		t.Fatal("suppressed_manual must add the manual suppression")
	}
	var rm contacts.UnsuppressResult
	hs.do("POST", "/suppressions/remove", `{"email":"a@example.com"}`).wantStatus(t, http.StatusOK).decode(t, &rm)
	if !rm.StatusChanged || manualRows("a@example.com") != 0 {
		t.Fatalf("remove = %+v", rm)
	}

	// An unsubscribed contact whose address is suppressed can't be
	// reactivated behind the suppression's back.
	u := hs.createContact(`{"email":"u@example.com","status":"unsubscribed"}`)
	hs.do("POST", "/suppressions", `{"email":"u@example.com"}`).wantStatus(t, http.StatusCreated)
	e := setStatus(u.ID, `{"status":"active","note":"asked"}`).wantError(t, http.StatusConflict, api.CodeConflict, "")
	if e.Message == "" || manualRows("u@example.com") != 1 {
		t.Fatalf("conflict = %+v", e)
	}
	if c, _ := hs.svc.Get(t.Context(), u.ID); c.Status != core.StatusUnsubscribed {
		t.Fatalf("status = %s", c.Status)
	}
	// Recording a bounce on a suppressed_manual contact keeps the row.
	b := hs.createContact(`{"email":"b@example.com","status":"suppressed_manual"}`)
	if manualRows("b@example.com") != 1 {
		t.Fatal("create with suppressed_manual must add the manual suppression")
	}
	setStatus(b.ID, `{"status":"bounced"}`).wantStatus(t, http.StatusOK)
	if manualRows("b@example.com") != 1 {
		t.Fatal("a bounce must not remove the suppression")
	}

	// A suppressed_manual contact without a hash row (e.g. imported) can
	// still be unsuppressed; an active one without a row is not suppressed.
	legacy := testutil.InsertContact(t, hs.d, "legacy@example.com", core.StatusSuppressedManual)
	hs.do("POST", "/suppressions/remove", `{"email":"legacy@example.com","note":"ok"}`).wantStatus(t, http.StatusOK).decode(t, &rm)
	if !rm.StatusChanged || rm.ContactID != legacy.ID {
		t.Fatalf("legacy remove = %+v", rm)
	}
	hs.do("POST", "/suppressions/remove", `{"email":"legacy@example.com"}`).wantError(t, http.StatusNotFound, api.CodeNotFound, "")
}

// TestReactivationIsLoggedAsConsent: moving a contact to active from
// pending, unsubscribed or suppressed_manual (status endpoint or
// remove-suppression) appends an api consent event with the note (C-3);
// a bounced contact's reactivation doesn't, and one that asked to
// unsubscribe while suppressed lands on unsubscribed without consent.
func TestReactivationIsLoggedAsConsent(t *testing.T) {
	hs := newHarness(t)
	consent := func(id int64) []string {
		t.Helper()
		rows, err := hs.d.Read.Query(`SELECT type || '|' || source || '|' || consent_text FROM consent_events WHERE contact_id = ? ORDER BY id`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	status := func(id int64, body string) contacts.Contact {
		t.Helper()
		var out struct {
			Contact contacts.Contact `json:"contact"`
		}
		hs.do("POST", fmt.Sprintf("/contacts/%d/status", id), body).wantStatus(t, http.StatusOK).decode(t, &out)
		return out.Contact
	}
	last := func(id int64) string {
		t.Helper()
		all := consent(id)
		if len(all) == 0 {
			return ""
		}
		return all[len(all)-1]
	}

	for _, from := range []string{"pending", "unsubscribed"} {
		c := hs.createContact(fmt.Sprintf(`{"email":"%s@example.com","status":%q}`, from, from))
		before := len(consent(c.ID))
		if got := status(c.ID, `{"status":"active","note":"confirmed by phone"}`); got.Status != core.StatusActive {
			t.Fatalf("%s: status = %s", from, got.Status)
		}
		if n := len(consent(c.ID)); n != before+1 || !strings.HasPrefix(last(c.ID), "api|api:") ||
			!strings.HasSuffix(last(c.ID), ":status:"+from+"|confirmed by phone") {
			t.Fatalf("%s: consent log = %q", from, consent(c.ID))
		}
	}

	b := hs.createContact(`{"email":"bounced@example.com","status":"bounced"}`)
	before := len(consent(b.ID))
	status(b.ID, `{"status":"active","note":"mailbox fixed"}`)
	if n := len(consent(b.ID)); n != before {
		t.Fatalf("un-bounce wrote consent: %q", consent(b.ID))
	}

	m := hs.createContact(`{"email":"m@example.com"}`)
	hs.do("POST", "/suppressions", `{"email":"m@example.com"}`).wantStatus(t, http.StatusCreated)
	before = len(consent(m.ID))
	var rm contacts.UnsuppressResult
	hs.do("POST", "/suppressions/remove", `{"email":"m@example.com","note":"asked again"}`).wantStatus(t, http.StatusOK).decode(t, &rm)
	if rm.Status != core.StatusActive || len(consent(m.ID)) != before+1 || !strings.HasSuffix(last(m.ID), "|asked again") {
		t.Fatalf("remove = %+v, consent %q", rm, consent(m.ID))
	}

	// Asked to unsubscribe while suppressed: lifting lands on unsubscribed.
	o := hs.createContact(`{"email":"o@example.com"}`)
	hs.do("POST", "/suppressions", `{"email":"o@example.com"}`).wantStatus(t, http.StatusCreated)
	if err := hs.d.WithTx(t.Context(), func(tx *sql.Tx) error {
		_, err := hs.store.RecordOptOut(t.Context(), tx, o.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before = len(consent(o.ID))
	hs.do("POST", "/suppressions/remove", `{"email":"o@example.com"}`).wantStatus(t, http.StatusOK).decode(t, &rm)
	if !rm.StatusChanged || rm.Status != core.StatusUnsubscribed || len(consent(o.ID)) != before {
		t.Fatalf("remove after opt-out = %+v, consent %q", rm, consent(o.ID))
	}
	if n := hs.count(`SELECT count(*) FROM suppressions WHERE email_hash = ?`, core.EmailHash("o@example.com")); n != 0 {
		t.Fatal("the manual suppression stayed")
	}
}
