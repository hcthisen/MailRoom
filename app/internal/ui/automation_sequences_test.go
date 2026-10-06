package ui_test

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// autoSeqVersion is the current version of sequence id (its If-Match).
func autoSeqVersion(hs *harness, id int) string {
	return strconv.Itoa(hs.count(`SELECT version FROM sequences WHERE id = ?`, id))
}

// autoMultipart posts a multipart form with the session's CSRF token and
// files (field name "files").
func (c *client) autoMultipart(path string, form url.Values, files map[string]string) resp {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("csrf_token", c.csrf)
	for k, vs := range form {
		for _, v := range vs {
			_ = mw.WriteField(k, v)
		}
	}
	for name, content := range files {
		fw, _ := mw.CreateFormFile("files", name)
		_, _ = io.WriteString(fw, content)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.do(req)
}

// autoNewSequence creates a sequence through the UI and returns its ID.
func autoNewSequence(t *testing.T, hs *harness, c *client, name string) int {
	t.Helper()
	c.post("/admin/sequences", url.Values{"name": {name}, "status": {"active"}, "reentry": {"never"}}).
		wantRedirect(t, "/admin/sequences/")
	return hs.count(`SELECT id FROM sequences WHERE name = ?`, name)
}

// autoAddStep adds a step through the UI.
func autoAddStep(t *testing.T, hs *harness, c *client, id int, subject, delay, unit string) {
	t.Helper()
	p := "/admin/sequences/" + strconv.Itoa(id)
	c.post(p+"/steps", url.Values{"version": {autoSeqVersion(hs, id)}, "delay_value": {delay}, "delay_unit": {unit},
		"subject": {subject}, "format": {"markdown"}, "body": {"Hi {{first_name | \"there\"}}, " + subject}}).
		wantRedirect(t, p)
}

func TestAutomationSequencesLoggedOut(t *testing.T) {
	hs := newHarness(t)
	c := hs.anon()
	for _, p := range []string{"/admin/sequences", "/admin/sequences/new", "/admin/sequences/import", "/admin/sequences/1",
		"/admin/sequences/1/edit", "/admin/sequences/1/steps/new", "/admin/sequences/1/steps/1", "/admin/sequences/1/enrollments",
		"/admin/sequences/1/enroll", "/admin/sequences/1/export", "/admin/sequences/1/export.zip"} {
		c.get(p).wantLogin(t)
	}
	c.postRaw("/admin/sequences", url.Values{"name": {"x"}}).wantLogin(t)
	if n := hs.count(`SELECT count(*) FROM sequences`); n != 0 {
		t.Fatalf("%d sequences created while logged out", n)
	}
}

func TestAutomationSequenceSettings(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	c.get("/admin/sequences").want(t, http.StatusOK).contains(t, "<h1>Sequences</h1>", `aria-current="page">Sequences`, "No sequences yet")
	c.get("/admin/sequences/new").want(t, http.StatusOK).contains(t, `name="trigger_tag"`, `name="send_window.days"`, `name="reentry"`)

	valid := url.Values{"name": {"Welcome"}, "status": {"active"}, "reentry": {"never"}, "trigger_tag": {"joined"},
		"exit_tags": {"customer, refunded"}, "exit_add_tag": {"welcome-exited"},
		"send_window.own": {"1"}, "send_window.days": {"1", "2", "3", "4", "5"}, "send_window.start": {"08:00"}, "send_window.end": {"20:00"}}
	c.postNoCSRF("/admin/sequences", valid).want(t, http.StatusForbidden)

	// A service validation error is shown next to its field, values kept.
	bad := url.Values{}
	for k, v := range valid {
		bad[k] = v
	}
	bad.Set("exit_tags", "customer, joined")
	c.post("/admin/sequences", bad).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-exit_tags-error"`, "must not contain the trigger tag", `value="Welcome"`)
	bad = url.Values{"name": {"Welcome"}, "send_window.own": {"1"}, "send_window.start": {"20:00"}, "send_window.end": {"08:00"},
		"send_window.days": {"1"}}
	c.post("/admin/sequences", bad).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-send_window.end-error"`, "must be after start")

	c.post("/admin/sequences", valid).wantRedirect(t, "/admin/sequences/")
	id := hs.count(`SELECT id FROM sequences WHERE name = 'Welcome'`)
	p := "/admin/sequences/" + strconv.Itoa(id)
	c.get(p).want(t, http.StatusOK).contains(t, "<h1>Welcome</h1>", "joined", "customer", "welcome-exited",
		"Mon–Fri, 08:00–20:00", "No steps yet")
	c.get("/admin/sequences").want(t, http.StatusOK).contains(t, `href="/admin/sequences/`+strconv.Itoa(id)+`"`, "Welcome")

	// Settings: edit, save, a stale version is refused (412).
	edit := c.get(p+"/edit").want(t, http.StatusOK).contains(t, `value="joined"`, `value="customer, refunded"`)
	version := edit.field(t, "version")
	upd := url.Values{"version": {version}, "name": {"Welcome drip"}, "status": {"active"}, "reentry": {"always"},
		"trigger_tag": {""}, "exit_tags": {"customer"}, "exit_add_tag": {""}}
	c.post(p, upd).wantRedirect(t, p)
	if n := hs.count(`SELECT count(*) FROM sequences WHERE id = ? AND name = 'Welcome drip' AND reentry = 'always'
		AND trigger_tag_id IS NULL AND send_window IS NULL`, id); n != 1 {
		t.Fatal("the settings were not saved")
	}
	c.post(p, upd).want(t, http.StatusPreconditionFailed).contains(t, "Someone else changed this")

	// Pause and resume.
	c.postNoCSRF(p+"/pause", url.Values{"version": {autoSeqVersion(hs, id)}}).want(t, http.StatusForbidden)
	c.post(p+"/pause", url.Values{"version": {autoSeqVersion(hs, id)}}).wantRedirect(t, p)
	c.get(p).want(t, http.StatusOK).contains(t, "This sequence is paused", "Resume")
	c.post(p+"/resume", url.Values{"version": {autoSeqVersion(hs, id)}}).wantRedirect(t, p)
	if n := hs.count(`SELECT count(*) FROM sequences WHERE id = ? AND status = 'active'`, id); n != 1 {
		t.Fatal("not resumed")
	}
	c.get("/admin/sequences/999").want(t, http.StatusNotFound)
}

func TestAutomationSequenceSteps(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	id := autoNewSequence(t, hs, c, "Drip")
	p := "/admin/sequences/" + strconv.Itoa(id)
	c.get(p+"/steps/new").want(t, http.StatusOK).contains(t, "Add step 1", `name="subject"`, `name="body"`,
		`hx-post="/admin/preview/email"`, `name="delay_value" type="number" value="0"`)
	c.postNoCSRF(p+"/steps", url.Values{"subject": {"x"}}).want(t, http.StatusForbidden)

	// Input errors are shown at the inputs.
	c.post(p+"/steps", url.Values{"version": {autoSeqVersion(hs, id)}, "delay_value": {"soon"}, "delay_unit": {"d"}, "subject": {"Hello"}, "body": {"x"}}).
		want(t, http.StatusUnprocessableEntity).contains(t, `id="f-delay_value-error"`, `value="Hello"`)
	c.post(p+"/steps", url.Values{"version": {autoSeqVersion(hs, id)}, "delay_value": {"400"}, "delay_unit": {"d"}, "subject": {"Hello"}, "body": {"x"}}).
		want(t, http.StatusUnprocessableEntity).contains(t, `id="f-delay_value-error"`, "365 days")

	autoAddStep(t, hs, c, id, "Welcome", "0", "d")
	autoAddStep(t, hs, c, id, "Two days in", "2", "d")
	if n := hs.count(`SELECT count(*) FROM sequence_steps WHERE sequence_id = ?`, id); n != 2 {
		t.Fatalf("%d steps, want 2", n)
	}
	c.get(p).want(t, http.StatusOK).contains(t, "Welcome", "right after enrolment", "2 days after step 1", "Two days in")

	step1 := hs.count(`SELECT id FROM sequence_steps WHERE sequence_id = ? AND position = 1`, id)
	step2 := hs.count(`SELECT id FROM sequence_steps WHERE sequence_id = ? AND position = 2`, id)
	sp := p + "/steps/" + strconv.Itoa(step2)

	// Edit: the email and the delay are prefilled; saving changes both.
	c.get(sp).want(t, http.StatusOK).contains(t, "<h1>Step 2</h1>", `value="Two days in"`, `name="delay_value" type="number" value="2"`,
		`<option value="d" selected>days</option>`, "Send a test", "owner@example.org")
	c.post(sp, url.Values{"version": {autoSeqVersion(hs, id)}, "delay_value": {"36"}, "delay_unit": {"h"},
		"subject": {"Day two"}, "preheader": {""}, "format": {"markdown"}, "body": {"New body"}}).wantRedirect(t, p)
	if n := hs.count(`SELECT delay_seconds FROM sequence_steps WHERE id = ?`, step2); n != 36*3600 {
		t.Fatalf("delay = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM emails e JOIN sequence_steps s ON s.email_id = e.id WHERE s.id = ? AND e.subject = 'Day two' AND e.body = 'New body'`, step2); n != 1 {
		t.Fatal("the step's email was not updated")
	}
	c.post(sp, url.Values{"version": {autoSeqVersion(hs, id)}, "delay_value": {"1"}, "delay_unit": {"d"},
		"subject": {""}, "format": {"markdown"}, "body": {"x"}}).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-subject-error"`)

	// Test send: to the owner by default, an invalid address is shown at the field.
	c.post(sp+"/test", url.Values{"to": {"not-an-address"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-to-error"`, "is not a valid email address")
	c.post(sp+"/test", url.Values{"to": {""}}).wantRedirect(t, sp)
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'test' AND to_email = 'owner@example.org'`); n != 1 {
		t.Fatalf("%d test messages to the owner", n)
	}
	c.get(sp).want(t, http.StatusOK).contains(t, "Test of step 2 queued to owner@example.org")

	// Reorder without active enrollments: no question asked.
	c.post(sp+"/move", url.Values{"dir": {"up"}, "version": {autoSeqVersion(hs, id)}}).wantRedirect(t, p)
	if n := hs.count(`SELECT position FROM sequence_steps WHERE id = ?`, step2); n != 1 {
		t.Fatalf("moved step is at %d", n)
	}

	// With an active enrollment, moving and deleting ask what happens to it.
	ct := testutil.InsertContact(t, hs.app.DB, "a@example.org", core.StatusActive)
	testutil.InsertEnrollment(t, hs.app.DB, int64(id), ct.ID, int64(step1))
	c.post(sp+"/move", url.Values{"dir": {"down"}, "version": {autoSeqVersion(hs, id)}}).want(t, http.StatusOK).
		contains(t, "Move step 1 down?", `name="on_active" value="keep" checked`, `value="end"`, `name="confirm" value="yes"`)
	c.post(sp+"/move", url.Values{"dir": {"down"}, "version": {autoSeqVersion(hs, id)}, "on_active": {"keep"}, "confirm": {"yes"}}).
		wantRedirect(t, p)
	if n := hs.count(`SELECT position FROM sequence_steps WHERE id = ?`, step2); n != 2 {
		t.Fatalf("moved step is at %d", n)
	}

	s1 := p + "/steps/" + strconv.Itoa(step1)
	c.postNoCSRF(s1+"/delete", nil).want(t, http.StatusForbidden)
	c.post(s1+"/delete", url.Values{"version": {autoSeqVersion(hs, id)}}).want(t, http.StatusOK).
		contains(t, "Delete step 1?", "1 active enrollment is waiting", `name="on_active" value="advance" checked`, `value="exit"`)
	if n := hs.count(`SELECT count(*) FROM sequence_steps WHERE id = ?`, step1); n != 1 {
		t.Fatal("the confirmation page deleted the step")
	}
	c.post(s1+"/delete", url.Values{"version": {autoSeqVersion(hs, id)}, "on_active": {"exit"}, "confirm": {"yes"}}).wantRedirect(t, p)
	c.get(p).want(t, http.StatusOK).contains(t, "Step 1 deleted. 1 enrollment was ended.")
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE status = 'exited' AND exit_reason = 'step_deleted'`); n != 1 {
		t.Fatal("the waiting enrollment was not ended")
	}
	c.get(s1).want(t, http.StatusNotFound)
}

func TestAutomationEnrollments(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	id := autoNewSequence(t, hs, c, "Onboarding")
	autoAddStep(t, hs, c, id, "Hello", "1", "h")
	autoAddStep(t, hs, c, id, "Next", "1", "d")
	p := "/admin/sequences/" + strconv.Itoa(id)
	testutil.InsertContact(t, hs.app.DB, "a@example.org", core.StatusActive)
	testutil.InsertContact(t, hs.app.DB, "b@example.org", core.StatusActive)
	testutil.InsertContact(t, hs.app.DB, "c@example.org", core.StatusUnsubscribed)

	c.get(p+"/enrollments").want(t, http.StatusOK).contains(t, "Nobody is enrolled yet")
	c.get(p+"/enroll").want(t, http.StatusOK).contains(t, `name="emails"`, "Step 2: Next", `name="start_at"`)
	c.postNoCSRF(p+"/enroll", url.Values{"emails": {"a@example.org"}}).want(t, http.StatusForbidden)

	c.post(p+"/enroll", url.Values{"emails": {"a@example.org, bad address"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-emails-error"`, `&#34;bad&#34; is not a valid email address`)
	c.post(p+"/enroll", url.Values{"emails": {""}}).want(t, http.StatusUnprocessableEntity).contains(t, "enter at least one email address")

	// Step 1: the dry run, on a confirmation page; nothing is enrolled.
	form := url.Values{"emails": {"a@example.org\nb@example.org\nc@example.org\nnobody@example.org"}, "at_step": {"2"},
		"start_at": {"2026-01-10T09:00"}, "source": {"migration"}}
	conf := c.post(p+"/enroll", form).want(t, http.StatusOK).
		contains(t, "Enrol 2 contacts?", "2 contacts are enrolled.", "1 contact skipped: no contact with this address",
			"1 contact skipped: not active", "They start at step 2", `name="confirm" value="yes"`)
	if n := hs.count(`SELECT count(*) FROM enrollments`); n != 0 {
		t.Fatalf("the dry run enrolled %d", n)
	}
	// Step 2: the confirmation re-posts the fields.
	form.Set("emails", conf.field(t, "emails"))
	form.Set("confirm", "yes")
	c.post(p+"/enroll", form).wantRedirect(t, p+"/enrollments")
	if n := hs.count(`SELECT count(*) FROM enrollments e JOIN sequence_steps s ON s.id = e.next_step_id
		WHERE e.sequence_id = ? AND e.status = 'active' AND e.source = 'migration' AND s.position = 2`, id); n != 2 {
		t.Fatalf("%d enrollments at step 2", n)
	}
	c.get(p+"/enrollments").want(t, http.StatusOK).contains(t, "2 contacts enrolled.", "a@example.org", "b@example.org", "Step 2", "migration")
	c.get(p+"/enrollments?status=completed").want(t, http.StatusOK).contains(t, "No enrollments with this status")

	// Nobody new: the form says so instead of a confirmation.
	c.post(p+"/enroll", url.Values{"emails": {"a@example.org"}}).want(t, http.StatusOK).
		contains(t, "Nobody would be enrolled.", "already in this sequence")

	// Manual exit, with a confirmation.
	eid := hs.count(`SELECT e.id FROM enrollments e JOIN contacts c ON c.id = e.contact_id WHERE c.email = 'a@example.org'`)
	ep := p + "/enrollments/" + strconv.Itoa(eid) + "/exit"
	c.postNoCSRF(ep, nil).want(t, http.StatusForbidden)
	c.post(ep, url.Values{"email": {"a@example.org"}}).want(t, http.StatusOK).contains(t, "End this enrollment?", "a@example.org gets no further emails")
	c.post(ep, url.Values{"confirm": {"yes"}}).wantRedirect(t, p+"/enrollments")
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE id = ? AND status = 'exited' AND exit_reason = 'manual'`, eid); n != 1 {
		t.Fatal("the enrollment was not ended")
	}
	// Ending it again is a conflict, shown as an error page.
	c.post(ep, url.Values{"confirm": {"yes"}}).want(t, http.StatusConflict)

	// The sequence page shows the numbers.
	c.get(p).want(t, http.StatusOK).contains(t, `<p class="label">Active</p>`, `<p class="value">1</p>`)

	// Deleting a sequence with active enrollments says so, then ends them.
	c.post(p+"/delete", nil).want(t, http.StatusOK).contains(t, "Delete &#34;Onboarding&#34;?", "1 active enrollment ends", `name="force" value="1"`)
	if n := hs.count(`SELECT count(*) FROM sequences WHERE id = ?`, id); n != 1 {
		t.Fatal("the confirmation page deleted the sequence")
	}
	c.post(p+"/delete", url.Values{"version": {autoSeqVersion(hs, id)}, "force": {"1"}, "confirm": {"yes"}}).wantRedirect(t, "/admin/sequences")
	if n := hs.count(`SELECT count(*) FROM sequences WHERE id = ?`, id); n != 0 {
		t.Fatal("the sequence was not deleted")
	}
	c.get("/admin/sequences").want(t, http.StatusOK).contains(t, "Sequence deleted; 1 active enrollment ended.")
}

func TestAutomationSequenceYAML(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	c.get("/admin/sequences/import").want(t, http.StatusOK).contains(t, `enctype="multipart/form-data"`, `name="yaml"`, `name="files"`)

	doc := "name: Imported\ntrigger:\n  tag: signed-up\nsteps:\n  - delay: \"0\"\n    subject: Welcome\n    body_file: 01-welcome.md\n  - delay: 2d\n    subject: Later\n    body: Hello again\n"
	files := map[string]string{"01-welcome.md": "Hi {{first_name | \"there\"}}!"}

	// A document error is shown with the place it is at.
	c.autoMultipart("/admin/sequences/import", url.Values{"yaml": {strings.Replace(doc, "2d", "2 weeks", 1)}}, files).
		want(t, http.StatusUnprocessableEntity).contains(t, "steps[1].delay", "01-welcome.md")

	// The first post is a dry run: a preview and nothing stored.
	pre := c.autoMultipart("/admin/sequences/import", url.Values{"yaml": {doc}}, files).want(t, http.StatusOK).
		contains(t, "This import creates", "Imported", "signed-up", "Welcome", "Later", "2d", "Nothing has been saved yet",
			`name="file_name" value="01-welcome.md"`, `name="confirm" value="yes"`)
	if n := hs.count(`SELECT count(*) FROM sequences`); n != 0 {
		t.Fatal("the dry run stored a sequence")
	}
	// "Back to edit" keeps everything.
	carried := url.Values{"yaml": {pre.field(t, "yaml")}, "file_name": {"01-welcome.md"}, "file_body": {pre.field(t, "file_body")}}
	back := url.Values{"edit": {"1"}}
	for k, v := range carried {
		back[k] = v
	}
	c.autoMultipart("/admin/sequences/import", back, nil).want(t, http.StatusOK).contains(t, "Files already added", "01-welcome.md", "name: Imported")
	// The confirmation imports it.
	carried.Set("confirm", "yes")
	c.autoMultipart("/admin/sequences/import", carried, nil).wantRedirect(t, "/admin/sequences/")
	id := hs.count(`SELECT id FROM sequences WHERE name = 'Imported'`)
	if n := hs.count(`SELECT count(*) FROM sequence_steps WHERE sequence_id = ?`, id); n != 2 {
		t.Fatalf("%d steps imported", n)
	}
	// The same name again is a conflict shown above the form.
	c.autoMultipart("/admin/sequences/import", url.Values{"yaml": {doc}}, files).want(t, http.StatusConflict).contains(t, `class="form-error"`)

	// Export: the page and the zip.
	p := "/admin/sequences/" + strconv.Itoa(id)
	c.get(p+"/export").want(t, http.StatusOK).contains(t, "imported.yaml", "name: Imported", "signed-up", "Hi {{first_name | &#34;there&#34;}}!")
	z := c.get(p+"/export.zip").want(t, http.StatusOK)
	if ct := z.header.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := z.header.Get("Content-Disposition"); cd != `attachment; filename="imported.zip"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	zr, err := zip.NewReader(strings.NewReader(z.body), int64(len(z.body)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	if !strings.Contains(got["imported.yaml"], "name: Imported") || len(got) != 3 {
		t.Fatalf("zip = %v", got)
	}
	// What the zip holds imports again (under another name) through the API.
	y := strings.Replace(got["imported.yaml"], "name: Imported", "name: Imported copy", 1)
	fl := map[string]string{}
	for n, b := range got {
		if n != "imported.yaml" {
			fl[n] = b
		}
	}
	if _, err := hs.app.Sequences.ImportYAML(hs.ctx, y, fl, true); err != nil {
		t.Fatalf("re-import of the export: %v", err)
	}
}

func TestAutomationSequenceActionsAreRegistered(t *testing.T) {
	hs := newHarness(t)
	want := map[string]string{
		"POST /admin/sequences":                                       "create-sequence",
		"POST /admin/sequences/{id}":                                  "update-sequence",
		"POST /admin/sequences/{id}/pause":                            "pause-sequence",
		"POST /admin/sequences/{id}/resume":                           "resume-sequence",
		"POST /admin/sequences/{id}/delete":                           "delete-sequence",
		"POST /admin/sequences/import":                                "import-sequence",
		"GET /admin/sequences/{id}/export.zip":                        "export-sequence",
		"POST /admin/sequences/{id}/steps":                            "add-sequence-step",
		"POST /admin/sequences/{id}/steps/{step_id}":                  "update-sequence-step",
		"POST /admin/sequences/{id}/steps/{step_id}/delete":           "delete-sequence-step",
		"POST /admin/sequences/{id}/steps/{step_id}/move":             "reorder-sequence-steps",
		"POST /admin/sequences/{id}/steps/{step_id}/test":             "send-sequence-step-test",
		"POST /admin/sequences/{id}/enroll":                           "enroll-sequence-contacts",
		"POST /admin/sequences/{id}/enrollments/{enrollment_id}/exit": "unenroll-sequence-contact",
		"POST /admin/forms":                                           "create-form",
		"POST /admin/forms/{id}":                                      "update-form",
		"POST /admin/forms/{id}/delete":                               "delete-form",
		"GET /admin/forms/{id}/snippet":                               "get-form-snippet",
		"GET /admin/forms/{id}/submissions":                           "list-form-submissions",
	}
	got := map[string]string{}
	for _, a := range hs.app.UI.Actions() {
		got[a.Method+" "+a.Path] = a.OperationID
	}
	for route, op := range want {
		if got[route] != op {
			t.Errorf("%s -> %q, want %q", route, got[route], op)
		}
	}
}
