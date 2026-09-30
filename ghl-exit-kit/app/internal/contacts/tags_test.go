package contacts_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/hooks"
	"mailroom/internal/jobs"
	"mailroom/internal/testutil"
)

func TestTagsCRUD(t *testing.T) {
	hs := newHarness(t)
	var vip contacts.Tag
	hs.do("POST", "/tags", `{"name":" VIP "}`).wantStatus(t, http.StatusCreated).decode(t, &vip)
	if vip.Name != "VIP" || vip.Contacts != 0 {
		t.Fatalf("created = %+v", vip)
	}
	hs.do("POST", "/tags", `{"name":"vip"}`).wantError(t, http.StatusConflict, api.CodeConflict, "")
	hs.do("POST", "/tags", `{"name":"`+strings.Repeat("x", 65)+`"}`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "name")
	hs.do("POST", "/tags", `{"name":"   "}`).wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "name")

	a := hs.createContact(`{"email":"a@example.com","tags":["vip","lead"]}`)
	hs.createContact(`{"email":"b@example.com","tags":["lead"]}`)

	var list api.ListBody[contacts.Tag]
	hs.doAs(readToken, "GET", "/tags", "").wantStatus(t, http.StatusOK).decode(t, &list)
	got := map[string]int{}
	for _, tg := range list.Items {
		got[tg.Name] = tg.Contacts
	}
	if len(got) != 2 || got["VIP"] != 1 || got["lead"] != 2 {
		t.Fatalf("tags = %v", got)
	}
	hs.do("GET", "/tags?q=LE", "").decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].Name != "lead" {
		t.Fatalf("filtered = %+v", list.Items)
	}
	hs.do("GET", "/tags?limit=1", "").decode(t, &list)
	if len(list.Items) != 1 || list.NextCursor == "" {
		t.Fatalf("page = %+v", list)
	}

	// Rename; a clash is a conflict; a case change of itself is fine.
	var renamed contacts.Tag
	hs.do("PATCH", fmt.Sprintf("/tags/%d", vip.ID), `{"name":"Very Important"}`).wantStatus(t, http.StatusOK).decode(t, &renamed)
	if renamed.Name != "Very Important" || renamed.Contacts != 1 {
		t.Fatalf("renamed = %+v", renamed)
	}
	hs.do("PATCH", fmt.Sprintf("/tags/%d", vip.ID), `{"name":"LEAD"}`).wantError(t, http.StatusConflict, api.CodeConflict, "")
	hs.do("PATCH", fmt.Sprintf("/tags/%d", vip.ID), `{"name":"very important"}`).wantStatus(t, http.StatusOK)
	hs.do("PATCH", "/tags/999", `{"name":"x"}`).wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	c, _ := hs.svc.Get(t.Context(), a.ID)
	if strings.Join(c.Tags, ",") != "lead,very important" || c.Version != 3 {
		t.Fatalf("contact after renames = %+v", c)
	}

	// Delete removes the memberships and bumps the contacts' versions.
	hs.do("DELETE", fmt.Sprintf("/tags/%d", vip.ID), "").wantStatus(t, http.StatusNoContent)
	c, _ = hs.svc.Get(t.Context(), a.ID)
	if strings.Join(c.Tags, ",") != "lead" || c.Version != 4 {
		t.Fatalf("contact after delete = %+v", c)
	}
	hs.do("DELETE", fmt.Sprintf("/tags/%d", vip.ID), "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	hs.do("GET", fmt.Sprintf("/tags/%d", vip.ID), "").wantError(t, http.StatusNotFound, api.CodeNotFound, "")
	for _, op := range []string{"tag.create", "tag.rename", "tag.delete"} {
		if hs.count(`SELECT count(*) FROM audit_log WHERE operation = ?`, op) == 0 {
			t.Errorf("no %s audit entry", op)
		}
	}
}

func TestTagMergeAndUsage(t *testing.T) {
	hs := newHarness(t)
	a := hs.createContact(`{"email":"a@example.com","tags":["old","new"]}`)
	b := hs.createContact(`{"email":"b@example.com","tags":["old"]}`)
	oldID := testutil.InsertTag(t, hs.d, "old")
	newID := testutil.InsertTag(t, hs.d, "new")
	otherID := testutil.InsertTag(t, hs.d, "other")
	seqWith := func(name string, trigger any, exits string, exitAdd any) int64 {
		id := testutil.InsertSequence(t, hs.d, name)
		testutil.Exec(t, hs.d, `UPDATE sequences SET trigger_tag_id = ?, exit_tag_ids = ?, exit_add_tag_id = ? WHERE id = ?`,
			trigger, exits, exitAdd, id)
		return id
	}
	drip := seqWith("Drip", oldID, fmt.Sprintf("[%d]", otherID), oldID)
	exitsSeq := seqWith("Exits", nil, fmt.Sprintf("[%d,%d,%d]", otherID, oldID, newID), nil)
	clash := seqWith("Clash", oldID, fmt.Sprintf("[%d]", newID), nil)
	nurture := seqWith("Nurture", nil, fmt.Sprintf("[%d]", newID), nil)
	emailID := testutil.InsertEmail(t, hs.d, "s", "b")
	testutil.InsertEnrollment(t, hs.d, nurture, b.ID, testutil.InsertStep(t, hs.d, nurture, 1, 60, emailID))
	testutil.Exec(t, hs.d, `INSERT INTO forms (public_id, name, tag_ids, consent_text, created_at, updated_at)
		VALUES ('abc', 'Signup', ?, 'ok', 'x', 'x')`, fmt.Sprintf("[%d]", oldID))
	mergePath := fmt.Sprintf("/tags/%d/merge", oldID)

	// In use: can't be deleted.
	e := hs.do("DELETE", fmt.Sprintf("/tags/%d", oldID), "").wantError(t, http.StatusConflict, api.CodeConflict, "")
	if !strings.Contains(e.Message, `sequence "`) {
		t.Fatalf("message = %q", e.Message)
	}
	hs.do("POST", mergePath, fmt.Sprintf(`{"into_tag_id":%d}`, oldID)).
		wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "into_tag_id")
	hs.do("POST", mergePath, `{"into_tag_id":999}`).
		wantError(t, http.StatusUnprocessableEntity, api.CodeValidation, "into_tag_id")

	// Clash's trigger (old) would become its own exit tag (new): always
	// refused, force or not.
	for _, body := range []string{`{"into_tag_id":%d}`, `{"into_tag_id":%d,"force":true}`} {
		e = hs.do("POST", mergePath, fmt.Sprintf(body, newID)).wantError(t, http.StatusConflict, api.CodeConflict, "")
		if !strings.Contains(e.Message, `sequence "Clash"`) {
			t.Fatalf("clash message = %q", e.Message)
		}
	}
	testutil.Exec(t, hs.d, `UPDATE sequences SET exit_tag_ids = '[]' WHERE id = ?`, clash)

	// b is enrolled in Nurture (exit tag new) and would gain new: refused
	// without force, and nothing changed.
	e = hs.do("POST", mergePath, fmt.Sprintf(`{"into_tag_id":%d}`, newID)).wantError(t, http.StatusConflict, api.CodeConflict, "")
	if !strings.Contains(e.Message, `1 actively enrolled`) || !strings.Contains(e.Message, `sequence "Nurture"`) {
		t.Fatalf("exit message = %q", e.Message)
	}
	if hs.count(`SELECT count(*) FROM tags WHERE id = ?`, oldID) != 1 || hs.count(`SELECT count(*) FROM sequences WHERE version > 1`) != 0 {
		t.Fatal("refused merge changed data")
	}

	events := len(hs.tagEvents())
	var res contacts.MergeResult
	hs.do("POST", mergePath, fmt.Sprintf(`{"into_tag_id":%d,"force":true}`, newID)).wantStatus(t, http.StatusOK).decode(t, &res)
	if res.Moved != 1 || res.Tag.ID != newID || res.Tag.Contacts != 2 || res.ExitingEnrollments != 1 {
		t.Fatalf("merge = %+v", res)
	}
	if len(hs.tagEvents()) != events {
		t.Fatal("merge must not fire TagAdded")
	}
	for _, id := range []int64{a.ID, b.ID} {
		c, _ := hs.svc.Get(t.Context(), id)
		if strings.Join(c.Tags, ",") != "new" || c.Version != 2 {
			t.Fatalf("contact %d = %+v", id, c)
		}
	}
	type seqRow struct {
		trigger, exitAdd sql.NullInt64
		exits            string
		version          int
	}
	read := func(id int64) seqRow {
		var r seqRow
		if err := hs.d.Read.QueryRow(`SELECT trigger_tag_id, exit_add_tag_id, exit_tag_ids, version FROM sequences WHERE id = ?`, id).
			Scan(&r.trigger, &r.exitAdd, &r.exits, &r.version); err != nil {
			t.Fatal(err)
		}
		return r
	}
	want := map[int64]seqRow{
		drip:     {trigger: sql.NullInt64{Int64: newID, Valid: true}, exitAdd: sql.NullInt64{Int64: newID, Valid: true}, exits: fmt.Sprintf("[%d]", otherID), version: 2},
		exitsSeq: {exits: fmt.Sprintf("[%d,%d]", otherID, newID), version: 2},
		clash:    {trigger: sql.NullInt64{Int64: newID, Valid: true}, exits: "[]", version: 2},
		nurture:  {exits: fmt.Sprintf("[%d]", newID), version: 1},
	}
	for id, w := range want {
		if got := read(id); got != w {
			t.Errorf("sequence %d = %+v, want %+v", id, got, w)
		}
	}
	if n := hs.count(`SELECT count(*) FROM forms WHERE tag_ids = ?`, fmt.Sprintf("[%d]", newID)); n != 1 {
		t.Fatal("form tags not repointed")
	}
	if n := hs.count(`SELECT count(*) FROM tags WHERE id = ?`, oldID); n != 0 {
		t.Fatal("source tag not deleted")
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'tag.merge' AND changes LIKE '%"force":true%'`); n != 1 {
		t.Fatal("forced merge not in the audit log")
	}
}

func TestBulkTag(t *testing.T) {
	hs := newHarness(t)
	hs.startJobs()
	// 1,203 contacts: three batches.
	const n = 1203
	testutil.Exec(t, hs.d, `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < ?1)
		INSERT INTO contacts (email, status, status_changed_at, created_at, updated_at)
		SELECT 'c' || i || '@example.com', CASE WHEN i % 3 = 0 THEN 'unsubscribed' ELSE 'active' END, ?2, ?2, ?2 FROM seq`,
		n, db.FormatTime(testutil.Epoch))
	active := hs.count(`SELECT count(*) FROM contacts WHERE status = 'active'`)
	hs.do("POST", "/contacts/1/tags", `{"tags":["promo"]}`).wantStatus(t, http.StatusOK)
	before := len(hs.tagEvents())

	// Dry run: only the count, no job.
	var dry contacts.BulkTagResult
	hs.do("POST", "/tags/bulk?dry_run=true", fmt.Sprintf(`{"tag":"promo","action":"add","segment_id":%d}`, segActive)).
		wantStatus(t, http.StatusOK).decode(t, &dry)
	if dry.Matched != active || dry.Job != nil || hs.count(`SELECT count(*) FROM jobs`) != 0 {
		t.Fatalf("dry run = %+v", dry)
	}

	var acc contacts.BulkTagResult
	hs.do("POST", "/tags/bulk", fmt.Sprintf(`{"tag":"Promo","action":"add","segment_id":%d}`, segActive)).
		wantStatus(t, http.StatusAccepted).decode(t, &acc)
	if acc.Job == nil || acc.Job.Type != contacts.JobBulkTag || acc.Matched != active {
		t.Fatalf("accepted = %+v", acc)
	}
	if hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'tag.bulk' AND target_id = ?`, fmt.Sprint(acc.Job.ID)) != 1 {
		t.Fatal("bulk request not audited")
	}
	j := hs.waitJob(acc.Job.ID)
	var res contacts.BulkTagJobResult
	if err := json.Unmarshal(j.Result, &res); err != nil || j.Status != jobs.StatusSucceeded || j.Progress != 1 {
		t.Fatalf("job = %+v (%v)", j, err)
	}
	if res.Matched != active || res.Changed != active-1 || res.Unchanged != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := hs.count(`SELECT count(*) FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id WHERE t.name = 'promo'`); got != active {
		t.Fatalf("tagged = %d, want %d", got, active)
	}
	if got := len(hs.tagEvents()) - before; got != active-1 {
		t.Fatalf("TagAdded events = %d, want %d", got, active-1)
	}
	if hs.count(`SELECT count(*) FROM contact_tags WHERE source = 'api'`) != active {
		t.Fatal("bulk tags must be recorded with source api")
	}

	// Running the same request again is a no-op (the job is idempotent,
	// which makes it safe to resume after a restart).
	var again contacts.BulkTagResult
	hs.do("POST", "/tags/bulk", fmt.Sprintf(`{"tag":"promo","action":"add","segment_id":%d}`, segActive)).
		wantStatus(t, http.StatusAccepted).decode(t, &again)
	var rerun contacts.BulkTagJobResult
	_ = json.Unmarshal(hs.waitJob(again.Job.ID).Result, &rerun)
	if rerun.Changed != 0 || rerun.Unchanged != active {
		t.Fatalf("rerun = %+v", rerun)
	}

	// The job endpoint reports it.
	var polled jobs.Job
	hs.do("GET", fmt.Sprintf("/jobs/%d", acc.Job.ID), "").wantStatus(t, http.StatusOK).decode(t, &polled)
	if polled.Status != jobs.StatusSucceeded {
		t.Fatalf("polled = %+v", polled)
	}

	// Remove by IDs, with missing and duplicate IDs.
	hs.do("POST", "/tags/bulk", `{"tag":"promo","action":"remove","contact_ids":[1,2,3,2,99999]}`).
		wantStatus(t, http.StatusAccepted).decode(t, &acc)
	res = contacts.BulkTagJobResult{}
	j = hs.waitJob(acc.Job.ID)
	_ = json.Unmarshal(j.Result, &res)
	// Contact 3 is unsubscribed and never had the tag.
	if res.Matched != 3 || res.Changed != 2 || res.Unchanged != 1 || res.Missing != 1 {
		t.Fatalf("remove result = %+v", res)
	}
	if hs.count(`SELECT count(*) FROM contact_tags WHERE contact_id IN (1, 2)`) != 0 {
		t.Fatal("tags not removed")
	}
}

func TestBulkTagValidation(t *testing.T) {
	hs := newHarness(t)
	tests := []struct {
		name, body string
		status     int
		code       string
		field      string
	}{
		{"neither", `{"tag":"x","action":"add"}`, 422, api.CodeValidation, "contact_ids"},
		{"both", `{"tag":"x","action":"add","contact_ids":[1],"segment_id":1}`, 422, api.CodeValidation, "contact_ids"},
		{"bad action", `{"tag":"x","action":"toggle","contact_ids":[1]}`, 422, api.CodeValidation, "action"},
		{"bad tag", `{"tag":"  ","action":"add","contact_ids":[1]}`, 422, api.CodeValidation, "tag"},
		{"bad id", `{"tag":"x","action":"add","contact_ids":[0]}`, 422, api.CodeValidation, "contact_ids"},
		{"unknown segment", `{"tag":"x","action":"add","segment_id":42}`, 404, api.CodeNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs.do("POST", "/tags/bulk", tt.body).wantError(t, tt.status, tt.code, tt.field)
		})
	}
	if hs.count(`SELECT count(*) FROM jobs`) != 0 {
		t.Fatal("invalid requests created jobs")
	}
}

// TestBulkTagFailuresContinue: a contact whose tagging fails (a hook
// refuses it) is counted and the rest of the batch still commits.
func TestBulkTagFailuresContinue(t *testing.T) {
	hs := newHarness(t)
	hs.startJobs()
	for i := 1; i <= 3; i++ {
		testutil.InsertContact(t, hs.d, fmt.Sprintf("f%d@example.com", i), core.StatusActive)
	}
	hs.store.Bus.OnTagAdded(func(_ context.Context, _ *sql.Tx, e hooks.TagAdded) error {
		if e.ContactID == 2 {
			return errors.New("refused")
		}
		return nil
	})
	var acc contacts.BulkTagResult
	hs.do("POST", "/tags/bulk", `{"tag":"x","action":"add","contact_ids":[1,2,3]}`).wantStatus(t, http.StatusAccepted).decode(t, &acc)
	var res contacts.BulkTagJobResult
	_ = json.Unmarshal(hs.waitJob(acc.Job.ID).Result, &res)
	if res.Changed != 2 || res.Failed != 1 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "contact 2") {
		t.Fatalf("result = %+v", res)
	}
	if hs.count(`SELECT count(*) FROM contact_tags`) != 2 {
		t.Fatal("the other contacts must be tagged")
	}
}

// TestTagReferencesFollowRenameAndMerge covers Deps.TagRefs: renames and
// merges rewrite saved segment references in the same transaction, a
// failing rewrite rolls the tag change back, and a referenced tag can't be
// deleted.
func TestTagReferencesFollowRenameAndMerge(t *testing.T) {
	hs := newHarness(t)
	var vip, gold contacts.Tag
	hs.do("POST", "/tags", `{"name":"vip"}`).wantStatus(t, http.StatusCreated).decode(t, &vip)
	hs.do("POST", "/tags", `{"name":"gold"}`).wantStatus(t, http.StatusCreated).decode(t, &gold)
	hs.refs.users["vip"] = []int64{4, 9}

	// A failing rewrite rolls the rename back.
	hs.refs.fail = errors.New("segments unavailable")
	hs.do("PATCH", fmt.Sprintf("/tags/%d", vip.ID), `{"name":"VIP Club"}`).wantStatus(t, http.StatusInternalServerError)
	if got, _ := hs.svc.GetTag(t.Context(), vip.ID); got.Name != "vip" {
		t.Fatalf("rename survived a failed rewrite: %+v", got)
	}
	hs.refs.fail = nil

	hs.do("PATCH", fmt.Sprintf("/tags/%d", vip.ID), `{"name":"VIP Club"}`).wantStatus(t, http.StatusOK)
	if n := hs.count(`SELECT count(*) FROM settings WHERE key = 'tagref:vip>VIP Club'`); n != 1 {
		t.Fatalf("rename rewrote %d times in its transaction", n)
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'tag.rename' AND json_extract(changes, '$.segments_updated') = 2`); n != 1 {
		t.Fatalf("rename audit entries with segments_updated = %d", n)
	}

	// Deleting a tag segments still use is refused and names them.
	e := hs.do("DELETE", fmt.Sprintf("/tags/%d", vip.ID), "").wantError(t, http.StatusConflict, api.CodeConflict, "")
	if !strings.Contains(e.Message, "segment 4 and 9") {
		t.Fatalf("delete message = %q", e.Message)
	}

	// A merge rewrites the source name to the target name.
	hs.do("POST", fmt.Sprintf("/tags/%d/merge", vip.ID), fmt.Sprintf(`{"into_tag_id":%d}`, gold.ID)).wantStatus(t, http.StatusOK)
	if n := hs.count(`SELECT count(*) FROM settings WHERE key = 'tagref:VIP Club>gold'`); n != 1 {
		t.Fatalf("merge rewrites = %d", n)
	}
	// The target now carries the references, so it can't be deleted.
	hs.do("DELETE", fmt.Sprintf("/tags/%d", gold.ID), "").wantError(t, http.StatusConflict, api.CodeConflict, "")
	delete(hs.refs.users, "gold")
	hs.do("DELETE", fmt.Sprintf("/tags/%d", gold.ID), "").wantStatus(t, http.StatusNoContent)
}
