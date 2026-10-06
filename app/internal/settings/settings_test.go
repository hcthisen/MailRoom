package settings_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/config"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

func testConfig() config.Config {
	return config.Config{
		Domain:        "example.com",
		FromName:      "Example News",
		FromEmail:     "news@example.com",
		ReplyToEmail:  "hello@example.com",
		CompanyFooter: "Example Ltd, 1 High Street",
		OwnerEmail:    "owner@gmail.com",
		Timezone:      "Europe/London",
	}
}

func newService(t *testing.T) (*settings.Service, *db.DB) {
	t.Helper()
	d := testutil.NewDB(t)
	return settings.New(settings.Deps{DB: d, Clock: testutil.NewClock(time.Time{}), Domain: "example.com"}), d
}

func TestSeedFromConfig(t *testing.T) {
	svc, d := newService(t)
	ctx := context.Background()

	// Before seeding, Get returns the defaults with the domain.
	st, err := svc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Domain != "example.com" || !st.Tracking.Clicks || st.Tracking.Opens || st.Sending.RateFraction != 0.8 || st.Version != 0 {
		t.Fatalf("defaults = %+v", st)
	}

	seeded, err := svc.SeedFromConfig(ctx, testConfig())
	if err != nil || !seeded {
		t.Fatalf("seed = %v %v", seeded, err)
	}
	st, _ = svc.Get(ctx)
	want := settings.Sender{FromName: "Example News", FromEmail: "news@example.com", ReplyTo: "hello@example.com"}
	if st.Sender != want || st.OwnerEmail != "owner@gmail.com" || st.Timezone != "Europe/London" ||
		st.CompanyFooter != "Example Ltd, 1 High Street" || st.Version != 1 || st.Domain != "example.com" {
		t.Fatalf("seeded = %+v", st)
	}
	if st.CircuitBreaker != (settings.CircuitBreaker{MinMessages: 200, HardBounceRate: 0.05, ComplaintRate: 0.001}) || st.DefaultSendWindow != nil {
		t.Fatalf("seeded defaults = %+v", st)
	}
	if st.AlertAddress() != "owner@gmail.com" || st.Location().String() != "Europe/London" {
		t.Fatalf("helpers = %s %s", st.AlertAddress(), st.Location())
	}

	// Later starts never overwrite what the API owns.
	name := "Changed"
	if _, err := svc.Update(ctx, settings.Patch{Sender: &settings.SenderPatch{FromName: &name}}, 0); err != nil {
		t.Fatal(err)
	}
	seeded, err = svc.SeedFromConfig(ctx, testConfig())
	if err != nil || seeded {
		t.Fatalf("second seed = %v %v", seeded, err)
	}
	if st, _ := svc.Get(ctx); st.Sender.FromName != "Changed" {
		t.Fatalf("seed overwrote settings: %+v", st.Sender)
	}
	if n := testutil.Count(t, d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'settings.seed' AND actor_type = 'system'`); n != 1 {
		t.Fatalf("seed audit rows = %d", n)
	}

	// An off-domain sender in the environment is refused.
	other, _ := newService(t)
	bad := testConfig()
	bad.FromEmail = "news@other.org"
	if _, err := other.SeedFromConfig(ctx, bad); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("off-domain seed = %v", err)
	}
}

func TestValidate(t *testing.T) {
	base := settings.Defaults()
	base.Sender.FromEmail = "news@example.com"
	win := func(days []int, start, end string) *settings.SendWindow {
		return &settings.SendWindow{Days: days, Start: start, End: end}
	}
	tests := []struct {
		name  string
		mod   func(*settings.Settings)
		field string
	}{
		{"ok", func(s *settings.Settings) {}, ""},
		{"from on subdomain", func(s *settings.Settings) { s.Sender.FromEmail = "a@mail.example.com" }, "sender.from_email"},
		{"from elsewhere", func(s *settings.Settings) { s.Sender.FromEmail = "a@evil.com" }, "sender.from_email"},
		{"from invalid", func(s *settings.Settings) { s.Sender.FromEmail = "nope" }, "sender.from_email"},
		{"reply-to elsewhere", func(s *settings.Settings) { s.Sender.ReplyTo = "a@gmail.com" }, "sender.reply_to"},
		{"reply-to empty ok", func(s *settings.Settings) { s.Sender.ReplyTo = "" }, ""},
		{"from name header injection", func(s *settings.Settings) { s.Sender.FromName = "A\r\nBcc: x@y.z" }, "sender.from_name"},
		{"owner anywhere ok", func(s *settings.Settings) { s.OwnerEmail = "me@gmail.com" }, ""},
		{"alert invalid", func(s *settings.Settings) { s.AlertEmail = "x" }, "alert_email"},
		{"timezone", func(s *settings.Settings) { s.Timezone = "Mars/Olympus" }, "timezone"},
		{"timezone empty", func(s *settings.Settings) { s.Timezone = "" }, "timezone"},
		{"timezone local", func(s *settings.Settings) { s.Timezone = "Local" }, "timezone"},
		{"rate override", func(s *settings.Settings) { s.Sending.RateOverridePerSec = -1 }, "sending.rate_override_per_sec"},
		{"rate fraction zero", func(s *settings.Settings) { s.Sending.RateFraction = 0 }, "sending.rate_fraction"},
		{"rate fraction big", func(s *settings.Settings) { s.Sending.RateFraction = 1.5 }, "sending.rate_fraction"},
		{"min messages", func(s *settings.Settings) { s.CircuitBreaker.MinMessages = 0 }, "circuit_breaker.min_messages"},
		{"bounce rate", func(s *settings.Settings) { s.CircuitBreaker.HardBounceRate = 0 }, "circuit_breaker.hard_bounce_rate"},
		{"complaint rate", func(s *settings.Settings) { s.CircuitBreaker.ComplaintRate = 2 }, "circuit_breaker.complaint_rate"},
		{"window ok", func(s *settings.Settings) { s.DefaultSendWindow = win([]int{1, 2, 3, 4, 5}, "08:00", "20:00") }, ""},
		{"window no days", func(s *settings.Settings) { s.DefaultSendWindow = win(nil, "08:00", "20:00") }, "default_send_window.days"},
		{"window day 0", func(s *settings.Settings) { s.DefaultSendWindow = win([]int{0}, "08:00", "20:00") }, "default_send_window.days"},
		{"window dup day", func(s *settings.Settings) { s.DefaultSendWindow = win([]int{1, 1}, "08:00", "20:00") }, "default_send_window.days"},
		{"window bad start", func(s *settings.Settings) { s.DefaultSendWindow = win([]int{1}, "8am", "20:00") }, "default_send_window.start"},
		{"window end before start", func(s *settings.Settings) { s.DefaultSendWindow = win([]int{1}, "20:00", "08:00") }, "default_send_window.end"},
		{"window 24:00", func(s *settings.Settings) { s.DefaultSendWindow = win([]int{1}, "08:00", "24:00") }, "default_send_window.end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base
			tt.mod(&s)
			_, err := settings.Validate(s, "example.com")
			var ve *core.ValidationError
			switch {
			case tt.field == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tt.field != "" && (!errors.As(err, &ve) || ve.Field != tt.field):
				t.Fatalf("err = %v, want field %s", err, tt.field)
			}
		})
	}
	s := base
	s.Sender.FromEmail = "  News@Example.COM "
	if got, err := settings.Validate(s, "example.com"); err != nil || got.Sender.FromEmail != "news@example.com" {
		t.Fatalf("normalised = %q %v", got.Sender.FromEmail, err)
	}
}

type fakeAuth struct{}

func (fakeAuth) AuthenticateToken(context.Context, string) (api.Principal, error) {
	return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "4", Name: "agent"}, Scope: api.ScopeAdmin}, nil
}

func (fakeAuth) AuthenticateSession(context.Context, string) (api.Principal, string, error) {
	return api.Principal{}, "", api.ErrUnauthorized
}

func TestAPI(t *testing.T) {
	svc, d := newService(t)
	ctx := context.Background()
	if _, err := svc.SeedFromConfig(ctx, testConfig()); err != nil {
		t.Fatal(err)
	}
	a, err := api.New(api.Config{DB: d, BaseURL: "https://x.example.com", Auth: fakeAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	svc.RegisterAPI(a)
	h := a.Handler()
	call := func(method, body, ifMatch string) (int, string, []byte) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, "/api/v1/settings", rd)
		req.Header.Set("Authorization", "Bearer mr_x")
		req.Header.Set("Content-Type", "application/merge-patch+json")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get("ETag"), rec.Body.Bytes()
	}
	errField := func(b []byte) string {
		var e struct{ Error api.ErrorBody }
		_ = json.Unmarshal(b, &e)
		return e.Error.Code + ":" + e.Error.Field
	}

	code, etag, body := call("GET", "", "")
	var st settings.Settings
	if err := json.Unmarshal(body, &st); code != 200 || err != nil || etag != `"v1"` || st.Domain != "example.com" {
		t.Fatalf("get = %d %s %s", code, etag, body)
	}

	code, etag, body = call("PATCH", `{"sender":{"from_name":"Weekly"},"tracking":{"opens":true},"default_send_window":{"days":[1,2,3,4,5],"start":"08:00","end":"20:00"}}`, `"v1"`)
	if err := json.Unmarshal(body, &st); code != 200 || err != nil || etag != `"v2"` {
		t.Fatalf("patch = %d %s %s", code, etag, body)
	}
	if st.Sender.FromName != "Weekly" || st.Sender.FromEmail != "news@example.com" || !st.Tracking.Opens || !st.Tracking.Clicks ||
		st.DefaultSendWindow == nil || st.DefaultSendWindow.End != "20:00" {
		t.Fatalf("patched = %+v", st)
	}
	// The cache was refreshed.
	if got, _ := svc.Get(ctx); got.Sender.FromName != "Weekly" || got.Version != 2 {
		t.Fatalf("Get after patch = %+v", got)
	}

	// Audit: actor and changed fields only.
	recs, err := audit.New(d).List(ctx, audit.Filter{Operation: "settings.update"})
	if err != nil || len(recs) != 1 || recs[0].Actor.ID != "4" || recs[0].Actor.Type != "token" {
		t.Fatalf("audit = %+v %v", recs, err)
	}
	var changes map[string]map[string]any
	_ = json.Unmarshal(recs[0].Changes, &changes)
	if len(changes) != 3 || changes["sender.from_name"]["to"] != "Weekly" || changes["tracking.opens"]["from"] != false ||
		changes["default_send_window"]["from"] != nil {
		t.Fatalf("changes = %s", recs[0].Changes)
	}

	tests := []struct {
		name, body, ifMatch string
		status              int
		err                 string
	}{
		{"stale If-Match", `{"company_footer":"x"}`, `"v1"`, 412, "precondition_failed:"},
		{"bad If-Match", `{"company_footer":"x"}`, `"one"`, 422, "validation:header.If-Match"},
		{"domain is read-only", `{"domain":"other.com"}`, "", 422, "validation:domain"},
		{"version is read-only", `{"version":9}`, "", 422, "validation:version"},
		{"off-domain from", `{"sender":{"from_email":"a@gmail.com"}}`, "", 422, "validation:sender.from_email"},
		{"bad timezone", `{"timezone":"Nowhere/City"}`, "", 422, "validation:timezone"},
		{"schema: fraction", `{"sending":{"rate_fraction":2}}`, "", 422, "validation:sending.rate_fraction"},
		{"schema: type", `{"tracking":{"clicks":"yes"}}`, "", 422, "validation:tracking.clicks"},
		{"bad window", `{"default_send_window":{"days":[1],"start":"20:00","end":"08:00"}}`, "", 422, "validation:default_send_window.end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, body := call("PATCH", tt.body, tt.ifMatch)
			if code != tt.status || errField(body) != tt.err {
				t.Fatalf("= %d %s", code, body)
			}
		})
	}

	// null removes the window; a no-op patch keeps the version.
	code, etag, body = call("PATCH", `{"default_send_window":null}`, `W/"v2"`)
	if err := json.Unmarshal(body, &st); err != nil || code != 200 || etag != `"v3"` || st.DefaultSendWindow != nil {
		t.Fatalf("clear window = %d %s %s", code, etag, body)
	}
	if code, etag, _ = call("PATCH", `{"sender":{"from_name":"Weekly"}}`, ""); code != 200 || etag != `"v3"` {
		t.Fatalf("no-op = %d %s", code, etag)
	}
	if n := testutil.Count(t, d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'settings.update'`); n != 2 {
		t.Fatalf("audit rows = %d", n)
	}
}

func TestRawKeys(t *testing.T) {
	svc, d := newService(t)
	ctx := context.Background()
	if _, err := svc.GetRaw(ctx, nil, "layout"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	err := d.WithTx(ctx, func(tx *sql.Tx) error {
		return svc.PutRaw(ctx, tx, "layout", map[string]string{"color": "#123456"})
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := svc.GetRaw(ctx, nil, "layout")
	if err != nil || string(raw) != `{"color":"#123456"}` {
		t.Fatalf("layout = %s %v", raw, err)
	}
	if err := svc.PutRaw(ctx, d.Write, "layout", json.RawMessage(`{"color":"#000000"}`)); err != nil {
		t.Fatal(err)
	}
	if raw, _ := svc.GetRaw(ctx, d.Write, "layout"); string(raw) != `{"color":"#000000"}` {
		t.Fatalf("overwritten = %s", raw)
	}
	if err := svc.PutRaw(ctx, d.Write, settings.Key, map[string]any{}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("app key = %v", err)
	}
	if err := svc.PutRaw(ctx, d.Write, "x", json.RawMessage(`{`)); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("invalid json = %v", err)
	}
}

func TestSendWindow(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	w := settings.SendWindow{Days: []int{1, 2, 3, 4, 5}, Start: "08:00", End: "20:00"}
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04", s, london)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		now      string
		contains bool
		next     string
	}{
		{"2026-01-05 09:00", true, "2026-01-05 09:00"},  // Monday inside
		{"2026-01-05 07:59", false, "2026-01-05 08:00"}, // before opening
		{"2026-01-05 20:00", false, "2026-01-06 08:00"}, // end is exclusive
		{"2026-01-09 21:00", false, "2026-01-12 08:00"}, // Friday evening -> Monday
		{"2026-01-10 12:00", false, "2026-01-12 08:00"}, // Saturday
		{"2026-03-29 12:00", false, "2026-03-30 08:00"}, // DST change weekend
		{"2026-07-01 08:30", true, "2026-07-01 08:30"},  // summer time
	}
	for _, tt := range tests {
		now := at(tt.now)
		if got := w.Contains(now, london); got != tt.contains {
			t.Errorf("Contains(%s) = %v", tt.now, got)
		}
		if got := w.NextOpen(now, london); !got.Equal(at(tt.next)) {
			t.Errorf("NextOpen(%s) = %s, want %s", tt.now, got.In(london), tt.next)
		}
	}
	if (settings.SendWindow{Days: []int{1}, Start: "x", End: "y"}).Contains(at("2026-01-05 09:00"), london) {
		t.Fatal("invalid window contains a time")
	}
}
