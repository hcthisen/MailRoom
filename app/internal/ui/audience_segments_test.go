package ui_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/segments"
	"mailroom/internal/testutil"
)

// audSegmentForm is a builder form: customers who are not VIPs, or
// anyone named Ann (nested group).
func audSegmentForm(name string) url.Values {
	return url.Values{
		"name":                               {name},
		"filter.match":                       {"any"},
		"filter.rules[0].kind":               {"group"},
		"filter.rules[0].match":              {"all"},
		"filter.rules[0].rules[0].kind":      {"condition"},
		"filter.rules[0].rules[0].field":     {"tag"},
		"filter.rules[0].rules[0].op":        {"has"},
		"filter.rules[0].rules[0].value":     {"customer"},
		"filter.rules[0].rules[1].kind":      {"condition"},
		"filter.rules[0].rules[1].field":     {"tag"},
		"filter.rules[0].rules[1].op":        {"not_has"},
		"filter.rules[0].rules[1].value":     {"vip"},
		"filter.rules[1].kind":               {"condition"},
		"filter.rules[1].field":              {"first_name"},
		"filter.rules[1].op":                 {"eq"},
		"filter.rules[1].value":              {"Ann"},
		"filter.rules[1].rules[9].ignored":   {"x"}, // not a builder input: ignored
		"filter.rules[2].field_without_kind": {"x"},
	}
}

func audSegmentFixtures(t *testing.T, hs *harness) {
	t.Helper()
	testutil.InsertContact(t, hs.app.DB, "c1@example.org", core.StatusActive, "customer")
	testutil.InsertContact(t, hs.app.DB, "c2@example.org", core.StatusUnsubscribed, "customer")
	testutil.InsertContact(t, hs.app.DB, "vip@example.org", core.StatusActive, "customer", "vip")
	testutil.InsertContact(t, hs.app.DB, "other@example.org", core.StatusActive)
	testutil.Exec(t, hs.app.DB, `UPDATE contacts SET first_name = 'Ann' WHERE email = 'other@example.org'`)
}

func TestAudienceSegmentBuilder(t *testing.T) {
	hs := audHarness(t)
	audSegmentFixtures(t, hs)
	c := hs.session()

	c.get("/admin/segments").want(t, http.StatusOK).contains(t, "<h1>Segments</h1>", "No segments yet")
	c.get("/admin/segments/new").want(t, http.StatusOK).
		contains(t, `hx-post="/admin/segments/preview"`, "No rules yet: every contact matches.", `value="add-condition:filter"`)

	// Structural edits through htmx: add a condition, then a group.
	r := c.htmx(http.MethodPost, "/admin/segments/builder", url.Values{"edit": {"add-condition:filter"}}).want(t, http.StatusOK)
	r.contains(t, `name="filter.rules[0].kind" value="condition"`, `name="filter.rules[0].field"`, `value="remove:filter.rules[0]"`,
		`hx-swap-oob="innerHTML"`).lacks(t, "<html")
	r = c.htmx(http.MethodPost, "/admin/segments/builder", url.Values{
		"filter.rules[0].kind": {"condition"}, "filter.rules[0].field": {"status"}, "filter.rules[0].op": {"is"}, "filter.rules[0].value": {"active"},
		"edit": {"add-group:filter"}}).want(t, http.StatusOK)
	r.contains(t, `name="filter.rules[1].kind" value="group"`, `name="filter.rules[1].rules[0].field"`, "Group 2",
		`<option value="active" selected>`, `class="preview-error"`, "Rule 2.1, value: ")
	// Changing the field picks an operator of the new field.
	r = c.htmx(http.MethodPost, "/admin/segments/builder", url.Values{
		"filter.rules[0].kind": {"condition"}, "filter.rules[0].field": {"created_at"}, "filter.rules[0].op": {"has"}}).want(t, http.StatusOK)
	r.contains(t, `<option value="within_days" selected>`, `name="filter.rules[0].days"`)
	// Remove renumbers the rest.
	r = c.htmx(http.MethodPost, "/admin/segments/builder", url.Values{
		"filter.rules[0].kind": {"condition"}, "filter.rules[0].field": {"tag"}, "filter.rules[0].op": {"has"}, "filter.rules[0].value": {"a"},
		"filter.rules[1].kind": {"condition"}, "filter.rules[1].field": {"tag"}, "filter.rules[1].op": {"has"}, "filter.rules[1].value": {"b"},
		"edit": {"remove:filter.rules[0]"}}).want(t, http.StatusOK)
	r.contains(t, `name="filter.rules[0].value" type="text" value="b"`).lacks(t, `name="filter.rules[1].kind"`)
	c.postNoCSRF("/admin/segments/builder", url.Values{"edit": {"add-condition:filter"}}).want(t, http.StatusForbidden)

	// Live preview: counts and a sample.
	form := audSegmentForm("Customers or Ann")
	p := c.htmx(http.MethodPost, "/admin/segments/preview", form).want(t, http.StatusOK)
	p.contains(t, "Matching now", `<p class="value">3</p>`, `<p class="value">2</p>`, "c1@example.org", "other@example.org").
		lacks(t, "vip@example.org", "<html")
	// A preview error names the rule.
	p = c.htmx(http.MethodPost, "/admin/segments/preview", url.Values{
		"filter.rules[0].kind": {"condition"}, "filter.rules[0].field": {"created_at"}, "filter.rules[0].op": {"within_days"}, "filter.rules[0].days": {"x"}})
	p.want(t, http.StatusOK).contains(t, `class="preview-error"`, "Rule 1, days: must be a whole number")
	p = c.htmx(http.MethodPost, "/admin/segments/preview", url.Values{
		"filter.rules[0].kind": {"condition"}, "filter.rules[0].field": {"tag"}, "filter.rules[0].op": {"has"}, "filter.rules[0].value": {"ghost"}})
	p.want(t, http.StatusOK).contains(t, "No tag is named ghost", "No contact matches")

	// Without JavaScript the builder buttons post to the form's own action.
	c.post("/admin/segments", url.Values{"name": {"x"}, "edit": {"add-condition:filter"}}).want(t, http.StatusOK).
		contains(t, `name="filter.rules[0].field"`, `value="x"`)
	if n := hs.count(`SELECT COUNT(*) FROM segments`); n != 0 {
		t.Fatal("an edit saved the segment")
	}
}

func TestAudienceSegmentSave(t *testing.T) {
	hs := audHarness(t)
	audSegmentFixtures(t, hs)
	c := hs.session()

	// Validation errors land next to their input.
	bad := audSegmentForm("Bad")
	bad.Set("filter.rules[1].field", "status")
	bad.Set("filter.rules[1].op", "is")
	bad.Set("filter.rules[1].value", "sleeping")
	c.post("/admin/segments", bad).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-filter-rules-1-value-error"`, "Please correct the highlighted field.")

	form := audSegmentForm("Customers or Ann")
	c.postNoCSRF("/admin/segments", form).want(t, http.StatusForbidden)
	r := c.post("/admin/segments", form).want(t, http.StatusSeeOther)
	id := audLocationID(t, r, "/admin/segments/")
	s, err := hs.app.Segments.Get(hs.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Filter.Match != segments.MatchAny || len(s.Filter.Rules) != 2 || len(s.Filter.Rules[0].Rules) != 2 {
		t.Fatalf("filter = %+v", s.Filter)
	}
	page := fmt.Sprintf("/admin/segments/%d", id)
	c.get(page).want(t, http.StatusOK).contains(t, "Segment Customers or Ann saved.", "<h1>Customers or Ann</h1>",
		"(has tag customer and doesn&#39;t have tag vip) or first name is Ann", "c1@example.org", "other@example.org",
		"Filter as JSON").lacks(t, "vip@example.org")
	c.post("/admin/segments", form).want(t, http.StatusConflict).contains(t, `class="form-error"`)
	c.get(page+"?status=active").want(t, http.StatusOK).contains(t, "c1@example.org").lacks(t, "c2@example.org")
	c.get(page+"/count").want(t, http.StatusOK).contains(t, "<strong>2</strong> active", "of 3")
	c.get("/admin/segments").want(t, http.StatusOK).contains(t, "Customers or Ann", fmt.Sprintf(`hx-get="/admin/segments/%d/count"`, id))
	c.get("/admin/contacts?segment_id="+fmt.Sprint(id)).want(t, http.StatusOK).contains(t, "c1@example.org").lacks(t, "vip@example.org")

	// Edit: the saved filter comes back into the builder, and saving
	// replaces it.
	edit := c.get(page+"/edit").want(t, http.StatusOK).
		contains(t, `name="filter.rules[0].rules[1].value" type="text" value="vip"`, `name="filter.rules[1].kind" value="condition"`)
	ver := edit.field(t, "version")
	upd := url.Values{"name": {"Customers"}, "version": {ver}, "filter.match": {"all"},
		"filter.rules[0].kind": {"condition"}, "filter.rules[0].field": {"attr"}, "filter.rules[0].key": {"seats"},
		"filter.rules[0].op": {"eq"}, "filter.rules[0].value": {"5"}, "filter.rules[0].value_type": {"number"}}
	c.postNoCSRF(page+"/edit", upd).want(t, http.StatusForbidden)
	c.post(page+"/edit", upd).wantRedirect(t, page)
	s, _ = hs.app.Segments.Get(hs.ctx, id)
	if s.Name != "Customers" || s.Filter.Rules[0].Value != float64(5) {
		t.Fatalf("after update: %+v", s)
	}
	c.post(page+"/edit", upd).want(t, http.StatusPreconditionFailed).contains(t, "Someone else changed this")
	c.get(page+"/edit").want(t, http.StatusOK).contains(t, `<option value="number" selected>`, `value="5"`)

	// Delete: the dry run first, then the delete.
	c.postNoCSRF(page+"/delete", nil).want(t, http.StatusForbidden)
	c.post(page+"/delete", nil).want(t, http.StatusOK).contains(t, "Delete segment &#34;Customers&#34;?", "Delete segment")
	c.post(page+"/delete", url.Values{"confirm": {"yes"}}).wantRedirect(t, "/admin/segments")
	if n := hs.count(`SELECT COUNT(*) FROM segments`); n != 0 {
		t.Fatal("not deleted")
	}
	c.get(page).want(t, http.StatusNotFound)
}

func TestAudienceSegmentBulkTagAndDeleteRefused(t *testing.T) {
	hs := audHarness(t)
	hs.audStartJobs()
	audSegmentFixtures(t, hs)
	c := hs.session()
	r := c.post("/admin/segments", audSegmentForm("Seg")).want(t, http.StatusSeeOther)
	id := audLocationID(t, r, "/admin/segments/")
	page := fmt.Sprintf("/admin/segments/%d", id)

	// Tag every member: confirmation with the count, then a job.
	form := url.Values{"segment_id": {fmt.Sprint(id)}, "action": {"add"}, "tag": {"seg-tag"}, "back": {page}}
	c.post("/admin/contacts/bulk-tag", form).want(t, http.StatusOK).contains(t, "3 contacts in the segment")
	form.Set("confirm", "yes")
	c.post("/admin/contacts/bulk-tag", form).wantRedirect(t, page)
	var jobID int64
	if err := hs.app.DB.Read.QueryRow(`SELECT id FROM jobs WHERE type = 'tags.bulk'`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	hs.audWaitJob(jobID)
	if n := hs.count(`SELECT COUNT(*) FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id WHERE t.name = 'seg-tag'`); n != 3 {
		t.Fatalf("tagged = %d", n)
	}

	// A draft broadcast to the segment blocks its deletion (dry run).
	b := testutil.InsertBroadcast(t, hs.app.DB, "News", testutil.InsertEmail(t, hs.app.DB, "News", "Hi"))
	testutil.Exec(t, hs.app.DB, `UPDATE broadcasts SET audience = json_object('type', 'segment', 'segment_id', ?, 'segment_name', 'Seg') WHERE id = ?`, id, b)
	c.post(page+"/delete", nil).wantRedirect(t, page)
	c.get(page).want(t, http.StatusOK).contains(t, "unfinished broadcast")
	if n := hs.count(`SELECT COUNT(*) FROM segments`); n != 1 {
		t.Fatal("deleted")
	}
}
