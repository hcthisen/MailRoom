package forms_test

import (
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/forms"
	"mailroom/internal/testutil"
)

var publicIDPattern = regexp.MustCompile(`^[a-z2-7]{10}$`)

func TestCreateFormDefaults(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"  Newsletter  "}`)
	if f.Name != "Newsletter" || f.DoubleOptin || f.Version != 1 || f.SequenceID != nil || f.RedirectURL != "" {
		t.Fatalf("form = %+v", f)
	}
	if !publicIDPattern.MatchString(f.PublicID) {
		t.Fatalf("public_id = %q", f.PublicID)
	}
	if f.HostedURL != baseURL+"/f/"+f.PublicID || f.Endpoint != f.HostedURL {
		t.Fatalf("urls = %s %s", f.HostedURL, f.Endpoint)
	}
	// K-4: the default consent text mentions email and per-person click tracking.
	if f.ConsentText != forms.DefaultConsentText || !strings.Contains(f.ConsentText, "emails") || !strings.Contains(f.ConsentText, "clicks") {
		t.Fatalf("consent text = %q", f.ConsentText)
	}
	if f.Fields == nil || f.Tags == nil || f.AllowedOrigins == nil {
		t.Fatalf("lists must encode as [], got %+v", f)
	}
	if n := h.count(`SELECT count(*) FROM audit_log WHERE operation = 'form.create' AND target_id = ? AND actor_name = 'setup-agent'`, fmt.Sprint(f.ID)); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	other := h.createForm(`{"name":"Second"}`)
	if other.PublicID == f.PublicID {
		t.Fatal("public IDs must differ")
	}
}

func TestCreateFormFull(t *testing.T) {
	h := newHarness(t)
	seq := testutil.InsertSequence(t, h.d, "Welcome")
	f := h.createForm(fmt.Sprintf(`{"name":"Community","fields":[{"key":"first_name","required":true},{"key":"attr.company_size","label":" Team size "}],
		"tags":["community"," Community ","vip"],"sequence_id":%d,"double_optin":true,"consent_text":"I agree.","redirect_url":"https://example.com/thanks",
		"allowed_origins":["https://Example.com/","http://localhost:3000","https://example.com"]}`, seq))
	want := []forms.FormField{{Key: "first_name", Label: "First name", Required: true}, {Key: "attr.company_size", Label: "Team size"}}
	if fmt.Sprint(f.Fields) != fmt.Sprint(want) {
		t.Fatalf("fields = %+v", f.Fields)
	}
	if strings.Join(f.Tags, ",") != "community,vip" || f.SequenceID == nil || *f.SequenceID != seq || !f.DoubleOptin {
		t.Fatalf("form = %+v", f)
	}
	if strings.Join(f.AllowedOrigins, ",") != "https://example.com,http://localhost:3000" {
		t.Fatalf("origins = %v", f.AllowedOrigins)
	}
	if f.ConsentText != "I agree." || f.RedirectURL != "https://example.com/thanks" {
		t.Fatalf("form = %+v", f)
	}
	if n := h.count(`SELECT count(*) FROM tags`); n != 2 {
		t.Fatalf("tags created = %d", n)
	}
	var got forms.Form
	r := h.do("GET", fmt.Sprintf("/forms/%d", f.ID), "").wantStatus(t, http.StatusOK)
	r.decode(t, &got)
	if r.header.Get("ETag") != `"v1"` || !reflect.DeepEqual(got, f) {
		t.Fatalf("get = %+v etag %s", got, r.header.Get("ETag"))
	}
}

func TestCreateFormValidation(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name, body, field string
	}{
		{"no name", `{"name":"   "}`, "name"},
		{"email field", `{"name":"x","fields":[{"key":"email"}]}`, "fields[0].key"},
		{"unknown key", `{"name":"x","fields":[{"key":"phone"}]}`, "fields[0].key"},
		{"bad attr key", `{"name":"x","fields":[{"key":"attr.bad-key"}]}`, "fields[0].key"},
		{"duplicate key", `{"name":"x","fields":[{"key":"first_name"},{"key":"first_name"}]}`, "fields[1].key"},
		{"empty tag", `{"name":"x","tags":[" "]}`, "tags"},
		{"http redirect", `{"name":"x","redirect_url":"http://example.com/thanks"}`, "redirect_url"},
		{"relative redirect", `{"name":"x","redirect_url":"/thanks"}`, "redirect_url"},
		{"javascript redirect", `{"name":"x","redirect_url":"javascript:alert(1)"}`, "redirect_url"},
		{"origin with path", `{"name":"x","allowed_origins":["https://example.com/signup"]}`, "allowed_origins[0]"},
		{"origin wrong scheme", `{"name":"x","allowed_origins":["ftp://example.com"]}`, "allowed_origins[0]"},
		{"unknown sequence", `{"name":"x","sequence_id":99}`, "sequence_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.do("POST", "/forms", tt.body).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, tt.field)
		})
	}
	if n := h.count(`SELECT count(*) FROM forms`); n != 0 {
		t.Fatalf("forms = %d", n)
	}
}

func TestFormScopesAndErrors(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"x"}`)
	h.doAs(readToken, "POST", "/forms", `{"name":"y"}`).wantError(t, http.StatusForbidden, api.CodeForbidden, "")
	h.doAs(readToken, "GET", fmt.Sprintf("/forms/%d", f.ID), "").wantStatus(t, http.StatusOK)
	h.doAs("", "GET", "/forms", "").wantError(t, http.StatusUnauthorized, api.CodeUnauthorized, "")
	h.do("GET", "/forms/999", "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	h.do("GET", "/forms/999/snippet", "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	h.do("GET", "/forms/999/submissions", "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
}

func TestUpdateForm(t *testing.T) {
	h := newHarness(t)
	seq := testutil.InsertSequence(t, h.d, "Welcome")
	f := h.createForm(fmt.Sprintf(`{"name":"Old","tags":["a"],"sequence_id":%d}`, seq))
	path := fmt.Sprintf("/forms/%d", f.ID)

	// Stale If-Match.
	h.do("PATCH", path, `{"name":"New"}`, "If-Match", `"v7"`).wantError(t, http.StatusPreconditionFailed, api.CodePreconditionFailed, "")
	h.do("PATCH", path, `{"name":"New"}`, "If-Match", `bogus`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "header.If-Match")

	// Dry run changes nothing.
	var dry forms.Form
	h.do("PATCH", path+"?dry_run=true", `{"name":"Dry","double_optin":true}`, "If-Match", `"v1"`).wantStatus(t, http.StatusOK).decode(t, &dry)
	if dry.Name != "Dry" || !dry.DoubleOptin || dry.Version != 2 {
		t.Fatalf("dry run result = %+v", dry)
	}
	if cur, _ := h.svc.Get(t.Context(), f.ID); cur.Name != "Old" || cur.Version != 1 {
		t.Fatalf("dry run stored: %+v", cur)
	}

	var got forms.Form
	r := h.do("PATCH", path, `{"name":"New","tags":["b","a"],"sequence_id":0,"double_optin":true,"consent_text":"","redirect_url":"https://example.com/ok","allowed_origins":["https://example.com"]}`,
		"If-Match", `"v1"`).wantStatus(t, http.StatusOK)
	r.decode(t, &got)
	if got.Name != "New" || strings.Join(got.Tags, ",") != "b,a" || got.SequenceID != nil || !got.DoubleOptin ||
		got.ConsentText != forms.DefaultConsentText || got.RedirectURL != "https://example.com/ok" || got.Version != 2 || got.PublicID != f.PublicID {
		t.Fatalf("updated = %+v", got)
	}
	if r.header.Get("ETag") != `"v2"` {
		t.Fatalf("etag = %s", r.header.Get("ETag"))
	}
	// A no-op keeps the version and writes no audit record.
	h.do("PATCH", path, `{"name":"New"}`).wantStatus(t, http.StatusOK).decode(t, &got)
	if got.Version != 2 {
		t.Fatalf("no-op bumped version to %d", got.Version)
	}
	if n := h.count(`SELECT count(*) FROM audit_log WHERE operation = 'form.update'`); n != 1 {
		t.Fatalf("update audit rows = %d", n)
	}
	h.do("PATCH", path, `{"sequence_id":42}`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "sequence_id")
	h.do("PATCH", "/forms/999", `{"name":"x"}`).wantError(t, http.StatusNotFound, api.CodeNotFound, "")
}

func TestDeleteForm(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"x"}`)
	path := fmt.Sprintf("/forms/%d", f.ID)
	h.do("DELETE", path, "", "If-Match", `"v2"`).wantError(t, http.StatusPreconditionFailed, api.CodePreconditionFailed, "")
	h.do("DELETE", path+"?dry_run=true", "").wantStatus(t, http.StatusNoContent)
	h.do("GET", path, "").wantStatus(t, http.StatusOK)
	h.do("DELETE", path, "", "If-Match", `"v1"`).wantStatus(t, http.StatusNoContent)
	h.do("GET", path, "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	h.publicReq("GET", "/f/"+f.PublicID, "", "").wantStatus(t, http.StatusNotFound)
	if n := h.count(`SELECT count(*) FROM audit_log WHERE operation = 'form.delete'`); n != 1 {
		t.Fatalf("delete audit rows = %d", n)
	}
}

func TestCreateFormDryRun(t *testing.T) {
	h := newHarness(t)
	var f forms.Form
	h.do("POST", "/forms?dry_run=true", `{"name":"x","tags":["new-tag"]}`).wantStatus(t, http.StatusOK).decode(t, &f)
	if f.Name != "x" || !publicIDPattern.MatchString(f.PublicID) || strings.Join(f.Tags, ",") != "new-tag" {
		t.Fatalf("dry run = %+v", f)
	}
	for _, q := range []string{`SELECT count(*) FROM forms`, `SELECT count(*) FROM tags`, `SELECT count(*) FROM audit_log`} {
		if n := h.count(q); n != 0 {
			t.Fatalf("%s = %d after dry run", q, n)
		}
	}
}

func TestListFormsPagination(t *testing.T) {
	h := newHarness(t)
	for i := range 5 {
		h.createForm(fmt.Sprintf(`{"name":"form %d"}`, i))
	}
	var names []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("too many pages")
		}
		var page api.ListBody[forms.Form]
		h.do("GET", "/forms?limit=2&cursor="+cursor, "").wantStatus(t, http.StatusOK).decode(t, &page)
		for _, f := range page.Items {
			names = append(names, f.Name)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if strings.Join(names, ",") != "form 0,form 1,form 2,form 3,form 4" {
		t.Fatalf("names = %v", names)
	}
	h.do("GET", "/forms?cursor=nope", "").wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "query.cursor")
}

func TestSnippet(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"x","fields":[{"key":"first_name","label":"Your <name>","required":true},{"key":"attr.company"}],"consent_text":"We email & track clicks."}`)
	var sn forms.Snippet
	h.do("GET", fmt.Sprintf("/forms/%d/snippet", f.ID), "").wantStatus(t, http.StatusOK).decode(t, &sn)
	if sn.HostedURL != f.HostedURL || sn.Endpoint != f.Endpoint {
		t.Fatalf("snippet urls = %+v", sn)
	}
	for _, want := range []string{
		`action="` + f.Endpoint + `"`, `method="post"`, `name="email"`, `type="email"`, `name="first_name"`, `name="attr.company"`,
		`Your &lt;name&gt;`, `name="website"`, `tabindex="-1"`, `We email &amp; track clicks.`, `<button type="submit">`,
	} {
		if !strings.Contains(sn.HTML, want) {
			t.Errorf("snippet lacks %q:\n%s", want, sn.HTML)
		}
	}
	if strings.Contains(sn.HTML, "<script") {
		t.Fatal("snippet must not contain scripts")
	}
}

func TestListSubmissions(t *testing.T) {
	h := newHarness(t)
	f := h.createForm(`{"name":"x"}`)
	for i := range 3 {
		h.submit(f.PublicID, vals("email", fmt.Sprintf("s%d@example.com", i))).wantStatus(t, http.StatusSeeOther)
	}
	h.submit(f.PublicID, vals("email", "bot@example.com", "website", "http://spam")).wantStatus(t, http.StatusSeeOther)
	var page api.ListBody[forms.Submission]
	h.do("GET", fmt.Sprintf("/forms/%d/submissions?limit=3", f.ID), "").wantStatus(t, http.StatusOK).decode(t, &page)
	if len(page.Items) != 3 || page.NextCursor == "" {
		t.Fatalf("page 1 = %+v", page)
	}
	if page.Items[0].Outcome != forms.OutcomeSpam || page.Items[0].ContactID != nil || page.Items[0].Email != "" {
		t.Fatalf("newest = %+v", page.Items[0])
	}
	if page.Items[1].Outcome != forms.OutcomeCreated || page.Items[1].Email != "s2@example.com" || page.Items[1].ContactID == nil {
		t.Fatalf("second = %+v", page.Items[1])
	}
	var rest api.ListBody[forms.Submission]
	h.do("GET", fmt.Sprintf("/forms/%d/submissions?limit=3&cursor=%s", f.ID, page.NextCursor), "").wantStatus(t, http.StatusOK).decode(t, &rest)
	if len(rest.Items) != 1 || rest.Items[0].Email != "s0@example.com" || rest.NextCursor != "" {
		t.Fatalf("page 2 = %+v", rest)
	}
}
