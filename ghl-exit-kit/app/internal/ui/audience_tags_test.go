package ui_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

func TestAudienceTags(t *testing.T) {
	hs := audHarness(t)
	c := hs.session()

	c.get("/admin/tags").want(t, http.StatusOK).contains(t, "<h1>Tags</h1>", "No tags yet", `aria-current="page" href="/admin/tags"`)
	c.postNoCSRF("/admin/tags", url.Values{"name": {"news"}}).want(t, http.StatusForbidden)
	c.post("/admin/tags", url.Values{"name": {"news"}}).wantRedirect(t, "/admin/tags")
	c.get("/admin/tags").want(t, http.StatusOK).contains(t, "Tag news created.", ">news</a>")
	c.post("/admin/tags", url.Values{"name": {"NEWS"}}).want(t, http.StatusConflict).contains(t, `class="form-error"`)
	c.post("/admin/tags", url.Values{"name": {""}}).want(t, http.StatusUnprocessableEntity).contains(t, `id="f-name-error"`)

	ct := testutil.InsertContact(t, hs.app.DB, "t@example.org", core.StatusActive, "old", "keep")
	var oldID, keepID, newsID int64
	for name, dst := range map[string]*int64{"old": &oldID, "keep": &keepID, "news": &newsID} {
		if err := hs.app.DB.Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	c.get("/admin/tags?q=ol").want(t, http.StatusOK).contains(t, ">old</a>").lacks(t, ">keep</a>")

	// Rename.
	page := fmt.Sprintf("/admin/tags/%d", oldID)
	c.get(page).want(t, http.StatusOK).contains(t, "<h1>Tag old</h1>", "1 contact has this tag.", "Review merge")
	c.postNoCSRF(page+"/rename", url.Values{"name": {"older"}}).want(t, http.StatusForbidden)
	c.post(page+"/rename", url.Values{"name": {"keep"}}).want(t, http.StatusConflict)
	c.post(page+"/rename", url.Values{"name": {"older"}}).wantRedirect(t, page)
	c.get(page).want(t, http.StatusOK).contains(t, "Tag renamed to older.")

	// Merge: a confirmation first, then the merge.
	c.post(page+"/merge", url.Values{"into": {"nope"}}).want(t, http.StatusUnprocessableEntity).contains(t, "no tag has this name")
	c.post(page+"/merge", url.Values{"into": {"older"}}).want(t, http.StatusUnprocessableEntity).contains(t, "must be a different tag")
	c.post(page+"/merge", url.Values{"into": {"news"}}).want(t, http.StatusOK).
		contains(t, "Merge &#34;older&#34; into &#34;news&#34;?", "1 contact with older get news", `name="confirm" value="yes"`)
	if n := hs.count(`SELECT COUNT(*) FROM tags WHERE id = ?`, oldID); n != 1 {
		t.Fatal("merged before the confirmation")
	}
	c.post(page+"/merge", url.Values{"into": {"news"}, "confirm": {"yes"}}).wantRedirect(t, fmt.Sprintf("/admin/tags/%d", newsID))
	if n := hs.count(`SELECT COUNT(*) FROM tags WHERE id = ?`, oldID); n != 0 {
		t.Fatal("source tag kept")
	}
	if n := hs.count(`SELECT COUNT(*) FROM contact_tags WHERE contact_id = ? AND tag_id = ?`, ct.ID, newsID); n != 1 {
		t.Fatal("contact not moved")
	}
	c.get(fmt.Sprintf("/admin/tags/%d", newsID)).want(t, http.StatusOK).contains(t, "Merged older into news: 1 contact moved.")

	// Delete: a confirmation with the count, then the delete.
	keep := fmt.Sprintf("/admin/tags/%d", keepID)
	c.postNoCSRF(keep+"/delete", nil).want(t, http.StatusForbidden)
	c.post(keep+"/delete", nil).want(t, http.StatusOK).contains(t, "Delete tag &#34;keep&#34;?", "1 contact loses this tag.")
	c.post(keep+"/delete", url.Values{"confirm": {"yes"}}).wantRedirect(t, "/admin/tags")
	if n := hs.count(`SELECT COUNT(*) FROM tags WHERE id = ?`, keepID); n != 0 {
		t.Fatal("not deleted")
	}
	c.get("/admin/tags/999").want(t, http.StatusNotFound)
}

func TestAudienceTagMergeForce(t *testing.T) {
	hs := audHarness(t)
	c := hs.session()
	ct := testutil.InsertContact(t, hs.app.DB, "f@example.org", core.StatusActive, "lead")
	exitID := testutil.InsertTag(t, hs.app.DB, "done")
	leadID := int64(0)
	if err := hs.app.DB.Read.QueryRow(`SELECT id FROM tags WHERE name = 'lead'`).Scan(&leadID); err != nil {
		t.Fatal(err)
	}
	// A sequence whose exit tag is "done", with the contact enrolled:
	// merging "lead" into "done" would end its enrollment.
	seq := testutil.InsertSequence(t, hs.app.DB, "Welcome")
	testutil.Exec(t, hs.app.DB, `UPDATE sequences SET exit_tag_ids = ? WHERE id = ?`, fmt.Sprintf("[%d]", exitID), seq)
	step := testutil.InsertStep(t, hs.app.DB, seq, 1, 0, testutil.InsertEmail(t, hs.app.DB, "Hi", "Hello"))
	testutil.InsertEnrollment(t, hs.app.DB, seq, ct.ID, step)

	page := fmt.Sprintf("/admin/tags/%d", leadID)
	c.post(page+"/merge", url.Values{"into": {"done"}, "confirm": {"yes"}}).want(t, http.StatusOK).
		contains(t, "Merge anyway?", "Welcome", `name="force" value="1"`)
	if n := hs.count(`SELECT COUNT(*) FROM tags WHERE id = ?`, leadID); n != 1 {
		t.Fatal("merged without force")
	}
	c.post(page+"/merge", url.Values{"into": {"done"}, "confirm": {"yes"}, "force": {"1"}}).
		wantRedirect(t, fmt.Sprintf("/admin/tags/%d", exitID))
	if n := hs.count(`SELECT COUNT(*) FROM tags WHERE id = ?`, leadID); n != 0 {
		t.Fatal("not merged with force")
	}
}
