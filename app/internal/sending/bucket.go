package sending

import (
	"context"
	"math"
	"sync"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/settings"
)

// bucket is the one global token bucket shared by every sender worker
// (B-4), so the real send rate never exceeds rate, however many workers
// run. Time comes from the core.Clock; sleep is replaceable so tests can
// advance a fake clock instead of waiting.
type bucket struct {
	mu     sync.Mutex
	clock  core.Clock
	rate   float64 // tokens per second, > 0
	burst  float64 // capacity: one second of tokens, at least 1
	tokens float64
	last   time.Time
	sleep  func(ctx context.Context, d time.Duration) error
}

// minRate keeps a misconfigured rate from stalling the bucket forever.
const minRate = 0.01

func newBucket(clock core.Clock, rate float64) *bucket {
	b := &bucket{clock: clock, last: clock.Now(), sleep: sleepCtx}
	b.setRateLocked(rate)
	b.tokens = b.burst
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Rate returns the current rate in messages per second.
func (b *bucket) Rate() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rate
}

// SetRate changes the rate; tokens earned so far are kept (up to the new
// capacity).
func (b *bucket) SetRate(rate float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(b.clock.Now())
	b.setRateLocked(rate)
}

func (b *bucket) setRateLocked(rate float64) {
	if math.IsNaN(rate) || rate < minRate {
		rate = minRate
	}
	b.rate = rate
	b.burst = math.Max(1, math.Floor(rate))
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}

func (b *bucket) refillLocked(now time.Time) {
	if now.After(b.last) {
		b.tokens = math.Min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now // a clock that moved backwards restarts from here
}

// reserve takes a token and returns 0, or returns how long to wait before
// one is available (without taking it).
func (b *bucket) reserve() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(b.clock.Now())
	// The epsilon absorbs float rounding after a sleep of exactly the
	// computed duration.
	if b.tokens >= 1-1e-9 {
		b.tokens = math.Max(0, b.tokens-1)
		return 0
	}
	return time.Duration(math.Ceil((1 - b.tokens) / b.rate * float64(time.Second)))
}

// Wait blocks until a token is available and takes it.
func (b *bucket) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		d := b.reserve()
		if d == 0 {
			return nil
		}
		if err := b.sleep(ctx, d); err != nil {
			return err
		}
	}
}

// Refund returns a token taken for a message that turned out not to need
// sending (it was claimed by someone else or is no longer due).
func (b *bucket) Refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = math.Min(b.burst, b.tokens+1)
}

// effectiveRate is the B-4 rate: the override when set, else
// rate_fraction x SES MaxSendRate when the quota is known, else DefaultRate.
func effectiveRate(sending settings.Sending, acct *Account) float64 {
	if sending.RateOverridePerSec > 0 {
		return sending.RateOverridePerSec
	}
	if acct != nil && acct.MaxSendRate > 0 {
		f := sending.RateFraction
		if f <= 0 || f > 1 {
			f = 0.8
		}
		return f * acct.MaxSendRate
	}
	return DefaultRate
}
