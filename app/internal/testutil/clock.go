package testutil

import (
	"sync"
	"time"

	"mailroom/internal/core"
)

// Clock is a manually controlled core.Clock, safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

var _ core.Clock = (*Clock)(nil)

// NewClock returns a clock set to start (Epoch when start is zero).
func NewClock(start time.Time) *Clock {
	if start.IsZero() {
		start = Epoch
	}
	return &Clock{now: start.UTC()}
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}

// Advance moves the clock forward by d and returns the new time.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}
