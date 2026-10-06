// Package jobs runs long work in the background (PRD G-7): imports,
// broadcast snapshots, backups, bulk tagging. An operation submits a job
// and answers 202 {"job": {...}}; clients poll GET /api/v1/jobs/{id} for
// status, progress and result.
//
// Jobs are rows in the jobs table. Submit inserts a queued row through the
// caller's db.Querier, so it joins the operation's transaction (together
// with its audit.Log): the job exists, and runs, only if that transaction
// commits. The Runner claims queued rows in id order and executes them in a
// bounded pool of goroutines.
//
// Restarts: queued jobs survive a restart and run once the next process
// starts. Jobs that were running when the previous process stopped are
// marked failed with the error "interrupted", except for job types
// registered with Resumable, which are queued again and re-run from the
// start (their handler must be idempotent; JobContext.Resumed tells it).
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Job statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// ErrInterrupted is the error of jobs that were cut short by a restart or
// shutdown.
const ErrInterrupted = "interrupted"

// Job is a background job as stored and returned by the API.
type Job struct {
	ID         int64           `json:"id" example:"17"`
	Type       string          `json:"type" example:"import.contacts"`
	Status     string          `json:"status" enum:"queued,running,succeeded,failed" example:"running"`
	Progress   float64         `json:"progress" minimum:"0" maximum:"1" doc:"Fraction done, 0 to 1" example:"0.42"`
	Params     json.RawMessage `json:"params" doc:"The job's input"`
	Result     json.RawMessage `json:"result" doc:"The job's output once finished (may be partial on failure)"`
	Error      string          `json:"error" doc:"Why the job failed; empty unless status is failed" example:""`
	CreatedBy  string          `json:"created_by" doc:"Actor that submitted the job, e.g. token:3" example:"token:3"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// Done reports whether the job has finished.
func (j Job) Done() bool { return j.Status == StatusSucceeded || j.Status == StatusFailed }

// AcceptedBody is the body of a 202 response for work handed to a job:
// {"job": {...}}. Operations declare it as their output body with
// DefaultStatus 202.
type AcceptedBody struct {
	Job Job `json:"job"`
}

// JobContext is what a running job sees of itself.
type JobContext interface {
	// ID is the job's ID.
	ID() int64
	// Type is the job's type.
	Type() string
	// Params decodes the job's parameters into v.
	Params(v any) error
	// SetProgress records progress between 0 and 1. It only stores the
	// value in memory and never blocks: the runner writes the latest value
	// every ProgressInterval from its own goroutine, with a short timeout,
	// and skips a write while the database is busy. So it may be called for
	// every item, also while the handler holds a write transaction.
	SetProgress(p float64)
	// Resumed reports whether this run restarts a Resumable job that a
	// restart or shutdown interrupted, so the handler can skip work that is
	// already done.
	Resumed() bool
}

// Handler runs one job. The result (JSON-encodable, may be nil) is stored
// even when err is non-nil, so a failed job can report partial results.
// ctx carries the submitting actor (core.ActorFromContext) and is cancelled
// on shutdown.
type Handler func(ctx context.Context, job JobContext) (result any, err error)

// Deps are the Runner's dependencies.
type Deps struct {
	DB     *db.DB
	Clock  core.Clock   // default core.SystemClock
	Logger *slog.Logger // default slog.Default()
	// Workers is the number of jobs run at once (default 2).
	Workers int
	// PollInterval is how often the runner looks for queued jobs when Submit
	// didn't wake it, e.g. for jobs committed by another process (default 1s).
	PollInterval time.Duration
	// ProgressInterval is how often a running job's progress is written to
	// the database (default 1s).
	ProgressInterval time.Duration
}

// Runner persists and executes jobs. Register handlers during wiring, run
// the Runner as a server.Worker, and Submit from operations.
type Runner struct {
	db               *db.DB
	clock            core.Clock
	log              *slog.Logger
	workers          int
	pollInterval     time.Duration
	progressInterval time.Duration

	mu       sync.Mutex
	handlers map[string]handlerSpec
	running  bool
	changed  chan struct{} // closed and replaced whenever a job finishes
	notify   chan struct{}
}

type handlerSpec struct {
	fn        Handler
	resumable bool
}

// Option configures a job type at Register.
type Option func(*handlerSpec)

// Resumable marks a job type whose jobs are re-run from the start when a
// restart or shutdown interrupted them, instead of failing with
// "interrupted". Use it for idempotent work that must finish, such as a
// broadcast snapshot.
func Resumable() Option {
	return func(h *handlerSpec) { h.resumable = true }
}

// New returns a Runner.
func New(d Deps) *Runner {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Workers <= 0 {
		d.Workers = 2
	}
	if d.PollInterval <= 0 {
		d.PollInterval = time.Second
	}
	if d.ProgressInterval <= 0 {
		d.ProgressInterval = time.Second
	}
	return &Runner{
		db: d.DB, clock: d.Clock, log: d.Logger, workers: d.Workers,
		pollInterval: d.PollInterval, progressInterval: d.ProgressInterval,
		handlers: map[string]handlerSpec{},
		changed:  make(chan struct{}),
		notify:   make(chan struct{}, 1),
	}
}

// Register sets the handler for jobs of type typ. It panics on an empty
// type, a nil handler or a duplicate registration (wiring errors).
func (r *Runner) Register(typ string, fn Handler, opts ...Option) {
	if typ == "" || fn == nil {
		panic("jobs: Register needs a type and a handler")
	}
	spec := handlerSpec{fn: fn}
	for _, o := range opts {
		o(&spec)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.handlers[typ]; dup {
		panic("jobs: handler registered twice for " + typ)
	}
	r.handlers[typ] = spec
}

func (r *Runner) handler(typ string) (handlerSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.handlers[typ]
	return h, ok
}

// Submit inserts a queued job of type typ with params (JSON-encodable,
// nil = {}) on behalf of the actor in ctx, through q.
//
// Pass the operation's transaction as q, so the job commits, or rolls back
// (an error, a dry run), together with the change and its audit record.
// The job becomes visible to the runner only once that transaction commits,
// and runs once the Runner is running. A nil q inserts on the write pool in
// a statement of its own; never pass nil while holding a transaction on the
// same database, because the write pool has a single connection.
func (r *Runner) Submit(ctx context.Context, q db.Querier, typ string, params any) (Job, error) {
	if _, ok := r.handler(typ); !ok {
		return Job{}, fmt.Errorf("jobs: no handler for job type %q", typ)
	}
	if q == nil {
		q = r.db.Write
	}
	p, err := db.MarshalJSON(params)
	if err != nil {
		return Job{}, fmt.Errorf("jobs: encode params: %w", err)
	}
	actor := core.ActorFromContext(ctx)
	now := r.clock.Now()
	var id int64
	if err := q.QueryRowContext(ctx,
		`INSERT INTO jobs (type, status, params, created_by, actor_name, created_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		typ, StatusQueued, p, actor.String(), actor.Name, db.FormatTime(now)).Scan(&id); err != nil {
		return Job{}, fmt.Errorf("jobs: submit %s: %w", typ, err)
	}
	// Waking before the commit is harmless: the runner's claim waits for the
	// write connection and then sees the committed row, or nothing.
	r.wake()
	return Job{
		ID: id, Type: typ, Status: StatusQueued, Params: json.RawMessage(p), Result: json.RawMessage("{}"),
		CreatedBy: actor.String(), CreatedAt: now,
	}, nil
}

func (r *Runner) wake() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// parseActor restores the actor stored by Submit (created_by, actor_name).
func parseActor(createdBy, name string) core.Actor {
	typ, id, _ := strings.Cut(createdBy, ":")
	if typ == "" {
		typ = core.ActorSystem
	}
	return core.Actor{Type: typ, ID: id, Name: name}
}

const jobColumns = `id, type, status, progress, params, result, error, created_by, created_at, started_at, finished_at`

type scanner interface{ Scan(dest ...any) error }

func scanJob(s scanner) (Job, error) {
	var j Job
	var params, result, created string
	var started, finished sql.NullString
	if err := s.Scan(&j.ID, &j.Type, &j.Status, &j.Progress, &params, &result, &j.Error, &j.CreatedBy,
		&created, &started, &finished); err != nil {
		return Job{}, err
	}
	j.Params, j.Result = json.RawMessage(params), json.RawMessage(result)
	var err error
	if j.CreatedAt, err = db.ParseTime(created); err != nil {
		return Job{}, err
	}
	if j.StartedAt, err = db.ParseNullTime(started); err != nil {
		return Job{}, err
	}
	if j.FinishedAt, err = db.ParseNullTime(finished); err != nil {
		return Job{}, err
	}
	return j, nil
}

// Get returns the job with id; unknown IDs wrap core.ErrNotFound.
func (r *Runner) Get(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(r.db.Read.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("job %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job %d: %w", id, err)
	}
	return j, nil
}

// ListFilter narrows List. Zero fields don't filter.
type ListFilter struct {
	Type     string
	Status   string
	BeforeID int64 // keyset pagination, newest first
	Limit    int   // default 50
}

// List returns jobs, newest first.
func (r *Runner) List(ctx context.Context, f ListFilter) ([]Job, error) {
	q := `SELECT ` + jobColumns + ` FROM jobs WHERE 1=1`
	var args []any
	if f.Type != "" {
		q += ` AND type = ?`
		args = append(args, f.Type)
	}
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.BeforeID > 0 {
		q += ` AND id < ?`
		args = append(args, f.BeforeID)
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := r.db.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Wait blocks until the job has finished (or ctx is done) and returns it.
// Unknown IDs wrap core.ErrNotFound. It works for jobs of any process: it
// wakes when this process finishes a job, and re-reads the job every
// PollInterval otherwise.
func (r *Runner) Wait(ctx context.Context, id int64) (Job, error) {
	for {
		r.mu.Lock()
		ch := r.changed
		r.mu.Unlock()
		j, err := r.Get(ctx, id)
		if err != nil || j.Done() {
			return j, err
		}
		t := time.NewTimer(r.pollInterval)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return Job{}, ctx.Err()
		}
		t.Stop()
	}
}

// jobFinished wakes Wait callers.
func (r *Runner) jobFinished() {
	r.mu.Lock()
	defer r.mu.Unlock()
	close(r.changed)
	r.changed = make(chan struct{})
}
