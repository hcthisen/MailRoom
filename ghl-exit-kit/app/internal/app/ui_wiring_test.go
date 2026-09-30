package app_test

import (
	"net/http"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// TestAdminUIMounted: the admin UI is served under /admin/ (/ redirects
// there) next to the API, and the dashboard numbers it shows are in the
// API too (G-1).
func TestAdminUIMounted(t *testing.T) {
	hs := newHarness(t, nil)
	if r := hs.raw(http.MethodGet, "/", "", ""); r.code != http.StatusFound || r.header.Get("Location") != "/admin/" {
		t.Fatalf("GET / = %d %v", r.code, r.header)
	}
	if r := hs.raw(http.MethodGet, "/admin/", "", ""); r.code != http.StatusSeeOther || r.header.Get("Location") != "/admin/login" {
		t.Fatalf("GET /admin/ logged out = %d %v", r.code, r.header)
	}
	hs.raw(http.MethodGet, "/admin/login", "", "").want(t, http.StatusOK)
	// Public endpoints are unaffected.
	hs.raw(http.MethodGet, "/healthz", "", "").want(t, http.StatusOK)
	hs.raw(http.MethodGet, "/api/v1/nope", "", "").want(t, http.StatusUnauthorized)

	testutil.InsertContact(t, hs.app.DB, "a@example.org", core.StatusActive)
	var d struct {
		ContactsTotal  int64 `json:"contacts_total"`
		ContactsActive int64 `json:"contacts_active"`
		NewContacts7d  int64 `json:"new_contacts_7d"`
		Timezone       string
	}
	hs.api(http.MethodGet, "/stats/dashboard", "").want(t, http.StatusOK).decode(t, &d)
	if d.ContactsTotal != 1 || d.ContactsActive != 1 || d.NewContacts7d != 1 {
		t.Fatalf("dashboard = %+v", d)
	}
}
