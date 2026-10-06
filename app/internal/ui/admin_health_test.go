package ui_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mailroom/internal/jobs"
)

func TestAdminHealthDoctorAndSending(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	c.get("/admin/health").want(t, http.StatusOK).contains(t,
		"Install checks", "The checks have not run yet", "SES is not configured yet", "Pause all sending",
		"Send queue", "1.2.3-test")

	// Run the checks with htmx: only the card comes back, with a flash.
	r := c.htmx(http.MethodPost, "/admin/health/doctor", url.Values{}).want(t, http.StatusOK).
		contains(t, `id="doctor-card"`, "ses_identity", "Fix:", `hx-swap-oob`).lacks(t, "<html")
	if _, ok := hs.app.UI.LastDoctor(); !ok {
		t.Fatal("the doctor report was not kept")
	}
	_ = r
	// Without htmx: back to the overview, which shows the report.
	c.post("/admin/health/doctor", nil).wantRedirect(t, "/admin/health")
	c.get("/admin/health").want(t, http.StatusOK).contains(t, "Checks finished", "ses_identity").lacks(t, "have not run yet")

	// Pause with a reason.
	c.post("/admin/health/sending/pause", url.Values{"reason": {strings.Repeat("x", 501)}}).
		want(t, http.StatusUnprocessableEntity).contains(t, "must be at most 500 characters")
	c.post("/admin/health/sending/pause", url.Values{"reason": {"checking the <list>"}}).wantRedirect(t, "/admin/health")
	st, err := hs.app.Sending.GetState(hs.ctx, nil)
	if err != nil || !st.Paused || st.Reason != "checking the <list>" || st.Source != "manual" {
		t.Fatalf("state = %+v, %v", st, err)
	}
	c.get("/admin/health").want(t, http.StatusOK).contains(t, "All sending is paused", "checking the &lt;list&gt;", "Resume sending").
		lacks(t, "checking the <list>")
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'sending.pause' AND actor_name = ?`, hs.email); n != 1 {
		t.Fatalf("pause audit entries = %d", n)
	}

	// Resume asks first and says what will be sent.
	c.post("/admin/health/sending/resume", nil).want(t, http.StatusOK).contains(t,
		"Resume sending?", "0 emails are queued", "Paused because: checking the &lt;list&gt;", `name="confirm" value="yes"`)
	if st, _ := hs.app.Sending.GetState(hs.ctx, nil); !st.Paused {
		t.Fatal("the confirmation step resumed sending")
	}
	c.post("/admin/health/sending/resume", url.Values{"confirm": {"yes"}}).wantRedirect(t, "/admin/health")
	if st, _ := hs.app.Sending.GetState(hs.ctx, nil); st.Paused {
		t.Fatal("still paused")
	}
	c.get("/admin/health").contains(t, "Sending resumed.", "Pause all sending")
	c.post("/admin/health/sending/resume", nil).wantRedirect(t, "/admin/health")
	c.get("/admin/health").contains(t, "Sending is not paused.")
}

func TestAdminBackupsAndJobs(t *testing.T) {
	hs := newHarness(t)
	admStartJobs(t, hs)
	c := hs.session()
	c.get("/admin/health/backups").want(t, http.StatusOK).contains(t, "No backups yet", "every day at 02:30 (Europe/London)", "14 days")

	// Take a backup: the job page follows it.
	r := c.post("/admin/health/backups", nil).wantRedirect(t, "/admin/health/jobs/")
	jobURL := r.header.Get("Location")
	id := strings.TrimPrefix(jobURL, "/admin/health/jobs/")
	js, err := hs.app.Jobs.List(hs.ctx, jobs.ListFilter{Limit: 10})
	if err != nil || len(js) != 1 || admItoa(js[0].ID) != id || js[0].Type != "backup" {
		t.Fatalf("jobs = %+v, %v", js, err)
	}
	ctx, cancel := context.WithTimeout(hs.ctx, 30*time.Second)
	defer cancel()
	if j, err := hs.app.Jobs.Wait(ctx, js[0].ID); err != nil || j.Status != "succeeded" {
		t.Fatalf("backup job = %+v, %v", j, err)
	}
	c.get(jobURL).want(t, http.StatusOK).contains(t, "Backup started.", "succeeded", "Go to backups").lacks(t, `hx-trigger="every 2s"`)
	// htmx polling gets just the card.
	c.htmx(http.MethodGet, jobURL, nil).want(t, http.StatusOK).contains(t, `id="job-card"`).lacks(t, "<html")
	c.get("/admin/health/jobs?status=succeeded&type=backup").want(t, http.StatusOK).contains(t, `href="`+jobURL+`"`)
	c.get("/admin/health/jobs?status=failed").want(t, http.StatusOK).contains(t, "No jobs")
	c.get("/admin/health/jobs?status=bogus").want(t, http.StatusUnprocessableEntity)

	// The backup is listed and can be downloaded.
	list, err := hs.app.System.ListBackups()
	if err != nil || len(list) != 1 {
		t.Fatalf("backups = %v, %v", list, err)
	}
	name := list[0].Name
	c.get("/admin/health/backups").want(t, http.StatusOK).contains(t, name, `href="/admin/health/backups/`+name+`"`).lacks(t, "No backups yet")
	hs.anon().get("/admin/health/backups/" + name).wantLogin(t)
	d := c.get("/admin/health/backups/"+name).want(t, http.StatusOK)
	if ct := d.header.Get("Content-Type"); ct != "application/vnd.sqlite3" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := d.header.Get("Content-Disposition"); cd != `attachment; filename="`+name+`"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if !strings.HasPrefix(d.body, "SQLite format 3") {
		t.Fatalf("download is not a SQLite file: %.20q", d.body)
	}
	c.get("/admin/health/backups/not-a-backup.txt").want(t, http.StatusUnprocessableEntity)
	c.get("/admin/health/backups/mailroom-20200101T000000Z.db").want(t, http.StatusNotFound)
}
