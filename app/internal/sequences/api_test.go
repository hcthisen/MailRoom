package sequences_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/jobs"
	"mailroom/internal/sequences"
	"mailroom/internal/testutil"
)

func TestAPISequenceCRUD(t *testing.T) {
	hs := newHarness(t)

	// Create.
	r := hs.do("POST", "/sequences", `{"name":"Welcome","trigger_tag":"joined","exit_tags":["customer"],"reentry":"always",
		"send_window":{"days":[5,1,2],"start":"08:00","end":"20:00"}}`).wantStatus(t, http.StatusCreated)
	var seq sequences.Sequence
	r.decode(t, &seq)
	if seq.Status != "active" || *seq.TriggerTag != "joined" || seq.ExitAddTag != nil || seq.Reentry != "always" ||
		fmt.Sprint(seq.SendWindow.Days) != "[1 2 5]" || len(seq.Steps) != 0 || r.header.Get("ETag") != `"v1"` {
		t.Fatalf("created = %+v (ETag %s)", seq, r.header.Get("ETag"))
	}
	hs.do("POST", "/sequences", `{"name":"Welcome"}`).wantError(t, http.StatusConflict, "conflict", "")
	hs.do("POST", "/sequences", `{"name":"X","reentry":"sometimes"}`).wantError(t, http.StatusUnprocessableEntity, "validation", "reentry")
	hs.do("POST", "/sequences", `{"name":"X","trigger_tag":"a","exit_tags":["A"]}`).wantError(t, http.StatusUnprocessableEntity, "validation", "exit_tags")
	hs.do("POST", "/sequences", `{"name":"X","send_window":{"days":[8],"start":"08:00","end":"20:00"}}`).
		wantError(t, http.StatusUnprocessableEntity, "validation", "")
	hs.doAs(readToken, "POST", "/sequences", `{"name":"R"}`).wantError(t, http.StatusForbidden, "forbidden", "")

	// Get with ETag, read scope.
	path := "/sequences/" + strconv.FormatInt(seq.ID, 10)
	r = hs.doAs(readToken, "GET", path, "").wantStatus(t, http.StatusOK)
	if r.header.Get("ETag") != `"v1"` {
		t.Fatalf("ETag = %q", r.header.Get("ETag"))
	}
	hs.do("GET", "/sequences/999", "").wantError(t, http.StatusNotFound, "not_found", "")

	// Patch: null clears, [] clears, If-Match.
	r = hs.do("PATCH", path, `{"trigger_tag":null,"exit_tags":[],"exit_add_tag":"done","send_window":null,"status":"paused"}`, "If-Match", `"v1"`).
		wantStatus(t, http.StatusOK)
	r.decode(t, &seq)
	if seq.TriggerTag != nil || len(seq.ExitTags) != 0 || *seq.ExitAddTag != "done" || seq.SendWindow != nil || seq.Status != "paused" ||
		r.header.Get("ETag") != `"v2"` {
		t.Fatalf("patched = %+v", seq)
	}
	hs.do("PATCH", path, `{"name":"Stale"}`, "If-Match", `"v1"`).wantError(t, http.StatusPreconditionFailed, "precondition_failed", "")
	hs.do("PATCH", path, `{"name":"Bad"}`, "If-Match", `nonsense`).wantError(t, http.StatusUnprocessableEntity, "validation", "header.If-Match")
	// A no-op keeps the version; omitted fields are unchanged.
	r = hs.do("PATCH", path, `{"status":"paused"}`).wantStatus(t, http.StatusOK)
	if r.header.Get("ETag") != `"v2"` {
		t.Fatalf("no-op ETag = %s", r.header.Get("ETag"))
	}
	r = hs.do("POST", path+"/resume", "", "If-Match", `"v2"`).wantStatus(t, http.StatusOK)
	r.decode(t, &seq)
	if seq.Status != "active" || *seq.ExitAddTag != "done" {
		t.Fatalf("resumed = %+v", seq)
	}
	hs.do("POST", path+"/pause", "").wantStatus(t, http.StatusOK)

	// Pagination.
	for i := range 3 {
		hs.do("POST", "/sequences", fmt.Sprintf(`{"name":"S%d"}`, i)).wantStatus(t, http.StatusCreated)
	}
	var page struct {
		Items      []sequences.Sequence `json:"items"`
		NextCursor string               `json:"next_cursor"`
	}
	hs.do("GET", "/sequences?limit=3", "").wantStatus(t, http.StatusOK).decode(t, &page)
	if len(page.Items) != 3 || page.NextCursor == "" || page.Items[0].ID != seq.ID {
		t.Fatalf("page 1 = %+v", page)
	}
	hs.do("GET", "/sequences?limit=3&cursor="+page.NextCursor, "").wantStatus(t, http.StatusOK).decode(t, &page)
	if len(page.Items) != 1 || page.NextCursor != "" || page.Items[0].Name != "S2" {
		t.Fatalf("page 2 = %+v", page)
	}
	hs.do("GET", "/sequences?cursor=zzz", "").wantError(t, http.StatusUnprocessableEntity, "validation", "query.cursor")

	// Delete with active enrollments needs force.
	c := hs.contact("a@example.com")
	hs.do("POST", path+"/steps", `{"email":{"subject":"Hi","body":"Hello"},"delay_seconds":60}`).wantStatus(t, http.StatusCreated)
	id := hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	hs.do("DELETE", path, "").wantError(t, http.StatusConflict, "conflict", "")
	var del sequences.DeleteResult
	hs.do("DELETE", path+"?force=true", "").wantStatus(t, http.StatusOK).decode(t, &del)
	if del.ExitedEnrollments != 1 || hs.count(`SELECT count(*) FROM enrollments WHERE id = ?`, id) != 0 {
		t.Fatalf("delete = %+v", del)
	}
	hs.do("GET", path, "").wantError(t, http.StatusNotFound, "not_found", "")
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'sequence.delete' AND actor_name = 'setup-agent'`); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
}

func TestAPISteps(t *testing.T) {
	hs := newHarness(t)
	var seq sequences.Sequence
	hs.do("POST", "/sequences", `{"name":"Drip"}`).wantStatus(t, http.StatusCreated).decode(t, &seq)
	path := "/sequences/" + strconv.FormatInt(seq.ID, 10)

	var st sequences.SequenceStep
	r := hs.do("POST", path+"/steps", `{"email":{"subject":"One","body":"Body one"},"delay_seconds":0}`, "If-Match", `"v1"`).
		wantStatus(t, http.StatusCreated)
	r.decode(t, &st)
	if st.Position != 1 || st.Subject != "One" || r.header.Get("ETag") != `"v2"` {
		t.Fatalf("step = %+v, ETag %s", st, r.header.Get("ETag"))
	}
	hs.do("POST", path+"/steps", `{"email":{"subject":"Two","body":"x"},"delay_seconds":1}`, "If-Match", `"v1"`).
		wantError(t, http.StatusPreconditionFailed, "precondition_failed", "")
	hs.do("POST", path+"/steps", `{"email":{"subject":"","body":"x"},"delay_seconds":1}`).wantError(t, http.StatusUnprocessableEntity, "validation", "email.subject")
	hs.do("POST", path+"/steps", `{"delay_seconds":1}`).wantError(t, http.StatusUnprocessableEntity, "validation", "email_id")
	var st2 sequences.SequenceStep
	hs.do("POST", path+"/steps", `{"email_id":`+strconv.FormatInt(st.EmailID, 10)+`,"delay_seconds":3600}`).wantStatus(t, http.StatusCreated).decode(t, &st2)

	// Patch the delay and content.
	stepPath := path + "/steps/" + strconv.FormatInt(st2.ID, 10)
	r = hs.do("PATCH", stepPath, `{"delay_seconds":7200}`, "If-Match", `"v3"`).wantStatus(t, http.StatusOK)
	r.decode(t, &st2)
	if st2.DelaySeconds != 7200 || st2.Delay != "2h" || r.header.Get("ETag") != `"v4"` {
		t.Fatalf("patched = %+v", st2)
	}
	hs.do("PATCH", stepPath, `{"email_id":1,"subject":"x"}`).wantError(t, http.StatusUnprocessableEntity, "validation", "email_id")
	hs.do("PATCH", path+"/steps/999", `{"delay_seconds":1}`).wantError(t, http.StatusNotFound, "not_found", "")

	// Reorder.
	var got sequences.ReorderResult
	r = hs.do("POST", path+"/steps/reorder", jsonBody(t, map[string]any{"step_ids": []int64{st2.ID, st.ID}}), "If-Match", `"v4"`).
		wantStatus(t, http.StatusOK)
	r.decode(t, &got)
	if got.Sequence.Steps[0].ID != st2.ID || got.Sequence.Version != 5 || got.Completed != 0 || r.header.Get("ETag") != `"v5"` {
		t.Fatalf("reordered = %+v, ETag %s", got, r.header.Get("ETag"))
	}
	hs.do("POST", path+"/steps/reorder", `{"step_ids":[1]}`).wantError(t, http.StatusUnprocessableEntity, "validation", "step_ids")
	hs.do("POST", path+"/steps/reorder?on_active=sideways", jsonBody(t, map[string]any{"step_ids": []int64{st.ID, st2.ID}})).
		wantError(t, http.StatusUnprocessableEntity, "validation", "")
	hs.do("POST", path+"/steps/reorder", jsonBody(t, map[string]any{"step_ids": []int64{st.ID, st2.ID}}), "If-Match", `"v4"`).
		wantError(t, http.StatusPreconditionFailed, "precondition_failed", "")
	// Reorder back and forth with on_active=end: the active enrollment completes.
	other := hs.contact("reorder@example.com")
	ended := hs.enroll(seq.ID, other.ID, core.EnrollOptions{})
	hs.do("POST", path+"/steps/reorder?on_active=end", jsonBody(t, map[string]any{"step_ids": []int64{st.ID, st2.ID}}), "If-Match", `"v5"`).
		wantStatus(t, http.StatusOK).decode(t, &got)
	if got.Completed != 1 || hs.enrollment(ended).Status != sequences.EnrollmentCompleted {
		t.Fatalf("reorder on_active=end = %+v, enrollment %+v", got, hs.enrollment(ended))
	}

	// Test send (202) goes through the content service's test path.
	var ts sequences.StepTestResult
	hs.do("POST", stepPath+"/test", `{}`).wantStatus(t, http.StatusAccepted).decode(t, &ts)
	if len(ts.MessageIDs) != 1 || ts.To[0] != ownerEmail {
		t.Fatalf("test = %+v", ts)
	}
	var kind, data string
	if err := hs.d.Read.QueryRow(`SELECT kind, data FROM messages WHERE id = ?`, ts.MessageIDs[0]).Scan(&kind, &data); err != nil {
		t.Fatal(err)
	}
	if kind != "test" || !strings.Contains(data, `"test":true`) {
		t.Fatalf("test message = %s %s", kind, data)
	}
	hs.do("POST", stepPath+"/test", `{"to":["a@example.com","b@example.com","c@example.com","d@example.com","e@example.com","f@example.com"]}`).
		wantError(t, http.StatusUnprocessableEntity, "validation", "")

	// Delete needs on_active while an enrollment waits for the step.
	c := hs.contact("a@example.com")
	hs.enroll(seq.ID, c.ID, core.EnrollOptions{})
	firstPath := path + "/steps/" + strconv.FormatInt(st.ID, 10) // first again after the second reorder
	hs.do("DELETE", firstPath, "").wantError(t, http.StatusConflict, "conflict", "")
	hs.do("DELETE", firstPath+"?on_active=sideways", "").wantError(t, http.StatusUnprocessableEntity, "validation", "")
	var del sequences.DeleteStepResult
	r = hs.do("DELETE", firstPath+"?on_active=advance", "").wantStatus(t, http.StatusOK)
	r.decode(t, &del)
	if del.Advanced != 1 || len(del.Sequence.Steps) != 1 || r.header.Get("ETag") != `"v7"` {
		t.Fatalf("delete = %+v", del)
	}
}

func TestAPIEnrollments(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "Drip"}, 0, 86400)
	path := "/sequences/" + strconv.FormatInt(seq.ID, 10)
	a := hs.contact("a@example.com")
	b := hs.contact("b@example.com")
	hs.contact("c@example.com")
	unsub := hs.contact("u@example.com")
	hs.setStatus(unsub.ID, core.StatusUnsubscribed)

	// Dry run changes nothing.
	var res sequences.EnrollResult
	hs.do("POST", path+"/enrollments?dry_run=true", `{"emails":["A@example.com","u@example.com","nobody@example.com"]}`).
		wantStatus(t, http.StatusOK).decode(t, &res)
	if !res.DryRun || res.WouldEnroll != 1 || res.Enrolled != 0 || res.Skipped["not_active"] != 1 || res.Skipped["not_found"] != 1 {
		t.Fatalf("dry run = %+v", res)
	}
	if n := hs.count(`SELECT count(*) FROM enrollments`); n != 0 {
		t.Fatalf("dry run left %d enrollments", n)
	}

	// Real run at step 2 with a start time.
	body := fmt.Sprintf(`{"contact_ids":[%d,%d],"at_step":2,"start_at":"2026-01-01T00:00:00Z","source":"migration"}`, a.ID, b.ID)
	hs.do("POST", path+"/enrollments", body).wantStatus(t, http.StatusOK).decode(t, &res)
	if res.Enrolled != 2 || len(res.EnrollmentIDs) != 2 {
		t.Fatalf("enrol = %+v", res)
	}
	hs.do("POST", path+"/enrollments", `{"contact_ids":[1],"at_step":5}`).wantError(t, http.StatusUnprocessableEntity, "validation", "at_step")
	hs.do("POST", path+"/enrollments", `{}`).wantError(t, http.StatusUnprocessableEntity, "validation", "contact_ids")
	hs.do("POST", "/sequences/999/enrollments", `{"contact_ids":[1]}`).wantError(t, http.StatusNotFound, "not_found", "")

	// List with filter and pagination (newest first).
	var page struct {
		Items      []sequences.SequenceEnrollment `json:"items"`
		NextCursor string                         `json:"next_cursor"`
	}
	hs.doAs(readToken, "GET", path+"/enrollments?limit=1&status=active", "").wantStatus(t, http.StatusOK).decode(t, &page)
	if len(page.Items) != 1 || page.NextCursor == "" || page.Items[0].ID != res.EnrollmentIDs[1] {
		t.Fatalf("page 1 = %+v", page)
	}
	e := page.Items[0]
	if e.Email != "b@example.com" || e.Source != "migration" || *e.NextStepPosition != 2 || e.NextRunAt.UTC().Format("2006-01-02T15") != "2026-01-02T00" {
		t.Fatalf("enrollment = %+v", e)
	}
	hs.do("GET", path+"/enrollments?limit=1&status=active&cursor="+page.NextCursor, "").wantStatus(t, http.StatusOK).decode(t, &page)
	if len(page.Items) != 1 || page.NextCursor != "" || page.Items[0].ID != res.EnrollmentIDs[0] {
		t.Fatalf("page 2 = %+v", page)
	}
	hs.do("GET", path+"/enrollments?status=exited", "").wantStatus(t, http.StatusOK).decode(t, &page)
	if len(page.Items) != 0 {
		t.Fatalf("exited = %+v", page)
	}

	// Manual exit.
	epath := path + "/enrollments/" + strconv.FormatInt(res.EnrollmentIDs[0], 10)
	var got sequences.SequenceEnrollment
	hs.do("DELETE", epath, "").wantStatus(t, http.StatusOK).decode(t, &got)
	if got.Status != "exited" || got.ExitReason != "manual" || got.NextRunAt != nil || got.FinishedAt == nil {
		t.Fatalf("unenrolled = %+v", got)
	}
	hs.do("DELETE", epath, "").wantError(t, http.StatusConflict, "conflict", "")
	hs.do("DELETE", path+"/enrollments/999", "").wantError(t, http.StatusNotFound, "not_found", "")

	// Stats.
	var st sequences.SequenceStats
	hs.doAs(readToken, "GET", path+"/stats", "").wantStatus(t, http.StatusOK).decode(t, &st)
	if st.Active != 1 || st.Exited != 1 || len(st.Steps) != 2 || st.Steps[1].Waiting != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestAPIEnrollLargeRunsAsJob(t *testing.T) {
	hs := newHarness(t)
	hs.startJobs()
	seq := hs.sequence(sequences.SequenceInput{Name: "Big"}, 3600)
	path := "/sequences/" + strconv.FormatInt(seq.ID, 10)
	var ids []int64
	for i := range sequences.EnrollJobThreshold + 20 {
		c := testutil.InsertContact(t, hs.d, fmt.Sprintf("c%d@example.com", i), core.StatusActive)
		ids = append(ids, c.ID)
	}
	ids = append(ids, 99999) // unknown

	// A dry run of a large request is answered directly.
	var res sequences.EnrollResult
	hs.do("POST", path+"/enrollments?dry_run=true", jsonBody(t, map[string]any{"contact_ids": ids})).wantStatus(t, http.StatusOK).decode(t, &res)
	if res.WouldEnroll != len(ids)-1 || res.Skipped["not_found"] != 1 || res.Job != nil || res.EnrollmentIDs != nil {
		t.Fatalf("dry run = %+v", res)
	}

	hs.do("POST", path+"/enrollments", jsonBody(t, map[string]any{"contact_ids": ids})).wantStatus(t, http.StatusAccepted).decode(t, &res)
	if res.Job == nil || res.Job.Type != sequences.JobEnroll {
		t.Fatalf("result = %+v", res)
	}
	j := hs.waitJob(res.Job.ID)
	if j.Status != jobs.StatusSucceeded {
		t.Fatalf("job = %+v", j)
	}
	var out sequences.EnrollResult
	if err := jsonUnmarshal(j.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Enrolled != len(ids)-1 || out.Skipped["not_found"] != 1 {
		t.Fatalf("job result = %+v", out)
	}
	if n := hs.count(`SELECT count(*) FROM enrollments WHERE sequence_id = ?`, seq.ID); n != len(ids)-1 {
		t.Fatalf("enrollments = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'sequence.enroll'`); n != 1 {
		t.Fatalf("audit = %d", n)
	}
}

// TestEnrollDryRunDoesNotHoldWriteConnection checks that a dry run, also
// one above the job threshold, only reads: it completes while another
// transaction holds the single write connection, and matches a real run.
func TestEnrollDryRunDoesNotHoldWriteConnection(t *testing.T) {
	hs := newHarness(t)
	seq := hs.sequence(sequences.SequenceInput{Name: "Drip", ExitTags: []string{"customer"}}, 0)
	const n = sequences.EnrollJobThreshold + 20
	emails := make([]string, 0, n+2)
	hs.tx(func(tx *sql.Tx) error {
		for i := range n {
			email := fmt.Sprintf("c%03d@example.com", i)
			emails = append(emails, email)
			in := contactstore.CreateInput{Email: email, Status: core.StatusActive, Source: "api"}
			switch {
			case i%10 == 1:
				in.Status = core.StatusUnsubscribed
			case i%10 == 2:
				in.Tags = []string{"customer"}
			}
			c, err := hs.store.Create(hs.ctx, tx, in)
			if err != nil {
				return err
			}
			if i%10 == 3 { // already enrolled
				if _, err := hs.svc.Enroll(hs.ctx, tx, seq.ID, c.ID, core.EnrollOptions{}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	emails = append(emails, "nobody@example.com", "C000@example.com")
	req := sequences.EnrollRequest{Emails: emails}
	want := map[string]int{"not_found": 1, "not_active": n / 10, "exit_tag": n / 10, "already_active": n / 10}
	wantEnroll := n - 3*(n/10)

	// Hold the write connection while the dry run runs.
	release, held := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- hs.d.WithTx(context.Background(), func(*sql.Tx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(hs.ctx, 10*time.Second)
	res, err := hs.svc.EnrollContacts(ctx, seq.ID, req, true)
	cancel()
	close(release)
	if werr := <-writerDone; werr != nil {
		t.Fatal(werr)
	}
	if err != nil {
		t.Fatalf("dry run while the write connection is busy: %v", err)
	}
	if !res.DryRun || res.WouldEnroll != wantEnroll || res.Enrolled != 0 || res.Job != nil || len(res.EnrollmentIDs) != 0 || !maps.Equal(res.Skipped, want) {
		t.Fatalf("dry run = %+v, want would_enroll %d skipped %v", res, wantEnroll, want)
	}
	if got := hs.count(`SELECT count(*) FROM enrollments`); got != n/10 {
		t.Fatalf("dry run changed enrollments: %d", got)
	}
	// Also through the API, with the same answer.
	var viaAPI sequences.EnrollResult
	hs.do("POST", "/sequences/"+strconv.FormatInt(seq.ID, 10)+"/enrollments?dry_run=true", jsonBody(t, req)).
		wantStatus(t, http.StatusOK).decode(t, &viaAPI)
	if viaAPI.WouldEnroll != wantEnroll || !maps.Equal(viaAPI.Skipped, want) || viaAPI.Job != nil {
		t.Fatalf("API dry run = %+v", viaAPI)
	}
	// Errors are the same as a real run's.
	if _, err := hs.svc.EnrollContacts(hs.ctx, seq.ID, sequences.EnrollRequest{ContactIDs: []int64{1}, AtStep: 3}, true); fieldOf(err) != "at_step" {
		t.Fatalf("dry run at_step: %v", err)
	}
	if _, err := hs.svc.EnrollContacts(hs.ctx, 999, sequences.EnrollRequest{ContactIDs: []int64{1}}, true); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("dry run unknown sequence: %v", err)
	}
	empty := hs.sequence(sequences.SequenceInput{Name: "Empty"})
	res, err = hs.svc.EnrollContacts(hs.ctx, empty.ID, sequences.EnrollRequest{ContactIDs: []int64{1, 2}}, true)
	if err != nil || res.Skipped["no_steps"] != 2 || res.WouldEnroll != 0 {
		t.Fatalf("dry run without steps = %+v, %v", res, err)
	}
}

func TestAPIImportExport(t *testing.T) {
	hs := newHarness(t)
	body := jsonBody(t, map[string]any{"yaml": welcomeYAML, "files": welcomeFiles})

	var seq sequences.Sequence
	hs.do("POST", "/sequences/import?dry_run=true", body).wantStatus(t, http.StatusOK).decode(t, &seq)
	if seq.Name != "Community welcome" || hs.count(`SELECT count(*) FROM sequences`) != 0 {
		t.Fatalf("dry run = %+v", seq)
	}
	r := hs.do("POST", "/sequences/import", body).wantStatus(t, http.StatusCreated)
	r.decode(t, &seq)
	if len(seq.Steps) != 3 || r.header.Get("ETag") == "" {
		t.Fatalf("import = %+v", seq)
	}
	hs.do("POST", "/sequences/import", body).wantError(t, http.StatusConflict, "conflict", "")
	bad := jsonBody(t, map[string]any{"yaml": "name: X\nsteps:\n  - delay: soon\n    subject: A\n    body: B\n"})
	hs.do("POST", "/sequences/import", bad).wantError(t, http.StatusUnprocessableEntity, "validation", "steps[0].delay")

	var out sequences.SequenceYAML
	hs.doAs(readToken, "GET", "/sequences/"+strconv.FormatInt(seq.ID, 10)+"/export", "").wantStatus(t, http.StatusOK).decode(t, &out)
	if !strings.Contains(out.YAML, "name: Community welcome") || len(out.Files) != 3 {
		t.Fatalf("export = %+v", out)
	}
}

func TestAPIOpenAPI(t *testing.T) {
	hs := newHarness(t)
	r := hs.doAs(readToken, "GET", "/openapi.json", "").wantStatus(t, http.StatusOK)
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Tags        []string `json:"tags"`
		} `json:"paths"`
	}
	r.decode(t, &spec)
	want := []string{
		"list-sequences", "get-sequence", "create-sequence", "update-sequence", "pause-sequence", "resume-sequence",
		"delete-sequence", "get-sequence-stats", "import-sequence", "export-sequence", "add-sequence-step",
		"update-sequence-step", "delete-sequence-step", "reorder-sequence-steps", "send-sequence-step-test",
		"list-sequence-enrollments", "enroll-sequence-contacts", "unenroll-sequence-contact",
	}
	found := map[string]bool{}
	for _, ops := range spec.Paths {
		for _, op := range ops {
			if len(op.Tags) > 0 && op.Tags[0] == "Sequences" {
				found[op.OperationID] = true
			}
		}
	}
	for _, id := range want {
		if !found[id] {
			t.Errorf("operation %s missing from the spec", id)
		}
	}
	if len(found) != len(want) {
		t.Errorf("spec has %d Sequences operations, want %d", len(found), len(want))
	}
	if !strings.Contains(string(r.body), `"trigger_tag"`) {
		t.Error("spec lacks trigger_tag")
	}
}
