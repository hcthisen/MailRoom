package ui_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"
)

// admStartJobs runs the jobs runner in the background until the test ends.
func admStartJobs(t *testing.T, hs *harness) {
	t.Helper()
	ctx, cancel := context.WithCancel(hs.ctx)
	done := make(chan error, 1)
	go func() { done <- hs.app.Jobs.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("jobs runner did not stop")
		}
	})
}

// TestAdminPagesNeedSession checks every admin page: 200 signed in, the
// login page when logged out, and 403 for a POST without the CSRF token.
func TestAdminPagesNeedSession(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	users, err := hs.app.Auth.ListUsers(hs.ctx, 0, 10)
	if err != nil || len(users) != 1 {
		t.Fatalf("users = %v, %v", users, err)
	}
	job, err := hs.app.System.RequestBackup(hs.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	userURL := "/admin/users/" + admItoa(users[0].ID)
	jobURL := "/admin/health/jobs/" + admItoa(job.ID)
	pages := map[string]string{
		"/admin/settings":       "<h1>Settings</h1>",
		"/admin/users":          "<h1>Users</h1>",
		userURL:                 "Two-factor sign-in",
		"/admin/health":         "<h1>Health</h1>",
		"/admin/health/backups": "<h1>Backups</h1>",
		"/admin/health/jobs":    "<h1>Jobs</h1>",
		jobURL:                  "<h1>Job " + admItoa(job.ID) + "</h1>",
		"/admin/health/audit":   "<h1>Audit log</h1>",
		"/admin/tokens":         "<h1>API tokens</h1>",
	}
	for path, want := range pages {
		hs.anon().get(path).wantLogin(t)
		c.get(path).want(t, http.StatusOK).contains(t, want)
	}
	c.get("/admin/settings").contains(t, `aria-current="page">Settings`)
	c.get("/admin/tokens").contains(t, `aria-current="page">API tokens`)
	c.get("/admin/health/jobs").contains(t, `aria-current="page">Health`)

	posts := []string{
		"/admin/settings", "/admin/users", userURL, userURL + "/delete", userURL + "/totp", userURL + "/totp/confirm",
		userURL + "/totp/reset", "/admin/health/doctor", "/admin/health/sending/pause", "/admin/health/sending/resume",
		"/admin/health/backups", "/admin/tokens", "/admin/tokens/1/revoke",
	}
	for _, path := range posts {
		c.postNoCSRF(path, url.Values{"confirm": {"yes"}}).want(t, http.StatusForbidden)
		hs.anon().postRaw(path, nil).wantLogin(t)
	}
	if st, _ := hs.app.Sending.GetState(hs.ctx, nil); st.Paused {
		t.Fatal("a refused POST paused sending")
	}
	c.get("/admin/users/abc").want(t, http.StatusNotFound)
	c.get("/admin/health/jobs/999").want(t, http.StatusNotFound)
}

func admItoa(n int64) string { return strconv.FormatInt(n, 10) }

// admSettingsValues is a complete, valid settings form.
func admSettingsValues(version string) url.Values {
	return url.Values{
		"version":                          {version},
		"sender.from_name":                 {"Example Letters"},
		"sender.from_email":                {"letters@example.com"},
		"sender.reply_to":                  {"reply@example.com"},
		"company_footer":                   {"Example Ltd\r\n1 Street, Town"},
		"owner_email":                      {"owner@example.org"},
		"alert_email":                      {"alerts@example.org"},
		"timezone":                         {"Europe/Copenhagen"},
		"tracking.clicks":                  {"1"},
		"sending.rate_override_per_sec":    {"0"},
		"sending.rate_fraction":            {"0,5"},
		"circuit_breaker.min_messages":     {"150"},
		"circuit_breaker.hard_bounce_rate": {"0.04"},
		"circuit_breaker.complaint_rate":   {"0.002"},
		"default_send_window.enabled":      {"1"},
		"default_send_window.days":         {"3", "1", "2"},
		"default_send_window.start":        {"09:00"},
		"default_send_window.end":          {"17:30"},
	}
}

func TestAdminSettingsSave(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	page := c.get("/admin/settings").want(t, http.StatusOK).contains(t,
		"example.com", `name="sender.from_email"`, `value="news@example.com"`, `name="tracking.clicks" value="1" checked`,
		`name="default_send_window.days" value="1" checked`)
	version := page.field(t, "version")

	// A From address off the sending domain: the error is shown next to
	// the field and nothing is saved.
	bad := admSettingsValues(version)
	bad.Set("sender.from_email", "news@other.org")
	c.post("/admin/settings", bad).want(t, http.StatusUnprocessableEntity).contains(t,
		`id="f-sender.from_email-error"`, "must be an address on the sending domain example.com", `value="news@other.org"`)
	bad = admSettingsValues(version)
	bad.Set("sending.rate_fraction", "lots")
	c.post("/admin/settings", bad).want(t, http.StatusUnprocessableEntity).contains(t,
		`id="f-sending.rate_fraction-error"`, "must be a number")
	bad = admSettingsValues(version)
	bad.Set("default_send_window.end", "08:00")
	c.post("/admin/settings", bad).want(t, http.StatusUnprocessableEntity).contains(t, "must be after start")

	// A valid form is saved and the page says so.
	c.post("/admin/settings", admSettingsValues(version)).wantRedirect(t, "/admin/settings")
	st, err := hs.app.Settings.Get(hs.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Sender.FromName != "Example Letters" || st.Sender.FromEmail != "letters@example.com" || st.AlertEmail != "alerts@example.org" ||
		st.Timezone != "Europe/Copenhagen" || !st.Tracking.Clicks || st.Tracking.Opens || st.Sending.RateFraction != 0.5 ||
		st.CircuitBreaker.MinMessages != 150 || st.CircuitBreaker.ComplaintRate != 0.002 || st.CompanyFooter != "Example Ltd\n1 Street, Town" {
		t.Fatalf("settings = %+v", st)
	}
	if w := st.DefaultSendWindow; w == nil || !slices.Equal(w.Days, []int{1, 2, 3}) || w.Start != "09:00" || w.End != "17:30" {
		t.Fatalf("window = %+v", st.DefaultSendWindow)
	}
	c.get("/admin/settings").want(t, http.StatusOK).contains(t, "Settings saved.", `value="Example Letters"`, `value="17:30"`)

	// The form loaded before that save is stale now: a friendly message,
	// no overwrite.
	stale := admSettingsValues(version)
	stale.Set("sender.from_name", "Overwritten")
	c.post("/admin/settings", stale).want(t, http.StatusPreconditionFailed).contains(t, "Someone else changed the settings")
	if st, _ := hs.app.Settings.Get(hs.ctx); st.Sender.FromName != "Example Letters" {
		t.Fatalf("stale form overwrote the settings: %+v", st.Sender)
	}

	// Switching the window off removes it.
	off := admSettingsValues(c.get("/admin/settings").field(t, "version"))
	off.Del("default_send_window.enabled")
	c.post("/admin/settings", off).wantRedirect(t, "/admin/settings")
	if st, _ := hs.app.Settings.Get(hs.ctx); st.DefaultSendWindow != nil {
		t.Fatalf("window = %+v, want none", st.DefaultSendWindow)
	}

	// The change is in the audit log, with a filter.
	c.get("/admin/health/audit?operation=settings.update").want(t, http.StatusOK).contains(t, "settings.update", hs.email)
	c.get("/admin/health/audit?operation=token.create").want(t, http.StatusOK).contains(t, "No entries")
	c.get("/admin/health/audit?since=yesterday").want(t, http.StatusUnprocessableEntity).contains(t, "is not a date and time")
}
