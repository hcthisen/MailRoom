package ui_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestAutomationFormsLoggedOut(t *testing.T) {
	hs := newHarness(t)
	c := hs.anon()
	for _, p := range []string{"/admin/forms", "/admin/forms/new", "/admin/forms/1", "/admin/forms/1/snippet", "/admin/forms/1/submissions"} {
		c.get(p).wantLogin(t)
	}
	c.postRaw("/admin/forms", url.Values{"name": {"x"}}).wantLogin(t)
}

// autoFormValues is a valid create/edit form.
func autoFormValues(seqID int) url.Values {
	return url.Values{
		"name": {"Newsletter sign-up"}, "tags": {"newsletter, vip"}, "sequence_id": {strconv.Itoa(seqID)},
		"fields[0].kind": {"first_name"}, "fields[0].attr": {""}, "fields[0].label": {"First name"}, "fields[0].required": {"1"},
		"fields[1].kind": {""}, "fields[1].attr": {""}, "fields[1].label": {"ignored: row not used"},
		"fields[2].kind": {"attr"}, "fields[2].attr": {"company"}, "fields[2].label": {"Company"},
		"consent_text": {""}, "redirect_url": {"https://example.com/thanks"}, "allowed_origins": {"https://example.com\nhttps://www.example.com/"},
	}
}

func TestAutomationForms(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()
	seqID := autoNewSequence(t, hs, c, "Welcome")

	c.get("/admin/forms").want(t, http.StatusOK).contains(t, "<h1>Forms</h1>", `aria-current="page">Forms`, "No forms yet")
	c.get("/admin/forms/new").want(t, http.StatusOK).contains(t, `name="fields[0].kind"`, `<option value="first_name" selected>`,
		`name="double_optin" value="1"`, "Off by default", `<option value="`+strconv.Itoa(seqID)+`">Welcome</option>`).
		lacks(t, `name="double_optin" value="1" checked`)

	form := autoFormValues(seqID)
	c.postNoCSRF("/admin/forms", form).want(t, http.StatusForbidden)

	// Validation errors land next to their inputs, with the values kept.
	bad := autoFormValues(seqID)
	bad.Set("redirect_url", "http://example.com/thanks")
	c.post("/admin/forms", bad).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-redirect_url-error"`, "must be an absolute https URL", `value="Newsletter sign-up"`)
	bad = autoFormValues(seqID)
	bad.Set("allowed_origins", "https://example.com\nexample.org/path")
	c.post("/admin/forms", bad).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-allowed_origins-error"`, "must be an origin like")
	bad = autoFormValues(seqID)
	bad.Set("fields[2].attr", "no spaces")
	c.post("/admin/forms", bad).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-fields-1-error"`, "attribute key", `value="no spaces"`) // row 2 moved up: row 1 was unused
	if n := hs.count(`SELECT count(*) FROM forms`); n != 0 {
		t.Fatalf("%d forms stored by failed attempts", n)
	}

	c.post("/admin/forms", form).wantRedirect(t, "/admin/forms/")
	id := hs.count(`SELECT id FROM forms WHERE name = 'Newsletter sign-up'`)
	if n := hs.count(`SELECT count(*) FROM forms WHERE id = ? AND double_optin = 0 AND sequence_id = ?
		AND json_array_length(fields) = 2 AND json_array_length(allowed_origins) = 2 AND redirect_url = 'https://example.com/thanks'`, id, seqID); n != 1 {
		t.Fatal("the form was not stored as submitted (double opt-in must default to off)")
	}
	p := "/admin/forms/" + strconv.Itoa(id)
	publicID := ""
	if err := hs.app.DB.Read.QueryRow(`SELECT public_id FROM forms WHERE id = ?`, id).Scan(&publicID); err != nil {
		t.Fatal(err)
	}

	// The snippet page: embed HTML, hosted URL, copy buttons and the preview.
	c.get(p+"/snippet").want(t, http.StatusOK).contains(t, "Form &#34;Newsletter sign-up&#34; created.",
		`data-copy="#snippet-code"`, `data-copy="#hosted-url"`, "/f/"+publicID,
		`sandbox=""`, "&lt;form", "Company")
	c.get("/admin/forms").want(t, http.StatusOK).contains(t, "Newsletter sign-up", "newsletter", "vip", ">Welcome</a>", "/f/"+publicID)

	// Edit: prefilled; saving switches on double opt-in; a stale version is refused.
	edit := c.get(p).want(t, http.StatusOK).contains(t, `value="company"`, `value="Company"`, `https://www.example.com`,
		`<option value="attr" selected>`, "Delete…").lacks(t, ">By signing up you agree")
	upd := autoFormValues(seqID)
	upd.Set("version", edit.field(t, "version"))
	upd.Set("double_optin", "1")
	upd.Set("consent_text", "Yes, send me the newsletter.")
	upd.Set("sequence_id", "0")
	c.post(p, upd).wantRedirect(t, p)
	if n := hs.count(`SELECT count(*) FROM forms WHERE id = ? AND double_optin = 1 AND sequence_id IS NULL AND consent_text = 'Yes, send me the newsletter.'`, id); n != 1 {
		t.Fatal("the change was not saved")
	}
	c.post(p, upd).want(t, http.StatusPreconditionFailed).contains(t, "Someone else changed this")
	c.get(p).want(t, http.StatusOK).contains(t, `name="double_optin" value="1" checked`, "Yes, send me the newsletter.")

	// Submissions: a sign-up on the public endpoint shows up.
	c.get(p+"/submissions").want(t, http.StatusOK).contains(t, "No submissions yet")
	req := httptest.NewRequest(http.MethodPost, "/f/"+publicID, strings.NewReader(url.Values{"email": {"new@example.org"}, "first_name": {"New"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.7:1234"
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		t.Fatalf("public submission: %d %s", rec.Code, rec.Body.String())
	}
	c.get(p+"/submissions").want(t, http.StatusOK).contains(t, "new@example.org", "optin pending", "confirmation email sent")

	// Delete: a confirmation first (the service's dry run), then gone.
	c.postNoCSRF(p+"/delete", nil).want(t, http.StatusForbidden)
	c.post(p+"/delete", url.Values{"version": {strconv.Itoa(hs.count(`SELECT version FROM forms WHERE id = ?`, id))}}).
		want(t, http.StatusOK).contains(t, "Delete &#34;Newsletter sign-up&#34;?", "stop working", `name="confirm" value="yes"`)
	if n := hs.count(`SELECT count(*) FROM forms`); n != 1 {
		t.Fatal("the confirmation page deleted the form")
	}
	c.post(p+"/delete", url.Values{"confirm": {"yes"}, "version": {strconv.Itoa(hs.count(`SELECT version FROM forms WHERE id = ?`, id))}}).
		wantRedirect(t, "/admin/forms")
	if n := hs.count(`SELECT count(*) FROM forms`); n != 0 {
		t.Fatal("the form was not deleted")
	}
	c.get(p).want(t, http.StatusNotFound)
}
