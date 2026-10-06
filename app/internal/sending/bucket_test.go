package sending

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

// fakeBucket returns a bucket whose sleep advances the fake clock.
func fakeBucket(clock *testutil.Clock, rate float64) *bucket {
	b := newBucket(clock, rate)
	b.sleep = func(ctx context.Context, d time.Duration) error {
		clock.Advance(d)
		return ctx.Err()
	}
	return b
}

func TestBucketRate(t *testing.T) {
	tests := []struct {
		rate    float64
		n       int
		wantMin time.Duration // (n - burst) / rate
	}{
		{rate: 10, n: 100, wantMin: 9 * time.Second},
		{rate: 1, n: 5, wantMin: 4 * time.Second},
		{rate: 0.5, n: 3, wantMin: 4 * time.Second},
		{rate: 11.2, n: 112, wantMin: 9017857143}, // (112-11)/11.2 s
	}
	for _, tt := range tests {
		clock := testutil.NewClock(time.Time{})
		b := fakeBucket(clock, tt.rate)
		start := clock.Now()
		for range tt.n {
			if err := b.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		got := clock.Now().Sub(start)
		if got < tt.wantMin-time.Millisecond || got > tt.wantMin+50*time.Millisecond {
			t.Errorf("rate %v: %d tokens took %v, want about %v", tt.rate, tt.n, got, tt.wantMin)
		}
	}
}

// TestBucketShared: many goroutines share one bucket, and the number of
// tokens handed out never exceeds burst + elapsed x rate.
func TestBucketShared(t *testing.T) {
	clock := testutil.NewClock(time.Time{})
	b := fakeBucket(clock, 20)
	start := clock.Now()
	var taken atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if err := b.Wait(context.Background()); err != nil {
					t.Error(err)
					return
				}
				n := taken.Add(1)
				allowed := 20 + clock.Now().Sub(start).Seconds()*20
				if float64(n) > allowed+1e-9 {
					t.Errorf("%d tokens after %v exceeds the rate", n, clock.Now().Sub(start))
				}
			}
		}()
	}
	wg.Wait()
	if taken.Load() != 200 {
		t.Fatalf("taken = %d", taken.Load())
	}
}

func TestBucketSetRateRefundAndClock(t *testing.T) {
	clock := testutil.NewClock(time.Time{})
	b := fakeBucket(clock, 2)
	if b.reserve() != 0 || b.reserve() != 0 {
		t.Fatal("a fresh bucket starts full (burst 2)")
	}
	if d := b.reserve(); d != 500*time.Millisecond {
		t.Fatalf("wait = %v", d)
	}
	b.Refund()
	if b.reserve() != 0 {
		t.Fatal("refunded token not available")
	}
	b.SetRate(100)
	if b.Rate() != 100 || b.burst != 100 {
		t.Fatalf("rate %v burst %v", b.Rate(), b.burst)
	}
	clock.Advance(time.Second)
	for range 100 {
		if b.reserve() != 0 {
			t.Fatal("one second at 100/s must give 100 tokens")
		}
	}
	if b.reserve() == 0 {
		t.Fatal("burst exceeded")
	}
	// A clock that moves backwards neither panics nor mints tokens.
	clock.Advance(-time.Hour)
	if b.reserve() == 0 {
		t.Fatal("tokens from a backwards clock")
	}
	// A nonsensical rate is clamped instead of stalling forever.
	b.SetRate(0)
	if b.Rate() != minRate {
		t.Fatalf("rate = %v", b.Rate())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Wait(ctx); err == nil {
		t.Fatal("Wait must honour a cancelled context")
	}
}

func TestEffectiveRate(t *testing.T) {
	tests := []struct {
		name    string
		sending settings.Sending
		acct    *Account
		want    float64
	}{
		{"default until the quota is known", settings.Sending{RateFraction: 0.8}, nil, DefaultRate},
		{"unknown quota", settings.Sending{RateFraction: 0.8}, &Account{MaxSendRate: 0}, DefaultRate},
		{"fraction of max rate", settings.Sending{RateFraction: 0.8}, &Account{MaxSendRate: 14}, 11.2},
		{"override wins", settings.Sending{RateOverridePerSec: 3, RateFraction: 0.8}, &Account{MaxSendRate: 14}, 3},
		{"bad fraction falls back to 0.8", settings.Sending{RateFraction: 0}, &Account{MaxSendRate: 10}, 8},
	}
	for _, tt := range tests {
		if got := effectiveRate(tt.sending, tt.acct); got < tt.want-1e-9 || got > tt.want+1e-9 {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}
