package ui_test

import (
	"net/http"
	"net/url"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

func TestAudienceSuppressions(t *testing.T) {
	hs := audHarness(t)
	c := hs.session()
	ct := testutil.InsertContact(t, hs.app.DB, "sup@example.org", core.StatusActive)

	c.get("/admin/suppressions").want(t, http.StatusOK).contains(t, "<h1>Suppressions</h1>", "No suppressions")

	// Add.
	c.postNoCSRF("/admin/suppressions", url.Values{"email": {"sup@example.org"}}).want(t, http.StatusForbidden)
	c.post("/admin/suppressions", url.Values{"email": {"bad"}}).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-email-error"`)
	c.post("/admin/suppressions", url.Values{"email": {"Sup@Example.org"}, "note": {"asked by phone"}}).wantRedirect(t, "/admin/suppressions")
	hash := core.EmailHash("sup@example.org")
	c.get("/admin/suppressions").want(t, http.StatusOK).
		contains(t, "its contact is set to suppressed", hash[:16], "asked by phone").lacks(t, "sup@example.org")
	if got, _ := hs.app.Contacts.Get(hs.ctx, ct.ID); got.Status != core.StatusSuppressedManual {
		t.Fatalf("status = %s", got.Status)
	}

	// Look up one address by its hash.
	c.get("/admin/suppressions?email=SUP@example.org").want(t, http.StatusOK).contains(t, hash[:16])
	c.get("/admin/suppressions?email=other@example.org").want(t, http.StatusOK).contains(t, "is not on the suppression list")
	c.get("/admin/suppressions?email=nope").want(t, http.StatusUnprocessableEntity).contains(t, "is not a valid email address")
	c.get("/admin/suppressions?reason=erased").want(t, http.StatusOK).lacks(t, hash[:16])

	// Remove: confirmation, then the removal.
	c.postNoCSRF("/admin/suppressions/remove", url.Values{"remove_email": {"sup@example.org"}}).want(t, http.StatusForbidden)
	c.post("/admin/suppressions/remove", url.Values{"remove_email": {"x"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-remove_email-error"`)
	c.post("/admin/suppressions/remove", url.Values{"remove_email": {"sup@example.org"}, "remove_note": {"called back"}}).
		want(t, http.StatusOK).contains(t, "Remove the suppression for sup@example.org?", `name="confirm" value="yes"`)
	if n := hs.count(`SELECT COUNT(*) FROM suppressions`); n != 1 {
		t.Fatal("removed before the confirmation")
	}
	c.post("/admin/suppressions/remove", url.Values{"remove_email": {"sup@example.org"}, "remove_note": {"called back"}, "confirm": {"yes"}}).
		wantRedirect(t, "/admin/suppressions")
	if n := hs.count(`SELECT COUNT(*) FROM suppressions`); n != 0 {
		t.Fatal("not removed")
	}
	c.get("/admin/suppressions").want(t, http.StatusOK).contains(t, "The suppression was removed. Its contact is now Active.")
	// Not suppressed: shown on the form.
	c.post("/admin/suppressions/remove", url.Values{"remove_email": {"sup@example.org"}, "confirm": {"yes"}}).
		want(t, http.StatusNotFound).contains(t, "this address is not suppressed")
}
