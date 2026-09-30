package segments_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/segments"
	"mailroom/internal/testutil"
)

// fakeAuth accepts "mr_admin" (scope admin) and "mr_read" (scope read).
type fakeAuth struct{}

func (fakeAuth) AuthenticateToken(_ context.Context, token string) (api.Principal, error) {
	switch token {
	case "mr_admin":
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "4", Name: "agent"}, Scope: api.ScopeAdmin}, nil
	case "mr_read":
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "5", Name: "reader"}, Scope: api.ScopeRead}, nil
	}
	return api.Principal{}, api.ErrUnauthorized
}

func (fakeAuth) AuthenticateSession(context.Context, string) (api.Principal, string, error) {
	return api.Principal{}, "", api.ErrUnauthorized
}

type apiClient struct {
	t *testing.T
	h http.Handler
}

type response struct {
	code int
	etag string
	body []byte
}

func (r response) errCode() string {
	var e struct{ Error api.ErrorBody }
	_ = json.Unmarshal(r.body, &e)
	return e.Error.Code + ":" + e.Error.Field
}

func (c apiClient) do(token, method, path, body string, headers ...string) response {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return response{code: rec.Code, etag: rec.Header().Get("ETag"), body: rec.Body.Bytes()}
}

func (c apiClient) admin(method, path, body string, headers ...string) response {
	c.t.Helper()
	return c.do("mr_admin", method, path, body, headers...)
}

func newAPI(t *testing.T) (*fixture, apiClient) {
	t.Helper()
	f := newFixture(t)
	a, err := api.New(api.Config{DB: f.db, BaseURL: "https://news.example.com", Auth: fakeAuth{}, Clock: f.clock})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.RegisterAPI(a)
	return f, apiClient{t: t, h: a.Handler()}
}

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return v
}

func TestAPI(t *testing.T) {
	f, c := newAPI(t)
	vip := `{"match":"all","rules":[{"field":"tag","op":"has","value":"vip"}]}`

	// Create.
	r := c.admin("POST", "/segments", `{"name":"VIPs","filter":`+vip+`}`)
	if r.code != http.StatusCreated || r.etag != `"v1"` {
		t.Fatalf("create = %d %s %s", r.code, r.etag, r.body)
	}
	sg := decode[segments.Segment](t, r)
	if sg.Name != "VIPs" || sg.Version != 1 || len(sg.Filter.Rules) != 1 || sg.Filter.Rules[0].Value != "vip" {
		t.Fatalf("created = %+v", sg)
	}
	id := fmt.Sprint(sg.ID)

	// Create errors use the one error format.
	for _, tt := range []struct {
		name, body string
		status     int
		err        string
	}{
		{"duplicate name", `{"name":"vips","filter":` + vip + `}`, 409, "conflict:"},
		{"invalid rule", `{"name":"X","filter":{"rules":[{"field":"tag","op":"has"}]}}`, 422, "validation:filter.rules[0].value"},
		{"nested invalid rule", `{"name":"X","filter":{"rules":[{"match":"any","rules":[{"field":"attr","key":"a b","op":"exists"}]}]}}`, 422, "validation:filter.rules[0].rules[0].key"},
		{"schema: unknown member", `{"name":"X","filter":{"rules":[{"field":"tag","op":"has","value":"a","bogus":1}]}}`, 422, "validation:filter.rules[0].bogus"},
		{"schema: bad op", `{"name":"X","filter":{"rules":[{"field":"tag","op":"like","value":"a"}]}}`, 422, "validation:filter.rules[0].op"},
		{"schema: missing name", `{"filter":` + vip + `}`, 422, "validation:name"},
		{"schema: missing filter", `{"name":"X"}`, 422, "validation:filter"},
		{"blank name", `{"name":"  ","filter":` + vip + `}`, 422, "validation:name"},
		{"malformed JSON", `{"name":`, 400, "validation:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := c.admin("POST", "/segments", tt.body)
			if r.code != tt.status || r.errCode() != tt.err {
				t.Fatalf("= %d %s (want %d %s)", r.code, r.body, tt.status, tt.err)
			}
		})
	}

	// Dry run creates nothing.
	r = c.admin("POST", "/segments?dry_run=true", `{"name":"Dry","filter":{"rules":[]}}`)
	if r.code != http.StatusOK || decode[segments.Segment](t, r).Name != "Dry" {
		t.Fatalf("dry create = %d %s", r.code, r.body)
	}
	if n := testutil.Count(t, f.db.Read, `SELECT count(*) FROM segments`); n != 1 {
		t.Fatalf("segments = %d", n)
	}

	// Get.
	r = c.admin("GET", "/segments/"+id, "")
	if r.code != 200 || r.etag != `"v1"` || decode[segments.Segment](t, r).ID != sg.ID {
		t.Fatalf("get = %d %s %s", r.code, r.etag, r.body)
	}
	if r = c.admin("GET", "/segments/999", ""); r.code != 404 || r.errCode() != "not_found:" {
		t.Fatalf("get unknown = %d %s", r.code, r.body)
	}
	if r = c.admin("GET", "/segments/abc", ""); r.code != 422 {
		t.Fatalf("get bad id = %d %s", r.code, r.body)
	}

	// Update with If-Match.
	r = c.admin("PATCH", "/segments/"+id, `{"name":"VIPs (active)","filter":{"rules":[{"field":"tag","op":"has","value":"vip"},{"field":"status","op":"is","value":"active"}]}}`, "If-Match", `"v1"`)
	if r.code != 200 || r.etag != `"v2"` {
		t.Fatalf("patch = %d %s %s", r.code, r.etag, r.body)
	}
	for _, tt := range []struct {
		name, body, ifMatch string
		status              int
		err                 string
	}{
		{"stale If-Match", `{"name":"Y"}`, `"v1"`, 412, "precondition_failed:"},
		{"bad If-Match", `{"name":"Y"}`, `nope`, 422, "validation:header.If-Match"},
		{"invalid filter", `{"filter":{"match":"most","rules":[]}}`, `"v2"`, 422, "validation:filter.match"},
		{"blank name", `{"name":" "}`, `"v2"`, 422, "validation:name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := c.admin("PATCH", "/segments/"+id, tt.body, "If-Match", tt.ifMatch)
			if r.code != tt.status || r.errCode() != tt.err {
				t.Fatalf("= %d %s (want %d %s)", r.code, r.body, tt.status, tt.err)
			}
		})
	}
	r = c.admin("PATCH", "/segments/"+id+"?dry_run=true", `{"name":"Maybe"}`, "If-Match", `W/"v2"`)
	// A dry run sends no ETag: "v3" was never stored.
	if r.code != 200 || r.etag != "" {
		t.Fatalf("dry patch = %d %s %s", r.code, r.etag, r.body)
	}
	if r = c.admin("GET", "/segments/"+id, ""); r.etag != `"v2"` || decode[segments.Segment](t, r).Name != "VIPs (active)" {
		t.Fatalf("after dry patch = %s %s", r.etag, r.body)
	}
	if r = c.admin("PATCH", "/segments/999", `{"name":"Z"}`); r.code != 404 {
		t.Fatalf("patch unknown = %d %s", r.code, r.body)
	}

	// Count and members.
	r = c.admin("GET", "/segments/"+id+"/count", "")
	if cnt := decode[segments.SegmentCounts](t, r); r.code != 200 || cnt.Total != 1 || cnt.Active != 1 {
		t.Fatalf("count = %d %s", r.code, r.body)
	}
	if !strings.Contains(string(r.body), `"missing_tags":[]`) {
		t.Fatalf("count without missing_tags: %s", r.body)
	}
	if r = c.admin("GET", "/segments/999/count", ""); r.code != 404 {
		t.Fatalf("count unknown = %d", r.code)
	}

	// Preview: live count, read scope is enough.
	r = c.do("mr_read", "POST", "/segments/preview", `{"filter":{"match":"any","rules":[{"field":"tag","op":"has","value":"customer"},{"field":"clicked","op":"any_link"}]},"sample_limit":2}`)
	p := decode[segments.SegmentPreview](t, r)
	if r.code != 200 || p.Total != 3 || p.Active != 2 || len(p.Sample) != 2 || p.Sample[0].Email != "cat@test.io" {
		t.Fatalf("preview = %d %s", r.code, r.body)
	}
	r = c.do("mr_read", "POST", "/segments/preview", `{"filter":{"rules":[{"field":"tags","op":"has_none","values":["customer","ghost"]}]}}`)
	if p := decode[segments.SegmentPreview](t, r); r.code != 200 || len(p.MissingTags) != 1 || p.MissingTags[0] != "ghost" {
		t.Fatalf("preview missing_tags = %d %s", r.code, r.body)
	}
	if r = c.admin("POST", "/segments/preview", `{"filter":{"rules":[]},"sample_limit":51}`); r.code != 422 || r.errCode() != "validation:sample_limit" {
		t.Fatalf("preview sample_limit = %d %s", r.code, r.body)
	}
	if r = c.admin("POST", "/segments/preview", `{"filter":{"rules":[{"field":"created_at","op":"within_days","days":0}]}}`); r.code != 422 ||
		r.errCode() != "validation:filter.rules[0].days" {
		t.Fatalf("preview invalid = %d %s", r.code, r.body)
	}
	if r = c.admin("POST", "/segments/preview", `{"filter":{"rules":[{"field":"attr","key":"plan","op":"eq","value":{"x":1}}]}}`); r.code != 422 ||
		r.errCode() != "validation:filter.rules[0].value" {
		t.Fatalf("preview object value = %d %s", r.code, r.body)
	}

	// Scopes: read tokens can read but not write.
	if r = c.do("mr_read", "GET", "/segments/"+id, ""); r.code != 200 {
		t.Fatalf("read get = %d", r.code)
	}
	if r = c.do("mr_read", "POST", "/segments", `{"name":"R","filter":{"rules":[]}}`); r.code != 403 || r.errCode() != "forbidden:" {
		t.Fatalf("read create = %d %s", r.code, r.body)
	}
	if r = c.do("mr_nobody", "GET", "/segments", ""); r.code != 401 || r.errCode() != "unauthorized:" {
		t.Fatalf("no auth = %d %s", r.code, r.body)
	}

	// Delete: refused while a draft broadcast targets the segment.
	testutil.Exec(t, f.db, `UPDATE broadcasts SET audience = ? WHERE id = ?`, `{"type":"segment","segment_id":`+id+`}`, f.b1)
	if r = c.admin("DELETE", "/segments/"+id, ""); r.code != 409 || r.errCode() != "conflict:" || !strings.Contains(string(r.body), fmt.Sprint(f.b1)) {
		t.Fatalf("delete referenced = %d %s", r.code, r.body)
	}
	testutil.Exec(t, f.db, `UPDATE broadcasts SET status = 'sent' WHERE id = ?`, f.b1)
	if r = c.admin("DELETE", "/segments/"+id, "", "If-Match", `"v1"`); r.code != 412 {
		t.Fatalf("stale delete = %d %s", r.code, r.body)
	}
	if r = c.admin("DELETE", "/segments/"+id+"?dry_run=true", "", "If-Match", `"v2"`); r.code != 204 {
		t.Fatalf("dry delete = %d %s", r.code, r.body)
	}
	if r = c.admin("GET", "/segments/"+id, ""); r.code != 200 {
		t.Fatalf("dry delete removed it: %d", r.code)
	}
	if r = c.admin("DELETE", "/segments/"+id, "", "If-Match", `"v2"`); r.code != 204 {
		t.Fatalf("delete = %d %s", r.code, r.body)
	}
	if r = c.admin("GET", "/segments/"+id, ""); r.code != 404 {
		t.Fatalf("get deleted = %d", r.code)
	}

	// Audit rows carry the token actor.
	recs, err := audit.New(f.db).List(context.Background(), audit.Filter{TargetType: "segment", TargetID: id})
	if err != nil || len(recs) != 3 || recs[0].Operation != "segment.delete" || recs[0].Actor.Name != "agent" {
		t.Fatalf("audit = %+v, %v", recs, err)
	}
}

func TestAPIPagination(t *testing.T) {
	f, c := newAPI(t)
	for i := range 5 {
		if _, err := f.svc.Create(context.Background(), segments.CreateInput{Name: fmt.Sprintf("S%d", i)}, false); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("too many pages")
		}
		r := c.admin("GET", "/segments?limit=2&cursor="+cursor, "")
		page := decode[api.ListBody[segments.Segment]](t, r)
		if r.code != 200 {
			t.Fatalf("list = %d %s", r.code, r.body)
		}
		for _, s := range page.Items {
			names = append(names, s.Name)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if strings.Join(names, ",") != "S0,S1,S2,S3,S4" {
		t.Fatalf("names = %v", names)
	}
	if r := c.admin("GET", "/segments?cursor=bogus", ""); r.code != 422 || r.errCode() != "validation:query.cursor" {
		t.Fatalf("bad cursor = %d %s", r.code, r.body)
	}
	if r := c.admin("GET", "/segments?limit=501", ""); r.code != 422 {
		t.Fatalf("limit 501 = %d %s", r.code, r.body)
	}

	// Members: every contact, 4 per page, then only active ones.
	all, err := f.svc.Create(context.Background(), segments.CreateInput{Name: "All"}, false)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/segments/%d/members", all.ID)
	r := c.admin("GET", path+"?limit=4", "")
	page := decode[api.ListBody[segments.SegmentMember]](t, r)
	if r.code != 200 || len(page.Items) != 4 || page.NextCursor == "" || page.Items[0].Email != "ann@example.com" {
		t.Fatalf("members page 1 = %d %s", r.code, r.body)
	}
	r = c.admin("GET", path+"?limit=4&cursor="+page.NextCursor, "")
	page = decode[api.ListBody[segments.SegmentMember]](t, r)
	if r.code != 200 || len(page.Items) != 2 || page.NextCursor != "" || page.Items[1].Email != "f_y@example.net" {
		t.Fatalf("members page 2 = %d %s", r.code, r.body)
	}
	r = c.admin("GET", path+"?status=active", "")
	page = decode[api.ListBody[segments.SegmentMember]](t, r)
	if r.code != 200 || len(page.Items) != 3 || !strings.Contains(string(r.body), `"tags":["customer","VIP"]`) {
		t.Fatalf("active members = %d %s", r.code, r.body)
	}
	if r = c.admin("GET", path+"?status=gone", ""); r.code != 422 || r.errCode() != "validation:query.status" {
		t.Fatalf("bad status = %d %s", r.code, r.body)
	}
	if r = c.admin("GET", "/segments/999/members", ""); r.code != 404 {
		t.Fatalf("members unknown = %d", r.code)
	}
}
