package broadcasts_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/broadcasts"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
	"mailroom/internal/segments"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

const (
	adminToken = "mr_admin"
	readToken  = "mr_read"
)

type fakeAuth struct{}

func (fakeAuth) AuthenticateToken(_ context.Context, tok string) (api.Principal, error) {
	switch tok {
	case adminToken:
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "1", Name: "setup-agent"}, Scope: api.ScopeAdmin}, nil
	case readToken:
		return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "2", Name: "reader"}, Scope: api.ScopeRead}, nil
	}
	return api.Principal{}, fmt.Errorf("unknown token: %w", api.ErrUnauthorized)
}

func (fakeAuth) AuthenticateSession(context.Context, string) (api.Principal, string, error) {
	return api.Principal{}, "", api.ErrUnauthorized
}

// alert is one recorded Alert call.
type alert struct {
	key, title, body string
	inTx             bool
}

type fakeAlerter struct {
	mu     sync.Mutex
	alerts []alert
}

func (f *fakeAlerter) Alert(_ context.Context, tx *sql.Tx, key, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, alert{key: key, title: title, body: body, inTx: tx != nil})
	return nil
}

func (f *fakeAlerter) all() []alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]alert(nil), f.alerts...)
}

type harness struct {
	t        *testing.T
	ctx      context.Context
	d        *db.DB
	clock    *testutil.Clock
	settings *settings.Service
	content  *content.Service
	segments *segments.Service
	runner   *jobs.Runner
	alerter  *fakeAlerter
	svc      *broadcasts.Service
	h        http.Handler
	jobsOn   bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hs := &harness{t: t, ctx: context.Background(), d: testutil.NewDB(t), clock: testutil.NewClock(time.Time{}), alerter: &fakeAlerter{}}
	hs.settings = settings.New(settings.Deps{DB: hs.d, Clock: hs.clock, Domain: "example.com"})
	from, owner, footer := "news@example.com", "owner@example.org", "Example Ltd, 1 High Street"
	if _, err := hs.settings.Update(hs.ctx, settings.Patch{
		Sender: &settings.SenderPatch{FromEmail: &from}, OwnerEmail: &owner, CompanyFooter: &footer,
	}, 0); err != nil {
		t.Fatal(err)
	}
	hs.content = content.New(content.Deps{
		DB: hs.d, Settings: hs.settings, Signer: testutil.NewSigner(t, hs.clock),
		BaseURL: "https://newsletter.example.com", Clock: hs.clock, Logger: logger,
	})
	hs.segments = segments.New(segments.Deps{DB: hs.d, Clock: hs.clock})
	hs.runner = jobs.New(jobs.Deps{
		DB: hs.d, Clock: hs.clock, Logger: logger,
		PollInterval: 10 * time.Millisecond, ProgressInterval: 5 * time.Millisecond,
	})
	hs.svc = broadcasts.New(broadcasts.Deps{
		DB: hs.d, Content: hs.content, Segments: hs.segments, Jobs: hs.runner, Alerter: hs.alerter,
		Settings: hs.settings, BaseURL: "https://newsletter.example.com/", Clock: hs.clock, Logger: logger,
	})
	hs.svc.RegisterJobs()
	a, err := api.New(api.Config{
		DB: hs.d, Clock: hs.clock, Logger: logger, Auth: fakeAuth{},
		BaseURL: "https://newsletter.example.com", Rate: 1e6, Burst: 1_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs.svc.RegisterAPI(a)
	hs.runner.RegisterAPI(a)
	hs.h = a.Handler()
	return hs
}

// startJobs runs the job runner until the test ends.
func (hs *harness) startJobs() {
	if hs.jobsOn {
		return
	}
	hs.jobsOn = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hs.runner.Run(ctx) }()
	hs.t.Cleanup(func() { cancel(); <-done })
}

// waitJob runs the job runner and waits for the job to finish.
func (hs *harness) waitJob(id int64) jobs.Job {
	hs.t.Helper()
	hs.startJobs()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	j, err := hs.runner.Wait(ctx, id)
	if err != nil {
		hs.t.Fatalf("wait job %d: %v", id, err)
	}
	return j
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (r resp) wantStatus(t *testing.T, status int) resp {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
	return r
}

// wantError checks an error response's status, code and (optional) field.
func (r resp) wantError(t *testing.T, status int, code, field string) api.ErrorBody {
	t.Helper()
	r.wantStatus(t, status)
	var e struct {
		Error api.ErrorBody `json:"error"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil || e.Error.Code == "" {
		t.Fatalf("not an error response (%d): %s", r.status, r.body)
	}
	if e.Error.Code != code || (field != "" && e.Error.Field != field) {
		t.Fatalf("error = %+v, want code %s field %s", e.Error, code, field)
	}
	return e.Error
}

func (hs *harness) do(method, path, body string, headers ...string) resp {
	hs.t.Helper()
	return hs.doAs(adminToken, method, path, body, headers...)
}

func (hs *harness) doAs(token, method, path, body string, headers ...string) resp {
	hs.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	req.RemoteAddr = "192.0.2.1:1234"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return resp{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// create creates a broadcast through the API. audience is a JSON object
// or "" for everyone.
func (hs *harness) create(name, audience string) broadcasts.Broadcast {
	hs.t.Helper()
	body := `{"name":` + jsonString(name) + `,"email":{"subject":"Hello {{first_name | \"there\"}}","body":"Hi!\n\n[Offer](https://example.com/offer)"}`
	if audience != "" {
		body += `,"audience":` + audience
	}
	body += `}`
	var b broadcasts.Broadcast
	hs.do("POST", "/broadcasts", body).wantStatus(hs.t, http.StatusCreated).decode(hs.t, &b)
	return b
}

func (hs *harness) get(id int64) broadcasts.Broadcast {
	hs.t.Helper()
	b, err := hs.svc.Get(hs.ctx, id)
	if err != nil {
		hs.t.Fatalf("get broadcast %d: %v", id, err)
	}
	return b
}

// setStatus forces a broadcast into a status (fixture shortcut).
func (hs *harness) setStatus(id int64, status, pausedReason string) {
	hs.t.Helper()
	testutil.Exec(hs.t, hs.d, `UPDATE broadcasts SET status = ?, paused_reason = ?, scheduled_at = CASE WHEN ? = 'scheduled' THEN ? ELSE scheduled_at END WHERE id = ?`,
		status, pausedReason, status, db.FormatTime(hs.clock.Now().Add(time.Hour)), id)
}

// segment creates a segment of contacts tagged tag.
func (hs *harness) segment(name, tag string) int64 {
	hs.t.Helper()
	sg, err := hs.segments.Create(hs.ctx, segments.CreateInput{Name: name, Filter: segments.SegmentFilter{
		Rules: []segments.SegmentRule{{Field: segments.FieldTag, Op: segments.OpHas, Value: tag}},
	}}, false)
	if err != nil {
		hs.t.Fatalf("create segment: %v", err)
	}
	return sg.ID
}

// contacts inserts n contacts "<prefix><i>@example.net" with a status and
// optional tag in one transaction and returns their IDs.
func (hs *harness) contacts(prefix string, n int, status core.ContactStatus, tag string) []int64 {
	hs.t.Helper()
	var tagID int64
	if tag != "" {
		tagID = testutil.InsertTag(hs.t, hs.d, tag)
	}
	ids := make([]int64, 0, n)
	now := db.FormatTime(hs.clock.Now())
	err := hs.d.WithTx(hs.ctx, func(tx *sql.Tx) error {
		for i := range n {
			var id int64
			if err := tx.QueryRow(`INSERT INTO contacts (email, status, status_changed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
				fmt.Sprintf("%s%d@example.net", prefix, i), string(status), now, now, now).Scan(&id); err != nil {
				return err
			}
			if tagID != 0 {
				if _, err := tx.Exec(`INSERT INTO contact_tags (contact_id, tag_id, source, added_at) VALUES (?, ?, 'test', ?)`, id, tagID, now); err != nil {
					return err
				}
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		hs.t.Fatalf("insert contacts: %v", err)
	}
	return ids
}

// send starts a broadcast through the API and returns the snapshot job.
func (hs *harness) send(id int64) jobs.Job {
	hs.t.Helper()
	var res broadcasts.BroadcastSendResult
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/send", id), "").wantStatus(hs.t, http.StatusAccepted).decode(hs.t, &res)
	if res.Job == nil || res.Broadcast.Status != broadcasts.StatusSending {
		hs.t.Fatalf("send result = %+v", res)
	}
	return *res.Job
}

// sendAndSnapshot sends a broadcast and waits for its snapshot.
func (hs *harness) sendAndSnapshot(id int64) broadcasts.SnapshotResult {
	hs.t.Helper()
	j := hs.waitJob(hs.send(id).ID)
	if j.Status != jobs.StatusSucceeded {
		hs.t.Fatalf("snapshot job = %+v", j)
	}
	var res broadcasts.SnapshotResult
	if err := json.Unmarshal(j.Result, &res); err != nil {
		hs.t.Fatal(err)
	}
	return res
}

func (hs *harness) count(query string, args ...any) int {
	hs.t.Helper()
	return testutil.Count(hs.t, hs.d.Read, query, args...)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// clearFrom removes the From address from the settings.
func (hs *harness) clearFrom() {
	hs.t.Helper()
	empty := ""
	if _, err := hs.settings.Update(hs.ctx, settings.Patch{Sender: &settings.SenderPatch{FromEmail: &empty}}, 0); err != nil {
		hs.t.Fatal(err)
	}
}
