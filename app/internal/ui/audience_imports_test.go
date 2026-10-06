package ui_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

const audCSV = "email,first_name,status,tags,consent_source\n" +
	"new1@example.org,Nia,active,newsletter;customer,fair\n" +
	"new2@example.org,,unsubscribed,newsletter,fair\n" +
	"old@example.org,Olga,active,,fair\n" +
	"broken,,active,,\n"

func TestAudienceImports(t *testing.T) {
	hs := audHarness(t)
	hs.audStartJobs()
	c := hs.session()
	testutil.InsertContact(t, hs.app.DB, "old@example.org", core.StatusUnsubscribed)

	c.get("/admin/imports").want(t, http.StatusOK).contains(t, "<h1>Import contacts</h1>", `enctype="multipart/form-data"`, "No imports yet")

	// CSRF is checked on the multipart upload too.
	c.audMultipart("/admin/imports", map[string]string{"mode": "check"}, "list.csv", audCSV, false).want(t, http.StatusForbidden)
	// No file.
	c.audMultipart("/admin/imports", map[string]string{"mode": "check"}, "", "", true).
		want(t, http.StatusUnprocessableEntity).contains(t, "choose a CSV file")
	// A file without an email column is refused on the form.
	c.audMultipart("/admin/imports", map[string]string{"mode": "check"}, "bad.csv", "name\nx\n", true).
		want(t, http.StatusUnprocessableEntity).contains(t, `id="f-file-error"`)

	// Step 1: the dry run's report; nothing is written.
	rep := c.audMultipart("/admin/imports", map[string]string{"mode": "check", "on_existing": "update"}, "list.csv", audCSV, true).
		want(t, http.StatusOK).contains(t, "list.csv: ready to import?", "4 rows checked", "Rejected rows", "Import now",
		`name="csv_b64"`, "newsletter", "an import never reactivates anyone")
	if n := hs.count(`SELECT COUNT(*) FROM contacts`); n != 1 {
		t.Fatalf("dry run wrote contacts: %d", n)
	}
	// Step 2: the carried-along file is imported as a job.
	form := url.Values{"mode": {"import"}, "on_existing": {rep.field(t, "on_existing")}, "filename": {rep.field(t, "filename")},
		"csv_b64": {rep.field(t, "csv_b64")}}
	c.postNoCSRF("/admin/imports", form).want(t, http.StatusForbidden)
	r := c.post("/admin/imports", form).want(t, http.StatusSeeOther)
	id := audLocationID(t, r, "/admin/imports/")
	hs.audWaitJob(id)
	if n := hs.count(`SELECT COUNT(*) FROM contacts`); n != 3 {
		t.Fatalf("contacts after import = %d", n)
	}
	if n := hs.count(`SELECT COUNT(*) FROM contacts WHERE email = 'old@example.org' AND status = 'unsubscribed'`); n != 1 {
		t.Fatal("import reactivated a contact")
	}

	page := fmt.Sprintf("/admin/imports/%d", id)
	c.get(page).want(t, http.StatusOK).contains(t, "The import has started.", "The import is complete.", "list.csv", "Rejected rows").
		lacks(t, `hx-trigger="every 2s"`)
	// htmx polling gets the status block only.
	frag := c.htmx(http.MethodGet, page, nil).want(t, http.StatusOK)
	frag.contains(t, `id="import-status"`).lacks(t, "<html", "<nav")
	c.get("/admin/imports").want(t, http.StatusOK).contains(t, fmt.Sprintf(`href="/admin/imports/%d"`, id), "list.csv")
	c.get("/admin/imports/99999").want(t, http.StatusNotFound)
}

func TestAudienceImportLargeFile(t *testing.T) {
	hs := audHarness(t)
	hs.audStartJobs()
	c := hs.session()
	// Above the embed limit the report asks for the file again.
	var b strings.Builder
	b.WriteString("email,first_name\n")
	for i := 0; b.Len() < 8<<20+1024; i++ {
		fmt.Fprintf(&b, "person%07d@example.org,%s\n", i, strings.Repeat("x", 200))
	}
	c.audMultipart("/admin/imports", map[string]string{"mode": "check"}, "big.csv", b.String(), true).
		want(t, http.StatusOK).contains(t, "Choose the same file again").lacks(t, `name="csv_b64"`)
	if n := hs.count(`SELECT COUNT(*) FROM contacts`); n != 0 {
		t.Fatalf("dry run wrote contacts: %d", n)
	}
	// The file chosen again is imported directly.
	r := c.audMultipart("/admin/imports", map[string]string{"mode": "import", "on_existing": "skip"}, "again.csv", audCSV, true).
		want(t, http.StatusSeeOther)
	hs.audWaitJob(audLocationID(t, r, "/admin/imports/"))
	if n := hs.count(`SELECT COUNT(*) FROM contacts`); n != 3 {
		t.Fatalf("contacts = %d", n)
	}
}
