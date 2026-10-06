package api

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"mailroom/internal/core"
)

// Default per-actor API rate limit (G-9): 20 requests/s, bursts of 60.
const (
	DefaultRate  = 20.0
	DefaultBurst = 60
)

// sweepInterval is how often idle buckets are dropped from memory.
const sweepInterval = time.Minute

// DefaultMaxKeys bounds the number of buckets a Limiter keeps. Beyond it,
// requests from new keys share one overflow bucket, so a flood of distinct
// keys (spoofed or rotating addresses) can't grow memory without bound.
const DefaultMaxKeys = 100_000

// overflowKey is the shared bucket of keys beyond the key limit.
const overflowKey = "limiter:overflow"

// forcedSweepInterval rate-limits the sweeps run when the key limit is hit.
const forcedSweepInterval = time.Second

// Limiter is an in-memory token-bucket rate limiter keyed by an arbitrary
// string (an actor, an IP address, "form:<id>|<ip>", ...). Each key gets
// its own bucket of burst tokens refilled at rate tokens per second. It is
// safe for concurrent use. Idle buckets are forgotten, and at most MaxKeys
// buckets are kept (new keys beyond that share one bucket), so memory stays
// bounded.
type Limiter struct {
	rate    float64
	burst   float64
	clock   core.Clock
	maxKeys int

	mu          sync.Mutex
	buckets     map[string]*bucket
	lastSweep   time.Time
	forcedSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Decision is the outcome of one Limiter.Allow call.
type Decision struct {
	Allowed    bool
	Limit      int           // bucket size (burst)
	Remaining  int           // whole tokens left after this request
	Reset      time.Duration // until the bucket is full again
	RetryAfter time.Duration // until the next request is allowed (0 when Allowed)
}

// NewLimiter returns a limiter refilling rate tokens per second with
// buckets of burst tokens. Non-positive values use DefaultRate and
// DefaultBurst; a nil clock uses core.SystemClock.
func NewLimiter(rate float64, burst int, clock core.Clock) *Limiter {
	if rate <= 0 {
		rate = DefaultRate
	}
	if burst <= 0 {
		burst = DefaultBurst
	}
	if clock == nil {
		clock = core.SystemClock{}
	}
	return &Limiter{rate: rate, burst: float64(burst), clock: clock, maxKeys: DefaultMaxKeys, buckets: map[string]*bucket{}}
}

// SetMaxKeys changes the bucket limit (default DefaultMaxKeys; values below
// 1 are ignored).
func (l *Limiter) SetMaxKeys(n int) {
	if n < 1 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxKeys = n
}

// Len returns the number of buckets held.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Allow takes one token from key's bucket if one is available.
func (l *Limiter) Allow(key string) Decision {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) >= sweepInterval {
		l.sweep(now)
	}
	b := l.buckets[key]
	if b == nil && len(l.buckets) >= l.maxKeys {
		if now.Sub(l.forcedSweep) >= forcedSweepInterval {
			l.sweep(now)
			l.forcedSweep = now
		}
		if len(l.buckets) >= l.maxKeys {
			key = overflowKey
			b = l.buckets[key]
		}
	}
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	l.refill(b, now)
	d := Decision{Limit: int(l.burst)}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
	} else {
		d.RetryAfter = l.duration(1 - b.tokens)
	}
	d.Remaining = int(math.Floor(b.tokens))
	d.Reset = l.duration(l.burst - b.tokens)
	return d
}

func (l *Limiter) refill(b *bucket, now time.Time) {
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(l.burst, b.tokens+el*l.rate)
	}
	b.last = now
}

// duration returns how long refilling n tokens takes.
func (l *Limiter) duration(n float64) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n / l.rate * float64(time.Second))
}

// sweep forgets buckets that have refilled completely.
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		l.refill(b, now)
		if b.tokens >= l.burst {
			delete(l.buckets, k)
		}
	}
	l.lastSweep = now
}

// WriteHeaders sets the RateLimit-Limit, RateLimit-Remaining and
// RateLimit-Reset headers (seconds, rounded up), plus Retry-After when the
// request was refused.
func (d Decision) WriteHeaders(h http.Header) {
	h.Set("RateLimit-Limit", strconv.Itoa(d.Limit))
	h.Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
	h.Set("RateLimit-Reset", strconv.Itoa(ceilSeconds(d.Reset)))
	if !d.Allowed {
		h.Set("Retry-After", strconv.Itoa(max(1, ceilSeconds(d.RetryAfter))))
	}
}

func ceilSeconds(d time.Duration) int {
	return int(math.Ceil(d.Seconds()))
}

// ErrRateLimited returns the 429 error response.
func ErrRateLimited() *Error {
	return NewError(http.StatusTooManyRequests, CodeRateLimited, "too many requests; slow down and retry after the Retry-After delay", "")
}

// Middleware rate-limits next by key(r). Refused requests get 429 in the
// API error format; every response carries the RateLimit-* headers. Public
// endpoints (forms, tracking) use it with a per-IP key, e.g.
//
//	lim := api.NewLimiter(1, 10, clock)
//	h := lim.Middleware(func(r *http.Request) string { return api.IPKey(api.ClientIP(r, trusted)) }, h)
func (l *Limiter) Middleware(key func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := l.Allow(key(r))
		d.WriteHeaders(w.Header())
		if !d.Allowed {
			ErrRateLimited().Write(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP returns the client's IP address. X-Forwarded-For is used only
// when the connection comes from a trusted reverse proxy (TRUSTED_PROXIES):
// the header is walked from the right, skipping trusted hops, and the first
// address that isn't a trusted proxy is the client. Whatever a client puts
// further left is ignored, so it can't choose its own address. With no
// trusted proxies (the default) it is the connection's remote address.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	remote := remoteAddr(r)
	if !remote.IsValid() {
		return r.RemoteAddr
	}
	if len(trusted) == 0 || !inPrefixes(remote, trusted) {
		return remote.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := remote
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // garbage: believe nothing further left
		}
		client = a.Unmap()
		if !inPrefixes(client, trusted) {
			break
		}
	}
	return client.String()
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// IPKey returns the rate-limit key of a client IP address: the address
// itself for IPv4, and its /64 network for IPv6, because one IPv6 client (a
// home connection, a VPS) typically controls a whole /64. Unparsable input
// is returned unchanged.
func IPKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, err := a.Prefix(64)
	if err != nil {
		return a.String()
	}
	return p.String()
}
