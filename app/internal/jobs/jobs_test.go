package jobs_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
	"mailroom/internal/testutil"
)

type fixture struct {
	d      *db.DB
	clock  *testutil.Clock
	runner *jobs.Runner
	cancel context.CancelFunc
	done   chan error
}

func newFixture(t *testing.T, d *db.DB) *fixture {
	t.Helper()
	if d == nil {
		d = testutil.NewDB(t)
	}
	f := &fixture{d: d, clock: testutil.NewClock(time.Time{})}
	f.runner = jobs.New(jobs.Deps{
		DB: d, Clock: f.clock, Workers: 2, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: 20 * time.Millisecond, ProgressInterval: 10 * time.Millisecond,
	})
	return f
}

func (f *fixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel, f.done = cancel, make(chan error, 1)
	go func() { f.done <- f.runner.Run(ctx) }()
	t.Cleanup(f.stop)
}

func (f *fixture) stop() {
	if f.cancel != nil {
		f.cancel()
		<-f.done
		f.cancel = nil
	}
}

func wait(t *testing.T, r *jobs.Runner, id int64) jobs.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	j, err := r.Wait(ctx, id)
	if err != nil {
		t.Fatalf("wait %d: %v", id, err)
	}
	return j
}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func progressOf(t *testing.T, d *db.DB, id int64) float64 {
	t.Helper()
	var p float64
	if err := d.Read.QueryRow(`SELECT progress FROM jobs WHERE id = ?`, id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

type sumParams struct {
	Numbers []int `json:"numbers"`
}

func TestLifecycle(t *testing.T) {
	f := newFixture(t, nil)
	release := make(chan struct{})
	f.runner.Register("sum", func(ctx context.Context, job jobs.JobContext) (any, error) {
		var p sumParams
		if err := job.Params(&p); err != nil {
			return nil, err
		}
		if job.Type() != "sum" || job.ID() == 0 || job.Resumed() {
			return nil, errors.New("bad job context")
		}
		job.SetProgress(0.5)
		<-release
		total := 0
		for _, n := range p.Numbers {
			total += n
		}
		a := core.ActorFromContext(ctx)
		return map[string]any{"total": total, "actor": a.String(), "name": a.Name}, nil
	})
	f.runner.Register("fail", func(ctx context.Context, job jobs.JobContext) (any, error) {
		job.SetProgress(0.25)
		return map[string]int{"done": 3}, errors.New("row 4 is invalid")
	})
	f.runner.Register("panic", func(ctx context.Context, job jobs.JobContext) (any, error) {
		panic("kaboom")
	})

	ctx := core.WithActor(context.Background(), core.Actor{Type: core.ActorToken, ID: "5", Name: "agent"})
	sub, err := f.runner.Submit(ctx, nil, "sum", sumParams{Numbers: []int{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != jobs.StatusQueued || sub.CreatedBy != "token:5" {
		t.Fatalf("submitted = %+v", sub)
	}
	// Queued until the runner runs.
	if j, _ := f.runner.Get(ctx, sub.ID); j.Status != jobs.StatusQueued {
		t.Fatalf("before start = %s", j.Status)
	}
	if _, err := f.runner.Submit(ctx, nil, "nope", nil); err == nil {
		t.Fatal("unknown type accepted")
	}

	f.start(t)
	eventually(t, "progress 0.5 to be saved", func() bool { return progressOf(t, f.d, sub.ID) == 0.5 })
	close(release)
	j := wait(t, f.runner, sub.ID)
	if j.Status != jobs.StatusSucceeded || j.Progress != 1 || j.StartedAt == nil || j.FinishedAt == nil || j.Error != "" {
		t.Fatalf("finished = %+v", j)
	}
	var res map[string]any
	if err := json.Unmarshal(j.Result, &res); err != nil || res["total"] != float64(6) || res["actor"] != "token:5" || res["name"] != "agent" {
		t.Fatalf("result = %s (%v)", j.Result, err)
	}

	failed, _ := f.runner.Submit(ctx, nil, "fail", nil)
	j = wait(t, f.runner, failed.ID)
	if j.Status != jobs.StatusFailed || j.Error != "row 4 is invalid" || string(j.Result) != `{"done":3}` || j.Progress != 0.25 {
		t.Fatalf("failed job = %+v", j)
	}
	pan, _ := f.runner.Submit(ctx, nil, "panic", nil)
	j = wait(t, f.runner, pan.ID)
	if j.Status != jobs.StatusFailed || !strings.Contains(j.Error, "kaboom") {
		t.Fatalf("panicked job = %+v", j)
	}

	list, err := f.runner.List(ctx, jobs.ListFilter{Status: jobs.StatusFailed})
	if err != nil || len(list) != 2 || list[0].ID != pan.ID {
		t.Fatalf("list failed = %+v %v", list, err)
	}
	if _, err := f.runner.Wait(ctx, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("wait unknown = %v", err)
	}
}

// TestSubmitInTransaction: Submit joins the caller's transaction (it used to
// deadlock on the single write connection), and the job runs only after the
// commit.
func TestSubmitInTransaction(t *testing.T) {
	f := newFixture(t, nil)
	ran := make(chan int64, 4)
	f.runner.Register("x", func(ctx context.Context, job jobs.JobContext) (any, error) {
		ran <- job.ID()
		return nil, nil
	})
	f.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var sub jobs.Job
	err := f.d.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		if sub, err = f.runner.Submit(ctx, tx, "x", map[string]int{"n": 1}); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond) // the runner is awake but must not see the row yet
		select {
		case id := <-ran:
			t.Errorf("job %d ran before the commit", id)
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatalf("submit in tx: %v", err)
	}
	if j := wait(t, f.runner, sub.ID); j.Status != jobs.StatusSucceeded {
		t.Fatalf("job = %+v", j)
	}

	// A rolled-back transaction (here an api.Tx dry run) leaves no job.
	var dry jobs.Job
	if err := api.Tx(ctx, f.d, true, func(tx *sql.Tx) error {
		var err error
		dry, err = f.runner.Submit(ctx, tx, "x", nil)
		return err
	}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := f.runner.Get(ctx, dry.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("dry-run job exists: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if len(ran) != 1 {
		t.Fatalf("jobs run = %d, want 1", len(ran))
	}
}

// TestSetProgressInsideTransaction: a handler that reports progress while
// holding a write transaction must not block (it used to wait forever for
// the single write connection).
func TestSetProgressInsideTransaction(t *testing.T) {
	f := newFixture(t, nil)
	f.runner.Register("import", func(ctx context.Context, job jobs.JobContext) (any, error) {
		err := f.d.WithTx(ctx, func(tx *sql.Tx) error {
			for i := 1; i <= 10; i++ {
				job.SetProgress(float64(i) / 20)
				time.Sleep(5 * time.Millisecond)
			}
			return nil
		})
		return nil, err
	})
	f.runner.Register("stalled", func(ctx context.Context, job jobs.JobContext) (any, error) {
		job.SetProgress(0.4)
		return nil, errors.New("stopped")
	})
	f.start(t)
	j, _ := f.runner.Submit(context.Background(), nil, "import", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := f.runner.Wait(ctx, j.ID)
	if err != nil || got.Status != jobs.StatusSucceeded || got.Progress != 1 {
		t.Fatalf("job = %+v, %v", got, err)
	}
	s, _ := f.runner.Submit(context.Background(), nil, "stalled", nil)
	if got := wait(t, f.runner, s.ID); got.Progress != 0.4 {
		t.Fatalf("failed job keeps its last progress: %+v", got)
	}
}

func TestInterruptedRecovery(t *testing.T) {
	d := testutil.NewDB(t)
	now := db.FormatTime(testutil.Epoch)
	testutil.Exec(t, d, `INSERT INTO jobs (id, type, status, created_by, actor_name, created_at, started_at) VALUES
		(1, 'import', 'queued', 'user:7', 'owner@example.com', ?, NULL),
		(2, 'import', 'running', 'user:7', '', ?, ?),
		(3, 'import', 'succeeded', '', '', ?, ?),
		(4, 'snapshot', 'running', 'token:2', 'agent', ?, ?)`, now, now, now, now, now, now, now)

	f := newFixture(t, d)
	type run struct {
		id      int64
		resumed bool
		actor   core.Actor
	}
	ran := make(chan run, 4)
	handler := func(ctx context.Context, job jobs.JobContext) (any, error) {
		ran <- run{job.ID(), job.Resumed(), core.ActorFromContext(ctx)}
		return nil, nil
	}
	f.runner.Register("import", handler)
	f.runner.Register("snapshot", handler, jobs.Resumable())
	// A job submitted while the runner starts is queued and runs.
	fresh, err := f.runner.Submit(context.Background(), nil, "import", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.start(t)
	for _, id := range []int64{1, 4, fresh.ID} {
		if j := wait(t, f.runner, id); j.Status != jobs.StatusSucceeded {
			t.Fatalf("job %d = %+v", id, j)
		}
	}
	got := map[int64]run{}
	for range 3 {
		r := <-ran
		got[r.id] = r
	}
	if r := got[1]; r.resumed || r.actor != (core.Actor{Type: core.ActorUser, ID: "7", Name: "owner@example.com"}) {
		t.Errorf("queued job from the previous process = %+v", r)
	}
	if r := got[4]; !r.resumed || r.actor.String() != "token:2" {
		t.Errorf("resumable job = %+v", r)
	}
	if r := got[fresh.ID]; r.resumed || r.actor.Type != core.ActorSystem {
		t.Errorf("fresh job = %+v", r)
	}
	for id, want := range map[int64]string{2: jobs.StatusFailed, 3: jobs.StatusSucceeded} {
		j, err := f.runner.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != want || (want == jobs.StatusFailed && (j.Error != jobs.ErrInterrupted || j.FinishedAt == nil)) {
			t.Errorf("job %d = %+v", id, j)
		}
	}
}

func TestShutdownInterruptsRunningJob(t *testing.T) {
	f := newFixture(t, nil)
	started := make(chan struct{}, 2)
	slow := func(ctx context.Context, job jobs.JobContext) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.runner.Register("slow", slow)
	f.runner.Register("resumable", slow, jobs.Resumable())
	j, _ := f.runner.Submit(context.Background(), nil, "slow", nil)
	rj, _ := f.runner.Submit(context.Background(), nil, "resumable", nil)
	f.start(t)
	<-started
	<-started
	f.stop()
	got, err := f.runner.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusFailed || !strings.HasPrefix(got.Error, jobs.ErrInterrupted) {
		t.Fatalf("after shutdown = %+v", got)
	}
	if got, _ := f.runner.Get(context.Background(), rj.ID); got.Status != jobs.StatusQueued {
		t.Fatalf("resumable after shutdown = %+v", got)
	}
}

func TestRegisterAndRunGuards(t *testing.T) {
	f := newFixture(t, nil)
	f.runner.Register("a", func(context.Context, jobs.JobContext) (any, error) { return nil, nil })
	for name, fn := range map[string]func(){
		"duplicate": func() {
			f.runner.Register("a", func(context.Context, jobs.JobContext) (any, error) { return nil, nil })
		},
		"no type": func() { f.runner.Register("", func(context.Context, jobs.JobContext) (any, error) { return nil, nil }) },
		"nil fn":  func() { f.runner.Register("b", nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			fn()
		}()
	}
	f.start(t)
	j, _ := f.runner.Submit(context.Background(), nil, "a", nil)
	wait(t, f.runner, j.ID) // the runner is running now
	if err := f.runner.Run(context.Background()); err == nil {
		t.Fatal("second concurrent Run succeeded")
	}
	if f.runner.Name() != "jobs" {
		t.Fatal(f.runner.Name())
	}
}

type fakeAuth struct{}

func (fakeAuth) AuthenticateToken(context.Context, string) (api.Principal, error) {
	return api.Principal{Actor: core.Actor{Type: core.ActorToken, ID: "1", Name: "r"}, Scope: api.ScopeRead}, nil
}

func (fakeAuth) AuthenticateSession(context.Context, string) (api.Principal, string, error) {
	return api.Principal{}, "", api.ErrUnauthorized
}

func TestAPI(t *testing.T) {
	f := newFixture(t, nil)
	f.runner.Register("echo", func(ctx context.Context, job jobs.JobContext) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	})
	f.start(t)
	j1, _ := f.runner.Submit(context.Background(), nil, "echo", map[string]int{"n": 1})
	j2, _ := f.runner.Submit(context.Background(), nil, "echo", map[string]int{"n": 2})
	wait(t, f.runner, j1.ID)
	wait(t, f.runner, j2.ID)

	a, err := api.New(api.Config{DB: f.d, BaseURL: "https://x.example.com", Auth: fakeAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	f.runner.RegisterAPI(a)
	get := func(path string) (int, []byte) {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer mr_x")
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	code, body := get("/api/v1/jobs/" + strconv.FormatInt(j1.ID, 10))
	var j jobs.Job
	if err := json.Unmarshal(body, &j); code != 200 || err != nil || j.ID != j1.ID || j.Status != jobs.StatusSucceeded ||
		string(j.Params) != `{"n":1}` || string(j.Result) != `{"ok":"yes"}` {
		t.Fatalf("get job = %d %s", code, body)
	}
	if code, _ := get("/api/v1/jobs/999"); code != 404 {
		t.Fatalf("missing job = %d", code)
	}
	code, body = get("/api/v1/jobs?limit=1&status=succeeded")
	var list api.ListBody[jobs.Job]
	if err := json.Unmarshal(body, &list); code != 200 || err != nil || len(list.Items) != 1 || list.Items[0].ID != j2.ID || list.NextCursor == "" {
		t.Fatalf("list = %d %s", code, body)
	}
	if code, _ := get("/api/v1/jobs?status=bogus"); code != 422 {
		t.Fatalf("bad status = %d", code)
	}
}
