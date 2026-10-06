package imports

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/jobs"
	"mailroom/internal/testutil"
)

const apiCSV = "email,first_name,status,tags\nann@example.com,Ann,,news\nbob@example.com,Bob,unsubscribed,news;vip\nbad,,,\n"

// acceptedJob decodes a 202 create-import response.
func acceptedJob(t *testing.T, r resp) jobs.Job {
	t.Helper()
	r.wantStatus(t, http.StatusAccepted)
	var res ImportResult
	r.decode(t, &res)
	if res.Job == nil || res.Report != nil || res.Job.Type != JobType || res.Job.Status != jobs.StatusQueued {
		t.Fatalf("result = %s", r.body)
	}
	return *res.Job
}

func TestAPICreateImportBodies(t *testing.T) {
	jsonBody, _ := json.Marshal(map[string]string{"csv": apiCSV})
	tests := []struct {
		name string
		send func(hs *harness) resp
	}{
		{"multipart", func(hs *harness) resp { return hs.postMultipart("", apiCSV) }},
		{"json", func(hs *harness) resp {
			return hs.send(adminToken, http.MethodPost, "/imports", "application/json", strings.NewReader(string(jsonBody)))
		}},
		{"text/csv", func(hs *harness) resp {
			return hs.send(adminToken, http.MethodPost, "/imports", "text/csv; charset=utf-8", strings.NewReader(apiCSV))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.startJobs()
			j := acceptedJob(t, tt.send(hs))
			hs.waitJob(j.ID)

			var ij ImportJob
			r := hs.get(fmt.Sprintf("/imports/%d", j.ID)).wantStatus(t, http.StatusOK)
			r.decode(t, &ij)
			rep := ij.Report
			if ij.Status != jobs.StatusSucceeded || rep == nil || rep.Created != 2 || rep.RejectedCount != 1 ||
				rep.Rejected[0].Line != 4 || rep.StatusCounts["unsubscribed"] != 1 || rep.TagCounts["news"] != 2 {
				t.Fatalf("import = %s", r.body)
			}
			if strings.Contains(string(r.body), "example.com") {
				t.Fatalf("the report must not contain addresses: %s", r.body)
			}
			if hs.contact("bob@example.com").Status != core.StatusUnsubscribed {
				t.Fatal("bob must be unsubscribed")
			}
			if files := hs.uploads(); len(files) != 0 {
				t.Fatalf("uploads left: %v", files)
			}
		})
	}
}

func TestAPIDryRun(t *testing.T) {
	hs := newHarness(t)
	testutil.InsertContact(t, hs.d, "ann@example.com", core.StatusBounced)
	r := hs.postMultipart("?dry_run=true", apiCSV, "on_existing", "skip").wantStatus(t, http.StatusOK)
	var res ImportResult
	r.decode(t, &res)
	rep := res.Report
	if res.Job != nil || rep == nil || !rep.DryRun || rep.OnExisting != OnExistingSkip || rep.Created != 1 || rep.Skipped != 1 || rep.RejectedCount != 1 {
		t.Fatalf("dry run = %s", r.body)
	}
	if hs.count(`SELECT count(*) FROM contacts`) != 1 || hs.count(`SELECT count(*) FROM jobs`) != 0 || hs.count(`SELECT count(*) FROM audit_log`) != 0 {
		t.Fatal("a dry run must write nothing")
	}
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("a dry run must not store the file: %v", files)
	}

	// A dry run with a bad file is a validation error too.
	hs.postMultipart("?dry_run=true", "name\nx\n").wantError(t, http.StatusUnprocessableEntity, "validation", "file")
}

func TestAPIOnExisting(t *testing.T) {
	hs := newHarness(t)
	testutil.InsertContact(t, hs.d, "ann@example.com", core.StatusActive)
	csv := "email,first_name\nann@example.com,Ann\n"
	dry := func(r resp) ImportReport {
		t.Helper()
		var res ImportResult
		r.wantStatus(t, http.StatusOK).decode(t, &res)
		return *res.Report
	}
	if rep := dry(hs.postMultipart("?dry_run=true&on_existing=skip", csv)); rep.Skipped != 1 {
		t.Fatalf("query skip = %+v", rep)
	}
	if rep := dry(hs.postMultipart("?dry_run=true", csv)); rep.Updated != 1 || rep.OnExisting != OnExistingUpdate {
		t.Fatalf("default = %+v", rep)
	}
	body := `{"csv": "email,first_name\nann@example.com,Ann\n", "on_existing": "skip"}`
	if rep := dry(hs.send(adminToken, http.MethodPost, "/imports?dry_run=true", "application/json", strings.NewReader(body))); rep.Skipped != 1 {
		t.Fatalf("json skip = %+v", rep)
	}

	hs.postMultipart("?on_existing=replace", csv).wantError(t, http.StatusUnprocessableEntity, "validation", "")
	hs.postMultipart("?on_existing=update", csv, "on_existing", "skip").wantError(t, http.StatusUnprocessableEntity, "validation", "on_existing")
	hs.postMultipart("", csv, "on_existing", "nope").wantError(t, http.StatusUnprocessableEntity, "validation", "on_existing")
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("refused uploads must be removed: %v", files)
	}
}

func TestAPICreateImportErrors(t *testing.T) {
	hs := newHarness(t)
	post := func(ct, body string) resp {
		return hs.send(adminToken, http.MethodPost, "/imports", ct, strings.NewReader(body))
	}
	noFileCT, noFile := func() (string, *strings.Builder) {
		var b strings.Builder
		mw := multipart.NewWriter(&b)
		mw.WriteField("on_existing", "skip")
		mw.Close()
		return mw.FormDataContentType(), &b
	}()
	twoFilesCT, twoFiles := func() (string, *strings.Builder) {
		var b strings.Builder
		mw := multipart.NewWriter(&b)
		for range 2 {
			fw, _ := mw.CreateFormFile("file", "a.csv")
			io.WriteString(fw, "email\na@example.com\n")
		}
		mw.Close()
		return mw.FormDataContentType(), &b
	}()

	tests := []struct {
		name   string
		r      resp
		status int
		field  string
	}{
		{"missing email column", hs.postMultipart("", "first_name\nAnn\n"), 422, "file"},
		{"empty file", hs.postMultipart("", ""), 422, "file"},
		{"no file part", post(noFileCT, noFile.String()), 422, "file"},
		{"two files", post(twoFilesCT, twoFiles.String()), 422, "file"},
		{"json without csv", post("application/json", `{"on_existing":"skip"}`), 422, "csv"},
		{"json unknown field", post("application/json", `{"csv":"email\n","extra":1}`), 422, "body"},
		{"json malformed", post("application/json", `{"csv":`), 422, "body"},
		{"no content type", post("", apiCSV), 415, "Content-Type"},
		{"unsupported content type", post("application/xml", "<x/>"), 415, "Content-Type"},
		{"multipart without boundary", post("multipart/form-data", "x"), 422, "Content-Type"},
		{"broken multipart", post("multipart/form-data; boundary=zzz", "--zzz\r\nbroken"), 422, "file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.r.wantError(t, tt.status, "validation", tt.field)
		})
	}
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("refused uploads must be removed: %v", files)
	}
	if n := hs.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("jobs = %d", n)
	}
}

func TestAPIRequestTooLarge(t *testing.T) {
	hs := newHarness(t)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, _ := mw.CreateFormFile("file", "big.csv")
		io.Copy(fw, bigCSV(maxRequestBytes+1))
		mw.Close()
		pw.Close()
	}()
	r := hs.send(adminToken, http.MethodPost, "/imports", mw.FormDataContentType(), pr)
	pr.CloseWithError(io.ErrClosedPipe) // unblock the writer
	if r.status != http.StatusRequestEntityTooLarge && r.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", r.status, r.body)
	}
	r.wantError(t, r.status, "validation", "file")
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("uploads left: %v", files)
	}
}

func TestAPIScopesAndNotFound(t *testing.T) {
	hs := newHarness(t)
	ct, body := multipartBody(t, apiCSV)
	hs.send(readToken, http.MethodPost, "/imports", ct, body).wantError(t, http.StatusForbidden, "forbidden", "")
	hs.send("", http.MethodPost, "/imports", "text/csv", strings.NewReader(apiCSV)).wantError(t, http.StatusUnauthorized, "unauthorized", "")
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("a refused request must not store a file: %v", files)
	}

	hs.get("/imports/999").wantError(t, http.StatusNotFound, "not_found", "")
	hs.get("/imports/0").wantError(t, http.StatusUnprocessableEntity, "validation", "")
	other, err := hs.runner.Submit(actorCtx(), nil, "other.job", nil)
	if err != nil {
		t.Fatal(err)
	}
	hs.get(fmt.Sprintf("/imports/%d", other.ID)).wantError(t, http.StatusNotFound, "not_found", "")

	// A queued import has no report yet; read tokens may look.
	j := acceptedJob(t, hs.postMultipart("", apiCSV))
	var ij ImportJob
	hs.send(readToken, http.MethodGet, fmt.Sprintf("/imports/%d", j.ID), "", nil).wantStatus(t, http.StatusOK).decode(t, &ij)
	if ij.Status != jobs.StatusQueued || ij.Report != nil || ij.Filename != "contacts.csv" {
		t.Fatalf("queued import = %+v", ij)
	}
}

func TestAPIListImportsPagination(t *testing.T) {
	hs := newHarness(t)
	var ids []int64
	for range 3 {
		ids = append(ids, acceptedJob(t, hs.postMultipart("", apiCSV)).ID)
	}
	if _, err := hs.runner.Submit(actorCtx(), nil, "other.job", nil); err != nil {
		t.Fatal(err)
	}
	type page struct {
		Items      []ImportJob `json:"items"`
		NextCursor string      `json:"next_cursor"`
	}
	var p1, p2 page
	hs.get("/imports?limit=2").wantStatus(t, http.StatusOK).decode(t, &p1)
	if len(p1.Items) != 2 || p1.Items[0].JobID != ids[2] || p1.Items[1].JobID != ids[1] || p1.NextCursor == "" {
		t.Fatalf("page 1 = %+v", p1)
	}
	hs.get("/imports?limit=2&cursor="+p1.NextCursor).wantStatus(t, http.StatusOK).decode(t, &p2)
	if len(p2.Items) != 1 || p2.Items[0].JobID != ids[0] || p2.NextCursor != "" {
		t.Fatalf("page 2 = %+v", p2)
	}
	hs.get("/imports?cursor=not-a-cursor").wantError(t, http.StatusUnprocessableEntity, "validation", "")
}

func TestAPIIdempotencyKey(t *testing.T) {
	hs := newHarness(t)
	ct, body := multipartBody(t, apiCSV)
	raw := body.String()
	first := acceptedJob(t, hs.send(adminToken, http.MethodPost, "/imports", ct, strings.NewReader(raw), "Idempotency-Key", "import-1"))
	again := hs.send(adminToken, http.MethodPost, "/imports", ct, strings.NewReader(raw), "Idempotency-Key", "import-1")
	if j := acceptedJob(t, again); j.ID != first.ID {
		t.Fatalf("replay created job %d, want %d", j.ID, first.ID)
	}
	if n := hs.count(`SELECT count(*) FROM jobs`); n != 1 {
		t.Fatalf("jobs = %d", n)
	}
}

func TestAPIOpenAPIDocumentsUploadBody(t *testing.T) {
	hs := newHarness(t)
	var found bool
	for _, item := range hs.a.OpenAPI().Paths {
		op := item.Post
		if op == nil || op.OperationID != opCreate {
			continue
		}
		found = true
		if op.RequestBody == nil || !op.RequestBody.Required {
			t.Fatal("create-import must document its body")
		}
		for _, ct := range []string{"multipart/form-data", "application/json", "text/csv"} {
			if op.RequestBody.Content[ct] == nil {
				t.Errorf("content type %s is not documented", ct)
			}
		}
		if op.RequestBody.Content["multipart/form-data"].Schema.Properties["file"] == nil {
			t.Error("the file field is not documented")
		}
		accepted, ok := op.Responses["202"], op.Responses["200"]
		if accepted == nil || ok == nil {
			t.Fatalf("responses 202 and 200 (dry run) must be documented: %v", op.Responses)
		}
		if ok.Content["application/json"] == nil || ok.Content["application/json"].Schema == nil ||
			ok.Content["application/json"].Schema.Ref != accepted.Content["application/json"].Schema.Ref ||
			!strings.Contains(ok.Description, "Dry run") {
			t.Errorf("200 response = %+v", ok)
		}
	}
	if !found {
		t.Fatal("create-import is not in the spec")
	}
	routes := map[string]bool{}
	for _, r := range hs.a.Routes() {
		routes[r.OperationID] = true
	}
	for _, id := range []string{opCreate, opGet, opList} {
		if !routes[id] {
			t.Errorf("route %s not registered", id)
		}
	}
}
