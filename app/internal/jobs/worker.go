package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/server"
)

var _ server.Worker = (*Runner)(nil)

// progressWriteTimeout bounds one progress write; a busy write pool (e.g.
// the job's own transaction) makes the write be skipped and retried later.
const progressWriteTimeout = 2 * time.Second

// Name implements server.Worker.
func (r *Runner) Name() string { return "jobs" }

// Run implements server.Worker. It first recovers jobs that a previous
// process left running (failed "interrupted", or queued again when their
// type is Resumable), then claims and executes queued jobs, at most Workers
// at a time, until ctx is cancelled. Running jobs then see their context
// cancelled; Run returns once they have returned.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("jobs: runner is already running")
	}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	// Only Run sets jobs to running, and it does so after this, so every
	// running row here belongs to a previous process. Jobs submitted
	// meanwhile are queued and are not touched.
	if err := r.recoverInterrupted(ctx); err != nil {
		return err
	}
	sem := make(chan struct{}, r.workers)
	var wg sync.WaitGroup
	defer wg.Wait()
	woken := true // look for work left by a previous process at once
	for {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		c, ok, err := r.claim(ctx, woken)
		if err != nil || !ok {
			<-sem
			if err != nil && ctx.Err() == nil {
				r.log.Error("jobs: claim", "err", err)
			}
			if woken, ok = r.idle(ctx); !ok {
				return nil
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.execute(ctx, c)
		}()
	}
}

// idle waits for Submit, the poll interval or ctx. woken reports a Submit;
// ok is false when ctx is done.
func (r *Runner) idle(ctx context.Context) (woken, ok bool) {
	t := time.NewTimer(r.pollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false, false
	case <-r.notify:
		return true, true
	case <-t.C:
		return false, true
	}
}

// claimed is a job the runner has moved to running.
type claimed struct {
	id      int64
	typ     string
	params  json.RawMessage
	actor   core.Actor
	resumed bool
}

// claim moves the oldest queued job to running and returns it. A poll that
// Submit didn't trigger first looks on the read pool, so an idle runner
// doesn't take the write lock every PollInterval. After a Submit it goes
// straight to the write pool: that waits for the submitting transaction
// and then sees its job.
func (r *Runner) claim(ctx context.Context, woken bool) (c claimed, ok bool, err error) {
	if !woken {
		var one int
		switch err := r.db.Read.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE status = 'queued' LIMIT 1`).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return c, false, nil
		case err != nil:
			return c, false, err
		}
	}
	err = r.db.WithTx(ctx, func(tx *sql.Tx) error {
		var params, createdBy, name string
		var started sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT id, type, params, created_by, actor_name, started_at FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1`).
			Scan(&c.id, &c.typ, &params, &createdBy, &name, &started)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET status = 'running', started_at = coalesce(started_at, ?) WHERE id = ?`,
			db.FormatTime(r.clock.Now()), c.id); err != nil {
			return err
		}
		c.params, c.actor, c.resumed, ok = json.RawMessage(params), parseActor(createdBy, name), started.Valid, true
		return nil
	})
	return c, ok && err == nil, err
}

// recoverInterrupted handles jobs left running by a previous process:
// Resumable types are queued again, everything else fails "interrupted".
func (r *Runner) recoverInterrupted(ctx context.Context) error {
	now := db.FormatTime(r.clock.Now())
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, type FROM jobs WHERE status = 'running'`)
		if err != nil {
			return fmt.Errorf("jobs: find interrupted jobs: %w", err)
		}
		type row struct {
			id  int64
			typ string
		}
		var found []row
		for rows.Next() {
			var x row
			if err := rows.Scan(&x.id, &x.typ); err != nil {
				rows.Close()
				return err
			}
			found = append(found, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, x := range found {
			if h, ok := r.handler(x.typ); ok && h.resumable {
				if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued', progress = 0 WHERE id = ?`, x.id); err != nil {
					return fmt.Errorf("jobs: requeue job %d: %w", x.id, err)
				}
				r.log.Warn("job interrupted by restart; queued again", "job", x.id, "type", x.typ)
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET status = 'failed', error = ?, finished_at = ? WHERE id = ?`, ErrInterrupted, now, x.id); err != nil {
				return fmt.Errorf("jobs: mark job %d interrupted: %w", x.id, err)
			}
			r.log.Warn("job interrupted by restart", "job", x.id, "type", x.typ)
		}
		return nil
	})
}

// execute runs one claimed job and records its outcome.
func (r *Runner) execute(ctx context.Context, c claimed) {
	defer r.jobFinished()
	store := context.WithoutCancel(ctx)
	h, ok := r.handler(c.typ)
	jc := &jobContext{id: c.id, typ: c.typ, params: c.params, resumed: c.resumed}
	stop := make(chan struct{})
	var flusher sync.WaitGroup
	flusher.Add(1)
	go func() {
		defer flusher.Done()
		r.flushProgress(ctx, jc, stop)
	}()

	var result any
	var runErr error
	if !ok {
		runErr = fmt.Errorf("no handler for job type %q", c.typ)
	} else {
		result, runErr = r.runSafely(core.WithActor(ctx, c.actor), h.fn, jc)
	}
	close(stop)
	flusher.Wait()

	if runErr != nil && ctx.Err() != nil {
		if h.resumable {
			if _, err := r.db.Write.ExecContext(store,
				`UPDATE jobs SET status = 'queued', progress = 0 WHERE id = ?`, c.id); err != nil {
				r.log.Error("job requeue", "job", c.id, "err", err)
			}
			r.log.Info("job interrupted by shutdown; it runs again at the next start", "job", c.id)
			return
		}
		runErr = fmt.Errorf("%s: server shutting down (%v)", ErrInterrupted, runErr)
	}
	r.complete(store, c.id, result, runErr, jc.progress())
}

// runSafely runs h, turning a panic into an error (the stack is logged,
// not stored in the job).
func (r *Runner) runSafely(ctx context.Context, h Handler, jc JobContext) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			r.log.Error("job panicked", "job", jc.ID(), "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	return h(ctx, jc)
}

// flushProgress writes jc's progress every progressInterval until stop is
// closed or ctx is done.
func (r *Runner) flushProgress(ctx context.Context, jc *jobContext, stop <-chan struct{}) {
	t := time.NewTicker(r.progressInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			r.saveProgress(ctx, jc)
		}
	}
}

// saveProgress writes the latest progress if it changed. When the write
// pool is busy past progressWriteTimeout the write is skipped and retried
// at the next tick.
func (r *Runner) saveProgress(ctx context.Context, jc *jobContext) {
	if !jc.dirty.Swap(false) {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, progressWriteTimeout)
	defer cancel()
	if _, err := r.db.Write.ExecContext(wctx,
		`UPDATE jobs SET progress = ? WHERE id = ? AND status = 'running'`, jc.progress(), jc.id); err != nil {
		jc.dirty.Store(true)
		if ctx.Err() == nil {
			r.log.Debug("job progress not saved; retrying", "job", jc.id, "err", err)
		}
	}
}

// complete stores the final status, result and error.
func (r *Runner) complete(ctx context.Context, id int64, result any, runErr error, progress float64) {
	resJSON, err := db.MarshalJSON(result)
	if err != nil {
		resJSON = "{}"
		if runErr == nil {
			runErr = fmt.Errorf("encode result: %w", err)
		}
	}
	status, msg, prog := StatusSucceeded, "", 1.0
	if runErr != nil {
		status, msg, prog = StatusFailed, runErr.Error(), progress
		r.log.Warn("job failed", "job", id, "err", runErr)
	}
	if _, err := r.db.Write.ExecContext(ctx,
		`UPDATE jobs SET status = ?, result = ?, error = ?, progress = max(progress, ?), finished_at = ? WHERE id = ?`,
		status, resJSON, msg, prog, db.FormatTime(r.clock.Now()), id); err != nil {
		r.log.Error("job complete", "job", id, "err", err)
	}
}

// jobContext implements JobContext.
type jobContext struct {
	id      int64
	typ     string
	params  json.RawMessage
	resumed bool

	prog  atomic.Uint64 // math.Float64bits of the latest progress
	dirty atomic.Bool   // prog changed since the last write
}

func (c *jobContext) ID() int64     { return c.id }
func (c *jobContext) Type() string  { return c.typ }
func (c *jobContext) Resumed() bool { return c.resumed }

func (c *jobContext) Params(v any) error {
	if err := json.Unmarshal(c.params, v); err != nil {
		return fmt.Errorf("job %d params: %w", c.id, err)
	}
	return nil
}

func (c *jobContext) SetProgress(p float64) {
	if math.IsNaN(p) {
		return
	}
	c.prog.Store(math.Float64bits(min(max(p, 0), 1)))
	c.dirty.Store(true)
}

func (c *jobContext) progress() float64 { return math.Float64frombits(c.prog.Load()) }
