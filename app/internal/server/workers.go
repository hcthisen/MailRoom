package server

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"mailroom/internal/core"
)

// Worker is a background loop (ARCHITECTURE.md §8). Run blocks until ctx is
// cancelled. Returning nil before that means the worker is done (e.g. a
// run-once startup task) and it is not restarted; returning an error or
// panicking restarts it after a backoff.
type Worker interface {
	Name() string
	Run(ctx context.Context) error
}

// Supervisor runs workers with restart-on-error backoff and panic recovery.
type Supervisor struct {
	Logger *slog.Logger // default slog.Default()
	Clock  core.Clock   // default core.SystemClock{}; measures run time
	// MinBackoff and MaxBackoff bound the restart delay, which doubles after
	// each quick failure (defaults 1s and 1m).
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// StableAfter resets the backoff when a run lasted at least this long
	// (default 1m).
	StableAfter time.Duration

	mu     sync.Mutex
	active map[string]int // worker name -> running Run calls
}

// RunWorkers runs workers with a default Supervisor until ctx is cancelled
// and every worker has returned.
func RunWorkers(ctx context.Context, workers ...Worker) {
	(&Supervisor{}).Run(ctx, workers...)
}

// RunUntil runs workers like Run, but once ctx is cancelled it waits at
// most drain for them to return. It returns the names of the workers still
// running when it gave up (nil when all returned in time); they are left
// to be stopped by the process exit. Graceful shutdown uses it so a worker
// that ignores its context can't keep the process from exiting.
func (s *Supervisor) RunUntil(ctx context.Context, drain time.Duration, workers ...Worker) []string {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, workers...)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	t := time.NewTimer(drain)
	defer t.Stop()
	select {
	case <-done:
		return nil
	case <-t.C:
		return s.Running()
	}
}

// Running returns the sorted names of the workers whose Run is executing.
func (s *Supervisor) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for n, c := range s.active {
		if c > 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func (s *Supervisor) track(name string, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		s.active = map[string]int{}
	}
	s.active[name] += delta
}

// Run starts every worker in its own goroutine and blocks until ctx is
// cancelled and all of them have returned.
func (s *Supervisor) Run(ctx context.Context, workers ...Worker) {
	s.defaults()
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.supervise(ctx, w)
		}()
	}
	wg.Wait()
}

func (s *Supervisor) defaults() {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.Clock == nil {
		s.Clock = core.SystemClock{}
	}
	if s.MinBackoff <= 0 {
		s.MinBackoff = time.Second
	}
	if s.MaxBackoff < s.MinBackoff {
		s.MaxBackoff = max(time.Minute, s.MinBackoff)
	}
	if s.StableAfter <= 0 {
		s.StableAfter = time.Minute
	}
}

func (s *Supervisor) supervise(ctx context.Context, w Worker) {
	log := s.Logger.With("worker", w.Name())
	backoff := s.MinBackoff
	for {
		log.Info("worker starting")
		start := s.Clock.Now()
		s.track(w.Name(), 1)
		err := runSafely(ctx, w)
		s.track(w.Name(), -1)
		if ctx.Err() != nil {
			log.Info("worker stopped")
			return
		}
		if err == nil {
			log.Info("worker finished")
			return
		}
		if s.Clock.Now().Sub(start) >= s.StableAfter {
			backoff = s.MinBackoff
		}
		log.Error("worker failed; restarting", "err", err, "backoff", backoff)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			log.Info("worker stopped")
			return
		case <-t.C:
		}
		backoff = min(backoff*2, s.MaxBackoff)
	}
}

// runSafely runs w, converting a panic into an error.
func runSafely(ctx context.Context, w Worker) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return w.Run(ctx)
}
