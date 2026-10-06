package forms_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"mailroom/internal/api"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/forms"
	"mailroom/internal/testutil"
)

// wideForm creates a form with n attribute fields attr.a1..attr.an.
func wideForm(h *harness, n int, extra string) forms.Form {
	h.t.Helper()
	fields := make([]string, n)
	for i := range fields {
		fields[i] = fmt.Sprintf(`{"key":"attr.a%d"}`, i+1)
	}
	return h.createForm(`{"name":"Wide","fields":[` + strings.Join(fields, ",") + `],"tags":["wide"]` + extra + `}`)
}

// oversizedValues fills every field of wideForm(n) with MaxValueLength
// '<' characters: each value passes on its own, but '<' encodes as
// < in JSON, so together they exceed contactstore.MaxAttributesBytes.
func oversizedValues(n int, email string) map[string]string {
	v := map[string]string{"email": email}
	for i := range n {
		v[fmt.Sprintf("attr.a%d", i+1)] = strings.Repeat("<", forms.MaxValueLength)
	}
	return v
}

func TestValidateSubmissionCombinedAttributes(t *testing.T) {
	h := newHarness(t)
	f := wideForm(h, 8, "")
	errs := forms.ValidateSubmission(f, oversizedValues(8, "a@example.com"))
	if len(errs) != 1 || errs[0].Field != "attr.a8" || !strings.Contains(errs[0].Message, "too long together") {
		t.Fatalf("errs = %+v", errs)
	}
	// Two such values fit.
	if errs := forms.ValidateSubmission(f, oversizedValues(2, "a@example.com")); len(errs) != 0 {
		t.Fatalf("errs = %+v", errs)
	}
}

// TestOversizedAttributesIndistinguishable: a submission whose attributes
// only fail together is refused with the same answer for every address
// state before anything is written (F-7), rather than failing in the
// store for new and active addresses only.
func TestOversizedAttributesIndistinguishable(t *testing.T) {
	for _, double := range []bool{false, true} {
		t.Run(fmt.Sprintf("double_optin=%v", double), func(t *testing.T) {
			h := newHarness(t)
			f := wideForm(h, 8, fmt.Sprintf(`,"double_optin":%v`, double))
			testutil.InsertContact(t, h.d, "active@example.com", core.StatusActive)
			testutil.InsertContact(t, h.d, "unsub@example.com", core.StatusUnsubscribed)
			testutil.InsertContact(t, h.d, "pending@example.com", core.StatusPending)
			testutil.InsertContact(t, h.d, "bounced@example.com", core.StatusBounced)
			testutil.InsertContact(t, h.d, "complained@example.com", core.StatusComplained)
			testutil.InsertSuppression(t, h.d, "erased@example.com", "erased")
			contacts := h.count(`SELECT count(*) FROM contacts`)
			var jsonRef *resp
			for _, e := range []string{"new@example.com", "active@example.com", "unsub@example.com", "pending@example.com",
				"bounced@example.com", "complained@example.com", "erased@example.com"} {
				values := oversizedValues(8, e)
				form := vals()
				for k, v := range values {
					form.Set(k, v)
				}
				h.submit(f.PublicID, form).wantStatus(t, http.StatusUnprocessableEntity)
				body, err := json.Marshal(values)
				if err != nil {
					t.Fatal(err)
				}
				j := h.submitJSON(f.PublicID, string(body))
				j.wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "attr.a8")
				if jsonRef == nil {
					jsonRef = &j
				} else if !bytes.Equal(j.body, jsonRef.body) {
					t.Errorf("%s: JSON response differs: %s", e, j.body)
				}
			}
			if n := h.count(`SELECT count(*) FROM contacts`); n != contacts {
				t.Fatalf("contacts = %d, want %d", n, contacts)
			}
			for _, q := range []string{`SELECT count(*) FROM form_submissions`, `SELECT count(*) FROM pending_optins`, `SELECT count(*) FROM messages`} {
				if n := h.count(q); n != 0 {
					t.Fatalf("%s = %d", q, n)
				}
			}
		})
	}
}

// bigAttributes gives a contact attributes just below the size limit.
func bigAttributes(h *harness, contactID int64) string {
	h.t.Helper()
	notes := strings.Repeat("x", contactstore.MaxAttributesBytes-100)
	testutil.Exec(h.t, h.d, `UPDATE contacts SET attributes = json_object('notes', ?) WHERE id = ?`, notes, contactID)
	return notes
}

// TestSubmitAttributesDontFitExisting: attributes that fit on their own
// but not next to an existing contact's are skipped, and the sign-up
// still succeeds with the normal answer.
func TestSubmitAttributesDontFitExisting(t *testing.T) {
	h := newHarness(t)
	f, seq := signupForm(h, "")
	c := testutil.InsertContact(t, h.d, "full@example.com", core.StatusActive)
	notes := bigAttributes(h, c.ID)
	company := strings.Repeat("A", 200)
	h.submit(f.PublicID, vals("email", "full@example.com", "first_name", "Zed", "attr.company", company)).
		wantStatus(t, http.StatusSeeOther)
	got := h.contact("full@example.com")
	if got.FirstName != "Zed" || got.Attributes["notes"] != notes || got.Attributes["company"] != nil || strings.Join(got.Tags, ",") != "community" {
		t.Fatalf("contact = %+v", got)
	}
	if o := h.lastOutcome(f.ID); o != forms.OutcomeUpdated {
		t.Fatalf("outcome = %s", o)
	}
	if calls := h.enroller.Calls(); len(calls) != 1 || calls[0] != (enrollCall{seq, c.ID, "form:" + f.PublicID}) {
		t.Fatalf("enroll = %+v", calls)
	}
}

// TestConfirmAttributesDontFitExisting: a pending sign-up whose stored
// fields no longer fit the contact's attributes still confirms.
func TestConfirmAttributesDontFitExisting(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	c := testutil.InsertContact(t, h.d, "full@example.com", core.StatusPending)
	notes := bigAttributes(h, c.ID)
	h.submit(f.PublicID, vals("email", "full@example.com", "first_name", "Zed", "attr.company", strings.Repeat("A", 200))).
		wantStatus(t, http.StatusSeeOther)
	r := h.publicReq("POST", h.confirmPath(c.ID), "", "").wantStatus(t, http.StatusOK)
	if !strings.Contains(r.plain(), "You're subscribed") {
		t.Fatalf("page = %s", r.body)
	}
	got := h.contact("full@example.com")
	if got.Status != core.StatusActive || got.FirstName != "Zed" || got.Attributes["notes"] != notes || got.Attributes["company"] != nil {
		t.Fatalf("contact = %+v", got)
	}
}

// TestUserAgentBounded: a huge User-Agent header is stored truncated to
// valid UTF-8 in pending opt-ins and consent events.
func TestUserAgentBounded(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	ua := "x" + strings.Repeat("é", 3000) // odd offset: truncation lands mid-rune
	h.submit(f.PublicID, vals("email", "ua@example.com"), "User-Agent", ua).wantStatus(t, http.StatusSeeOther)
	c := h.contact("ua@example.com")
	var stored string
	if err := h.d.Read.QueryRow(`SELECT user_agent FROM pending_optins WHERE contact_id = ?`, c.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) > forms.MaxUserAgentLength || len(stored) < forms.MaxUserAgentLength-4 || !utf8.ValidString(stored) || !strings.HasPrefix(ua, stored) {
		t.Fatalf("stored user agent: %d bytes, valid=%v", len(stored), utf8.ValidString(stored))
	}
	cs := h.consents(c.ID)
	if len(cs) != 1 || cs[0].UserAgent != stored {
		t.Fatalf("consent user agent = %d bytes", len(cs[0].UserAgent))
	}
	// A refresh stores the bounded value too.
	h.submit(f.PublicID, vals("email", "ua@example.com"), "User-Agent", ua+ua).wantStatus(t, http.StatusSeeOther)
	if n := h.count(`SELECT max(length(CAST(user_agent AS BLOB))) FROM pending_optins`); n > forms.MaxUserAgentLength {
		t.Fatalf("refreshed user agent = %d bytes", n)
	}
}
