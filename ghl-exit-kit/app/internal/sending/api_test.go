package sending

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

// fakeAuth accepts "Bearer admin" (scope admin) and "Bearer read" (scope read).
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

func newAPI(t *testing.T, e *env) *apiClient {
	t.Helper()
	a, err := api.New(api.Config{DB: e.db, Clock: e.clock, BaseURL: "https://newsletter.example.com", Auth: fakeAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.RegisterAPI(a)
	return &apiClient{t: t, h: a.Handler()}
}

func (c *apiClient) do(token, method, path, body string) (int, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer mr_"+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (c *apiClient) json(token, method, path, body string, wantStatus int, out any) {
	c.t.Helper()
	code, b := c.do(token, method, path, body)
	if code != wantStatus {
		c.t.Fatalf("%s %s = %d %s, want %d", method, path, code, b, wantStatus)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: decode %s: %v", method, path, b, err)
		}
	}
}

func errCode(b []byte) string {
	var e struct{ Error api.ErrorBody }
	_ = json.Unmarshal(b, &e)
	return e.Error.Code + ":" + e.Error.Field
}

func TestSendingStateAPI(t *testing.T) {
	e := newEnv(t)
	c := newAPI(t, e)
	var st Status
	c.json("read", "GET", "/sending/state", "", 200, &st)
	if st.Paused || st.RatePerSec != DefaultRate || st.SESAccount != nil {
		t.Fatalf("state = %+v", st)
	}

	// Dry run: the answer shows the pause, nothing is stored or audited.
	c.json("admin", "POST", "/sending/pause?dry_run=true", `{"reason":"checking"}`, 200, &st)
	if !st.Paused || st.Reason != "checking" || st.Source != PauseManual {
		t.Fatalf("dry run = %+v", st)
	}
	if e.state().Paused || e.count(`SELECT count(*) FROM audit_log`) != 1 { // 1 = settings.seed
		t.Fatal("dry run changed something")
	}

	c.json("admin", "POST", "/sending/pause", `{"reason":"checking"}`, 200, &st)
	if !st.Paused || st.Since == nil || !st.Since.Equal(e.clock.Now()) || !e.state().Paused {
		t.Fatalf("pause = %+v", st)
	}
	recs, err := audit.New(e.db).List(e.ctx, audit.Filter{Operation: "sending.pause"})
	if err != nil || len(recs) != 1 || recs[0].Actor.ID != "4" {
		t.Fatalf("audit = %+v %v", recs, err)
	}
	// Pausing again keeps the first reason; no body is fine.
	c.json("admin", "POST", "/sending/pause", "", 200, &st)
	if st.Reason != "checking" {
		t.Fatalf("second pause = %+v", st)
	}

	// The SES snapshot and rate show up once polled.
	if _, err := e.svc.Resume(e.ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PollAccount(e.ctx); err != nil {
		t.Fatal(err)
	}
	c.json("admin", "POST", "/sending/pause", `{}`, 200, nil)
	c.json("admin", "POST", "/sending/resume?dry_run=true", "", 200, &st)
	if st.Paused || !e.state().Paused {
		t.Fatalf("resume dry run = %+v, stored %+v", st, e.state())
	}
	c.json("admin", "POST", "/sending/resume", "", 200, &st)
	if st.Paused || e.state().Paused || st.SESAccount == nil || st.SESAccount.MaxSendRate != 14 || st.RatePerSec < 11.19 || st.RatePerSec > 11.21 {
		t.Fatalf("resume = %+v", st)
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE operation = 'sending.resume'`); n != 2 {
		t.Fatalf("resume audits = %d", n)
	}

	tests := []struct {
		name, token, method, path, body string
		status                          int
		code                            string
	}{
		{"read token cannot pause", "read", "POST", "/sending/pause", `{}`, 403, "forbidden:"},
		{"no token", "nope", "GET", "/sending/state", "", 401, "unauthorized:"},
		{"reason too long", "admin", "POST", "/sending/pause", `{"reason":"` + strings.Repeat("x", 501) + `"}`, 422, "validation:reason"},
		{"unknown field", "admin", "POST", "/sending/pause", `{"why":"x"}`, 422, "validation:why"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, b := c.do(tt.token, tt.method, tt.path, tt.body)
			if code != tt.status || errCode(b) != tt.code {
				t.Fatalf("= %d %s", code, b)
			}
		})
	}
}

func TestMessagesAPI(t *testing.T) {
	e := newEnv(t)
	c := newAPI(t, e)
	bID, emailID := e.broadcast("sending")
	contact := testutil.InsertContact(t, e.db, "alice@example.org", core.StatusActive)
	var ids []int64
	ids = append(ids, e.broadcastMessage(bID, emailID, contact))
	for i := range 4 {
		ids = append(ids, e.broadcastMessage(bID, emailID, testutil.InsertContact(t, e.db, fmt.Sprintf("m%d@example.org", i), core.StatusActive)))
	}
	e.provider.FailNext(&core.ProviderError{Code: "MessageRejected", Message: "nope"})
	e.sendDue() // ids[0] fails (send_error event), the rest are sent
	alertAt := e.clock.Advance(time.Minute)
	if err := e.svc.Alert(e.ctx, nil, "k", "T", "B"); err != nil {
		t.Fatal(err)
	}
	now := db.FormatTime(e.clock.Now())
	testutil.Exec(t, e.db, `INSERT INTO links (id, url, created_at) VALUES (9, 'https://example.com/offer', ?)`, now)
	testutil.Exec(t, e.db, `INSERT INTO clicks (link_id, message_id, contact_id, created_at) VALUES (9, ?, ?, ?)`, ids[1], contact.ID, now)
	testutil.Exec(t, e.db, `INSERT INTO clicks (link_id, message_id, contact_id, created_at) VALUES (9, NULL, NULL, ?)`, now)
	testutil.Exec(t, e.db, `INSERT INTO events (message_id, type, subtype, detail, created_at) VALUES (?, 'delivery', '', '{"x":1}', ?)`, ids[1], now)

	// Pagination, newest first.
	var page api.ListBody[Message]
	c.json("read", "GET", "/messages?limit=2&kind=broadcast", "", 200, &page)
	if len(page.Items) != 2 || page.Items[0].ID != ids[4] || page.Items[1].ID != ids[3] || page.NextCursor == "" {
		t.Fatalf("page 1 = %+v", page)
	}
	var all []int64
	cursor := ""
	for {
		var p api.ListBody[Message]
		c.json("read", "GET", "/messages?limit=2&broadcast_id="+fmt.Sprint(bID)+"&cursor="+url.QueryEscape(cursor), "", 200, &p)
		for _, m := range p.Items {
			all = append(all, m.ID)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(all) != 5 || all[4] != ids[0] {
		t.Fatalf("all pages = %v", all)
	}

	filters := []struct {
		query string
		want  int
	}{
		{"status=failed", 1},
		{"status=sent", 4},
		{"kind=alert", 1},
		{"contact_id=" + fmt.Sprint(contact.ID), 1},
		{"enrollment_id=99", 0},
		{"step_id=99", 0},
	}
	for _, f := range filters {
		var p api.ListBody[Message]
		c.json("read", "GET", "/messages?"+f.query, "", 200, &p)
		if len(p.Items) != f.want {
			t.Errorf("%s: %d items, want %d", f.query, len(p.Items), f.want)
		}
	}

	var d MessageDetail
	c.json("read", "GET", fmt.Sprintf("/messages/%d", ids[1]), "", 200, &d)
	if d.ID != ids[1] || d.Status != "sent" || d.SESMessageID == "" || d.SentAt == nil || d.BroadcastID != bID ||
		len(d.Events) != 1 || d.Events[0].Type != "delivery" || string(d.Events[0].Detail) != `{"x":1}` ||
		len(d.Clicks) != 1 || d.Clicks[0].URL != "https://example.com/offer" || d.Clicks[0].ContactID != contact.ID {
		t.Fatalf("detail = %+v", d)
	}
	c.json("read", "GET", fmt.Sprintf("/messages/%d", ids[0]), "", 200, &d)
	if d.Status != "failed" || len(d.Events) != 1 || d.Events[0].Type != "send_error" || d.Events[0].Subtype != "MessageRejected" {
		t.Fatalf("failed detail = %+v", d)
	}

	var ev api.ListBody[Event]
	c.json("read", "GET", "/events?type=send_error", "", 200, &ev)
	if len(ev.Items) != 1 || ev.Items[0].MessageID != ids[0] {
		t.Fatalf("events = %+v", ev)
	}
	c.json("read", "GET", "/events?since="+url.QueryEscape(alertAt.Format(time.RFC3339)), "", 200, &ev)
	if len(ev.Items) != 1 || ev.Items[0].Type != "delivery" {
		t.Fatalf("events since = %+v", ev)
	}
	c.json("read", "GET", fmt.Sprintf("/events?message_id=%d&limit=1", ids[1]), "", 200, &ev)
	if len(ev.Items) != 1 || ev.NextCursor != "" {
		t.Fatalf("events by message = %+v", ev)
	}

	var cl api.ListBody[Click]
	c.json("read", "GET", fmt.Sprintf("/clicks?broadcast_id=%d", bID), "", 200, &cl)
	if len(cl.Items) != 1 || cl.Items[0].URL != "https://example.com/offer" || cl.Items[0].BroadcastID != bID || cl.Items[0].MessageID != ids[1] {
		t.Fatalf("clicks = %+v", cl)
	}
	c.json("read", "GET", "/clicks?link_id=9", "", 200, &cl)
	if len(cl.Items) != 2 || cl.Items[0].ContactID != 0 {
		t.Fatalf("clicks by link = %+v", cl)
	}
	c.json("read", "GET", fmt.Sprintf("/clicks?contact_id=%d", contact.ID), "", 200, &cl)
	if len(cl.Items) != 1 {
		t.Fatalf("clicks by contact = %+v", cl)
	}

	// get-event and get-click return one row, like the lists.
	var oneEvent Event
	c.json("read", "GET", fmt.Sprintf("/events/%d", ev.Items[0].ID), "", 200, &oneEvent)
	if oneEvent.ID != ev.Items[0].ID || oneEvent.MessageID != ids[1] || oneEvent.Type != ev.Items[0].Type {
		t.Fatalf("get-event = %+v, want %+v", oneEvent, ev.Items[0])
	}
	var oneClick Click
	c.json("read", "GET", fmt.Sprintf("/clicks/%d", cl.Items[0].ID), "", 200, &oneClick)
	if oneClick != cl.Items[0] {
		t.Fatalf("get-click = %+v, want %+v", oneClick, cl.Items[0])
	}

	errs := []struct {
		name, path string
		status     int
		code       string
	}{
		{"missing message", "/messages/999999", 404, "not_found:"},
		{"bad id", "/messages/0", 422, "validation:path.id"},
		{"bad cursor", "/messages?cursor=bogus", 422, "validation:query.cursor"},
		{"bad status", "/messages?status=lost", 422, "validation:query.status"},
		{"bad limit", "/messages?limit=501", 422, "validation:query.limit"},
		{"bad event type", "/events?type=open", 422, "validation:query.type"},
		{"bad since", "/events?since=yesterday", 422, "validation:query.since"},
		{"bad click filter", "/clicks?link_id=0", 422, "validation:query.link_id"},
		{"missing event", "/events/999999", 404, "not_found:"},
		{"missing click", "/clicks/999999", 404, "not_found:"},
	}
	for _, tt := range errs {
		t.Run(tt.name, func(t *testing.T) {
			code, b := c.do("read", "GET", tt.path, "")
			if code != tt.status || errCode(b) != tt.code {
				t.Fatalf("= %d %s", code, b)
			}
		})
	}
}
