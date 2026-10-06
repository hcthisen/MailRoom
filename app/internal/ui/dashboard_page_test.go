package ui_test

import (
	"net/http"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

func TestDashboard(t *testing.T) {
	hs := newHarness(t)
	hs.anon().get("/admin/").wantLogin(t)

	testutil.InsertContact(t, hs.app.DB, "a@example.org", core.StatusActive)
	testutil.InsertContact(t, hs.app.DB, "b@example.org", core.StatusActive)
	testutil.InsertContact(t, hs.app.DB, "c@example.org", core.StatusPending)
	hs.app.Stats.Invalidate()

	c := hs.session()
	r := c.get("/admin/").want(t, http.StatusOK).contains(t,
		"<h1>Dashboard</h1>", `aria-current="page">Dashboard`,
		"New contacts, 7 days", `<p class="value">3</p>`, "of 3 in total",
		"Emails sent this month", "Bounce rate, 30 days", "Complaint rate, 30 days",
		"SES is not configured yet", // offline: no AWS
		"The install checks have not run yet")
	r.lacks(t, "Sending is paused")

	// After a doctor run (the worker's job) its warnings are listed.
	rep := hs.app.UI.RefreshDoctor(hs.ctx)
	if rep.Status == "ok" {
		t.Fatalf("offline doctor = %+v, want warnings", rep)
	}
	c.get("/admin/").want(t, http.StatusOK).contains(t, `<ul class="checks">`, "ses_identity", "Checked just now").
		lacks(t, "have not run yet")

	// A paused sender is shown.
	if _, err := hs.app.Sending.Pause(hs.ctx, "list check", false); err != nil {
		t.Fatal(err)
	}
	c.get("/admin/").want(t, http.StatusOK).contains(t, "Sending is paused", "list check")
}
