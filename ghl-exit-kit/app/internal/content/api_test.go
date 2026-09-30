package content_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// fakeAuth accepts "mr_read" as a read token and any other token as admin.
type fakeAuth struct{}

func (fakeAuth) AuthenticateToken(_ context.Context, tok string) (api.Principal, error) {
	if tok == "mr_read" {
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "2", Name: "reader"}, Scope: api.ScopeRead}, nil
	}
	return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "1", Name: "agent"}, Scope: api.ScopeAdmin}, nil
}

func (fakeAuth) AuthenticateSession(context.Context, string) (api.Principal, string, error) {
	return api.Principal{}, "", api.ErrUnauthorized
}

type apiClient struct {
	t *testing.T
	f *fixture
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

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func newAPI(t *testing.T) *apiClient {
	t.Helper()
	f := newFixture(t)
	a, err := api.New(api.Config{DB: f.d, Clock: f.clock, BaseURL: testBaseURL, Auth: fakeAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.RegisterAPI(a)
	return &apiClient{t: t, f: f, h: a.Handler()}
}

// do sends a request; headers are name, value pairs.
func (c *apiClient) do(method, path string, body any, headers ...string) response {
	c.t.Helper()
	var rd io.Reader
	ct := "application/json"
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case *bytes.Buffer:
		rd = b
		ct = ""
	default:
		js, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(js)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer mr_admin")
	if rd != nil && ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return response{code: rec.Code, etag: rec.Header().Get("ETag"), body: rec.Body.Bytes()}
}

func TestAPIEmails(t *testing.T) {
	c := newAPI(t)

	// Create, with the error format for invalid content.
	r := c.do("POST", "/api/v1/emails", map[string]any{"name": "Welcome", "subject": "Hi {{first_name}}", "body": "Hello **{{first_name | \"there\"}}**"})
	var e content.Email
	r.decode(t, &e)
	if r.code != 201 || e.ID == 0 || r.etag != `"v1"` || e.Format != "markdown" {
		t.Fatalf("create = %d %s %s", r.code, r.etag, r.body)
	}
	for _, tt := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"subject": "S", "body": "Hi {{nickname}}"}, "validation:body"},
		{map[string]any{"subject": "S", "body": "x", "format": "mjml"}, "validation:format"},
		{map[string]any{"body": "x"}, "validation:subject"},
		{map[string]any{"subject": strings.Repeat("s", 301), "body": "x"}, "validation:subject"},
		{map[string]any{"subject": "  ", "body": "x"}, "validation:subject"},
	} {
		if r := c.do("POST", "/api/v1/emails", tt.body); r.code != 422 || r.errCode() != tt.want {
			t.Errorf("create %v = %d %s, want %s", tt.body, r.code, r.body, tt.want)
		}
	}

	// dry_run validates and returns the would-be email without saving it.
	r = c.do("POST", "/api/v1/emails?dry_run=true", map[string]any{"subject": "Dry", "body": "x"})
	if r.code != 200 || testutil.Count(t, c.f.d.Read, `SELECT count(*) FROM emails`) != 1 {
		t.Fatalf("dry run = %d %s", r.code, r.body)
	}

	// Get, 404 format.
	r = c.do("GET", "/api/v1/emails/"+itoa(e.ID), nil)
	if r.code != 200 || r.etag != `"v1"` {
		t.Fatalf("get = %d %s", r.code, r.body)
	}
	if r := c.do("GET", "/api/v1/emails/999", nil); r.code != 404 || r.errCode() != "not_found:" {
		t.Fatalf("get missing = %d %s", r.code, r.body)
	}

	// PATCH with If-Match: 412 on a stale version, 422 on a bad header.
	r = c.do("PATCH", "/api/v1/emails/"+itoa(e.ID), map[string]any{"subject": "New"}, "If-Match", `"v1"`)
	if r.code != 200 || r.etag != `"v2"` {
		t.Fatalf("patch = %d %s", r.code, r.body)
	}
	if r := c.do("PATCH", "/api/v1/emails/"+itoa(e.ID), map[string]any{"subject": "Newer"}, "If-Match", `"v1"`); r.code != 412 || r.errCode() != "precondition_failed:" {
		t.Fatalf("stale patch = %d %s", r.code, r.body)
	}
	if r := c.do("PATCH", "/api/v1/emails/"+itoa(e.ID), map[string]any{"subject": "Newer"}, "If-Match", "garbage"); r.code != 422 {
		t.Fatalf("bad If-Match = %d %s", r.code, r.body)
	}
	r = c.do("PATCH", "/api/v1/emails/"+itoa(e.ID)+"?dry_run=true", map[string]any{"subject": "Dry"}, "If-Match", `"v2"`)
	var dry content.Email
	r.decode(t, &dry)
	if r.code != 200 || dry.Subject != "Dry" || dry.Version != 3 {
		t.Fatalf("dry patch = %d %s", r.code, r.body)
	}
	if got, _ := c.f.svc.GetEmail(c.f.ctx, nil, e.ID); got.Subject != "New" || got.Version != 2 {
		t.Fatalf("dry run changed the email: %+v", got)
	}

	// List with pagination.
	for i := 0; i < 3; i++ {
		c.do("POST", "/api/v1/emails", map[string]any{"subject": "More " + itoa(int64(i)), "body": "x"})
	}
	var page api.ListBody[content.EmailSummary]
	r = c.do("GET", "/api/v1/emails?limit=3", nil)
	r.decode(t, &page)
	if r.code != 200 || len(page.Items) != 3 || page.NextCursor == "" {
		t.Fatalf("page 1 = %d %s", r.code, r.body)
	}
	r = c.do("GET", "/api/v1/emails?limit=3&cursor="+page.NextCursor, nil)
	var page2 api.ListBody[content.EmailSummary]
	r.decode(t, &page2)
	if len(page2.Items) != 1 || page2.NextCursor != "" || page2.Items[0].ID <= page.Items[2].ID {
		t.Fatalf("page 2 = %s", r.body)
	}
	if r := c.do("GET", "/api/v1/emails?cursor=bogus", nil); r.code != 422 || r.errCode() != "validation:query.cursor" {
		t.Fatalf("bad cursor = %d %s", r.code, r.body)
	}
	if strings.Contains(string(r.body), `"body":`) {
		t.Fatal("lists carry no bodies")
	}

	// Delete: 409 while in use, 204 otherwise.
	seq := testutil.InsertSequence(t, c.f.d, "Drip")
	testutil.InsertStep(t, c.f.d, seq, 1, 0, e.ID)
	if r := c.do("DELETE", "/api/v1/emails/"+itoa(e.ID), nil); r.code != 409 || r.errCode() != "conflict:" {
		t.Fatalf("delete used = %d %s", r.code, r.body)
	}
	last := page2.Items[0].ID
	if r := c.do("DELETE", "/api/v1/emails/"+itoa(last), nil, "If-Match", `"v9"`); r.code != 412 {
		t.Fatalf("delete stale = %d %s", r.code, r.body)
	}
	if r := c.do("DELETE", "/api/v1/emails/"+itoa(last), nil, "If-Match", `"v1"`); r.code != 204 {
		t.Fatalf("delete = %d %s", r.code, r.body)
	}
}

func TestAPIPreviewsAndTest(t *testing.T) {
	c := newAPI(t)
	contact := c.f.contact("hal@example.com", "Hal", nil)
	em := c.f.createEmail(content.EmailInput{Subject: `Hi {{first_name | "you"}}`, Body: "[x](https://example.com)"})

	var p content.PreviewBody
	r := c.do("POST", "/api/v1/emails/"+itoa(em.ID)+"/preview", map[string]any{"contact_id": contact.ID})
	r.decode(t, &p)
	if r.code != 200 || p.Subject != "Hi Hal" || !strings.Contains(p.HTML, `href="https://example.com"`) || !strings.Contains(p.Text, "x (https://example.com)") {
		t.Fatalf("preview = %d %s", r.code, r.body)
	}
	r = c.do("POST", "/api/v1/emails/"+itoa(em.ID)+"/preview", nil)
	r.decode(t, &p)
	if r.code != 200 || p.Subject != "Hi you" {
		t.Fatalf("preview without body = %d %s", r.code, r.body)
	}
	if r := c.do("POST", "/api/v1/emails/"+itoa(em.ID)+"/preview", map[string]any{"contact_id": 999}); r.code != 422 || r.errCode() != "validation:contact_id" {
		t.Fatalf("preview unknown contact = %d %s", r.code, r.body)
	}
	if r := c.do("POST", "/api/v1/emails/999/preview", map[string]any{}); r.code != 404 {
		t.Fatalf("preview missing email = %d %s", r.code, r.body)
	}

	r = c.do("POST", "/api/v1/emails/render", map[string]any{"subject": "Live {{first_name}}", "format": "html", "body": "<p>Hi</p>", "contact_id": contact.ID})
	r.decode(t, &p)
	if r.code != 200 || p.Subject != "Live Hal" || !strings.Contains(p.HTML, "<p>Hi</p>") {
		t.Fatalf("render = %d %s", r.code, r.body)
	}
	if r := c.do("POST", "/api/v1/emails/render", map[string]any{"subject": "S", "body": "{{x}}"}); r.code != 422 || r.errCode() != "validation:body" {
		t.Fatalf("render invalid = %d %s", r.code, r.body)
	}

	// Previews are read-only: a read token may call them, but not writes.
	req := httptest.NewRequest("POST", "/api/v1/emails/"+itoa(em.ID)+"/preview", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer mr_read")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("read-scope preview = %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest("POST", "/api/v1/emails/"+itoa(em.ID)+"/test", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer mr_read")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("read-scope test send = %d %s", rec.Code, rec.Body)
	}

	// Test send.
	var out struct {
		MessageIDs []int64  `json:"message_ids"`
		To         []string `json:"to"`
	}
	r = c.do("POST", "/api/v1/emails/"+itoa(em.ID)+"/test", map[string]any{"to": []string{"A@example.com", "b@example.com"}, "contact_id": contact.ID})
	r.decode(t, &out)
	if r.code != 202 || len(out.MessageIDs) != 2 || out.To[0] != "a@example.com" {
		t.Fatalf("test send = %d %s", r.code, r.body)
	}
	if n := testutil.Count(t, c.f.d.Read, `SELECT count(*) FROM messages WHERE kind = 'test' AND email_id = ?`, em.ID); n != 2 {
		t.Fatalf("test messages = %d", n)
	}
	r = c.do("POST", "/api/v1/emails/"+itoa(em.ID)+"/test", map[string]any{"to": []string{"1@x.io", "2@x.io", "3@x.io", "4@x.io", "5@x.io", "6@x.io"}})
	if r.code != 422 {
		t.Fatalf("6 recipients = %d %s", r.code, r.body)
	}
	r = c.do("POST", "/api/v1/emails/"+itoa(em.ID)+"/test", map[string]any{})
	r.decode(t, &out)
	if r.code != 202 || len(out.To) != 1 || out.To[0] != "owner@example.org" {
		t.Fatalf("owner test send = %d %s", r.code, r.body)
	}
	if n := testutil.Count(t, c.f.d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'email.test_send' AND actor_name = 'agent'`); n != 2 {
		t.Fatalf("test send audit = %d", n)
	}
}

func TestAPILayout(t *testing.T) {
	c := newAPI(t)
	r := c.do("GET", "/api/v1/layout", nil)
	var l content.Layout
	r.decode(t, &l)
	if r.code != 200 || r.etag != api.ETag(l.Version) || l.BrandColor != content.DefaultBrandColor {
		t.Fatalf("get = %d %s %s", r.code, r.etag, r.body)
	}
	etag := r.etag

	if r := c.do("PUT", "/api/v1/layout", map[string]any{"custom_html": "<p>{{content}}</p>"}, "If-Match", etag); r.code != 422 || r.errCode() != "validation:custom_html" {
		t.Fatalf("E-4 = %d %s", r.code, r.body)
	}
	body := map[string]any{"logo_url": "https://cdn.example.com/l.png", "brand_color": "#123456"}
	r = c.do("PUT", "/api/v1/layout?dry_run=true", body, "If-Match", etag)
	if r.code != 200 || r.etag != "" {
		t.Fatalf("dry put = %d %q %s (a dry run has no ETag)", r.code, r.etag, r.body)
	}
	if got := c.do("GET", "/api/v1/layout", nil); got.etag != etag {
		t.Fatal("dry run must not change the layout")
	}
	r = c.do("PUT", "/api/v1/layout", body, "If-Match", etag)
	r.decode(t, &l)
	if r.code != 200 || r.etag == etag || l.BrandColor != "#123456" {
		t.Fatalf("put = %d %s", r.code, r.body)
	}
	saved := r.etag
	if r := c.do("PUT", "/api/v1/layout", map[string]any{"brand_color": "#000000"}, "If-Match", etag); r.code != 412 {
		t.Fatalf("stale put = %d %s", r.code, r.body)
	}

	var p content.PreviewBody
	r = c.do("POST", "/api/v1/layout/preview", map[string]any{"brand_color": "#abcdef", "custom_html": `<div>{{content}}<p>{{footer}}</p><a href="{{unsubscribe_url}}">u</a></div>`})
	r.decode(t, &p)
	if r.code != 200 || !strings.Contains(p.HTML, `<a href="#preview">u</a>`) || !strings.Contains(p.HTML, "Your headline") {
		t.Fatalf("layout preview = %d %s", r.code, r.body)
	}
	if r := c.do("POST", "/api/v1/layout/preview", map[string]any{"email_id": 42}); r.code != 422 || r.errCode() != "validation:email_id" {
		t.Fatalf("preview unknown email = %d %s", r.code, r.body)
	}
	if got := c.do("GET", "/api/v1/layout", nil); got.etag != saved {
		t.Fatal("preview must not change the layout")
	}
}

func TestAPIMedia(t *testing.T) {
	c := newAPI(t)
	upload := func(name string, data []byte) response {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fw, err := w.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(data)
		_ = w.Close()
		return c.do("POST", "/api/v1/media", &buf, "Content-Type", w.FormDataContentType())
	}

	r := upload("logo.png", pngData)
	var m content.Media
	r.decode(t, &m)
	if r.code != 201 || m.ContentType != "image/png" || m.Filename != "logo.png" || !strings.HasPrefix(m.URL, testBaseURL+"/media/") {
		t.Fatalf("upload = %d %s", r.code, r.body)
	}
	if r := upload("evil.png", []byte("<svg><script>alert(1)</script></svg>")); r.code != 422 || r.errCode() != "validation:file" {
		t.Fatalf("svg = %d %s", r.code, r.body)
	}
	big := append(append([]byte{}, pngData...), make([]byte, content.MaxMediaBytes+1024)...)
	if r := upload("big.png", big); r.code != 422 && r.code != 413 {
		t.Fatalf("too big = %d %s", r.code, r.body)
	}
	if r := c.do("POST", "/api/v1/media", map[string]any{"file": "x"}); r.code < 400 || r.code >= 500 {
		t.Fatalf("json upload = %d %s", r.code, r.body)
	}

	var list api.ListBody[content.Media]
	r = c.do("GET", "/api/v1/media", nil)
	r.decode(t, &list)
	if r.code != 200 || len(list.Items) != 1 || list.Items[0].ID != m.ID {
		t.Fatalf("list = %d %s", r.code, r.body)
	}
	if r := c.do("GET", "/api/v1/media/"+itoa(m.ID), nil); r.code != 200 {
		t.Fatalf("get = %d %s", r.code, r.body)
	}
	if r := c.do("DELETE", "/api/v1/media/"+itoa(m.ID), nil); r.code != 204 {
		t.Fatalf("delete = %d %s", r.code, r.body)
	}
	if r := c.do("DELETE", "/api/v1/media/"+itoa(m.ID), nil); r.code != 404 || r.errCode() != "not_found:" {
		t.Fatalf("delete again = %d %s", r.code, r.body)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestAPIEmailUsage: get-email-usage over HTTP lists the steps and
// broadcasts that use an email (read scope suffices) and is 404 for an
// unknown one.
func TestAPIEmailUsage(t *testing.T) {
	c := newAPI(t)
	var e content.Email
	c.do("POST", "/api/v1/emails", map[string]any{"subject": "Used", "body": "x"}).decode(t, &e)
	seq := testutil.InsertSequence(t, c.f.d, "Welcome")
	step := testutil.InsertStep(t, c.f.d, seq, 1, 60, e.ID)
	b := testutil.InsertBroadcast(t, c.f.d, "May news", e.ID)

	req := httptest.NewRequest("GET", "/api/v1/emails/"+itoa(e.ID)+"/usage", nil)
	req.Header.Set("Authorization", "Bearer mr_read")
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	var u content.EmailUsage
	if err := json.Unmarshal(rec.Body.Bytes(), &u); rec.Code != 200 || err != nil {
		t.Fatalf("usage = %d %s", rec.Code, rec.Body)
	}
	if len(u.Steps) != 1 || u.Steps[0].StepID != step || u.Steps[0].SequenceName != "Welcome" ||
		len(u.Broadcasts) != 1 || u.Broadcasts[0].ID != b || u.PendingMessages != 0 {
		t.Fatalf("usage = %+v", u)
	}
	if r := c.do("GET", "/api/v1/emails/999/usage", nil); r.code != 404 || r.errCode() != "not_found:" {
		t.Fatalf("unknown email usage = %d %s", r.code, r.body)
	}
}
