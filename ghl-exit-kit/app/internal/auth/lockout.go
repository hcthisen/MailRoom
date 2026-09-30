package auth

import (
	"slices"
	"sync"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

// Login rate limits (A-4). Every attempt is counted before the password is
// checked, and a successful one is taken back, so concurrent attempts can't
// slip past a budget. Within LoginWindow at most:
//
//   - MaxLoginFailures failures per email and client address,
//   - MaxLoginFailuresPerIP failures per client address across all emails,
//   - MaxLoginFailuresPerAccount failures per email across all addresses.
//
// Within TOTPWindow at most MaxTOTPFailuresPerAccount wrong authenticator
// codes are accepted per email (after a correct password). A client
// address is an IPv4 address or an IPv6 /64 (api.IPKey).
//
// The two per-account budgets don't apply to addresses that logged in to
// the account successfully within KnownAddressTTL, so an attacker spread
// over many addresses can't lock the owner out from their usual address.
// Further attempts are refused without checking the password
// (ErrLoginLocked) until old failures leave the window.
const (
	LoginWindow                = 15 * time.Minute
	MaxLoginFailures           = 5
	MaxLoginFailuresPerIP      = 20
	MaxLoginFailuresPerAccount = 20
	TOTPWindow                 = 24 * time.Hour
	MaxTOTPFailuresPerAccount  = 10
	KnownAddressTTL            = 30 * 24 * time.Hour
)

// maxLoginKeys bounds the limiter's memory. Beyond it, new keys share one
// overflow budget per kind, which locks quickly under a flood of distinct
// keys while existing keys keep their own budgets.
const maxLoginKeys = 100_000

// forcedLoginSweep rate-limits sweeps triggered by the key limit.
const forcedLoginSweep = time.Second

// budget is one failure counter kind.
type budget struct {
	prefix string
	max    int
	window time.Duration
}

var (
	budgetPair    = budget{"e|ip:", MaxLoginFailures, LoginWindow}
	budgetIP      = budget{"ip:", MaxLoginFailuresPerIP, LoginWindow}
	budgetAccount = budget{"e:", MaxLoginFailuresPerAccount, LoginWindow}
	budgetTOTP    = budget{"totp:", MaxTOTPFailuresPerAccount, TOTPWindow}
)

// loginLimiter counts failed logins in memory (one process).
type loginLimiter struct {
	clock core.Clock

	mu          sync.Mutex
	failures    map[string][]time.Time // key -> failure times, oldest first
	windows     map[string]time.Duration
	known       map[string]time.Time // email|address -> last successful login
	lastSweep   time.Time
	forcedSweep time.Time
}

func newLoginLimiter(clock core.Clock) *loginLimiter {
	return &loginLimiter{
		clock:    clock,
		failures: map[string][]time.Time{},
		windows:  map[string]time.Duration{},
		known:    map[string]time.Time{},
	}
}

// reservation is a counted attempt; succeed or cancel takes it back.
type reservation struct {
	l    *loginLimiter
	keys []string
	at   time.Time
}

func addressKey(ip string) string { return api.IPKey(ip) }

// key returns the counter key of b for the given parts.
func (b budget) key(parts ...string) string {
	k := b.prefix
	for i, p := range parts {
		if i > 0 {
			k += "|"
		}
		k += p
	}
	return k
}

// recent drops failures older than the key's window and returns the rest.
func (l *loginLimiter) recent(key string, now time.Time) []time.Time {
	fs := l.failures[key]
	w := l.windows[key]
	i := 0
	for i < len(fs) && now.Sub(fs[i]) >= w {
		i++
	}
	fs = fs[i:]
	if len(fs) == 0 {
		delete(l.failures, key)
		delete(l.windows, key)
		return nil
	}
	l.failures[key] = fs
	return fs
}

// isKnown reports whether address logged in to email recently.
func (l *loginLimiter) isKnown(email, addr string, now time.Time) bool {
	t, ok := l.known[email+"|"+addr]
	return ok && now.Sub(t) < KnownAddressTTL
}

// sweep forgets expired failures and known addresses.
func (l *loginLimiter) sweep(now time.Time) {
	for k := range l.failures {
		l.recent(k, now)
	}
	for k, t := range l.known {
		if now.Sub(t) >= KnownAddressTTL {
			delete(l.known, k)
		}
	}
	l.lastSweep = now
}

// slot returns the key to count under: key itself, or the kind's overflow
// key when the limiter is full.
func (l *loginLimiter) slot(key string, b budget, now time.Time) string {
	if _, ok := l.failures[key]; ok || len(l.failures)+len(l.known) < maxLoginKeys {
		return key
	}
	if now.Sub(l.forcedSweep) >= forcedLoginSweep {
		l.sweep(now)
		l.forcedSweep = now
		if len(l.failures)+len(l.known) < maxLoginKeys {
			return key
		}
	}
	return b.prefix + "overflow"
}

// reserve counts one attempt against each (key, budget) pair, or none of
// them if any budget is exhausted (then it returns nil). The caller holds
// l.mu.
func (l *loginLimiter) reserve(now time.Time, keys []string, budgets []budget) *reservation {
	if now.Sub(l.lastSweep) >= LoginWindow {
		l.sweep(now)
	}
	for i, k := range keys {
		keys[i] = l.slot(k, budgets[i], now)
		if len(l.recent(keys[i], now)) >= budgets[i].max {
			return nil
		}
	}
	for i, k := range keys {
		l.failures[k] = append(l.failures[k], now)
		l.windows[k] = budgets[i].window
	}
	return &reservation{l: l, keys: keys, at: now}
}

// begin counts a login attempt for email from ip before the password is
// checked. It returns nil when the attempt must be refused
// (ErrLoginLocked): a budget is exhausted, or the account's TOTP budget is.
func (l *loginLimiter) begin(email, ip string) *reservation {
	now := l.clock.Now()
	addr := addressKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := []string{budgetPair.key(email, addr), budgetIP.key(addr)}
	budgets := []budget{budgetPair, budgetIP}
	if !l.isKnown(email, addr, now) {
		if len(l.recent(l.slot(budgetTOTP.key(email), budgetTOTP, now), now)) >= budgetTOTP.max {
			return nil
		}
		keys, budgets = append(keys, budgetAccount.key(email)), append(budgets, budgetAccount)
	}
	return l.reserve(now, keys, budgets)
}

// beginTOTP counts an authenticator code check for email from ip, after
// the password was right. It returns nil when the account's TOTP budget is
// exhausted. Known addresses are not counted (an empty reservation).
func (l *loginLimiter) beginTOTP(email, ip string) *reservation {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isKnown(email, addressKey(ip), now) {
		return &reservation{l: l}
	}
	return l.reserve(now, []string{budgetTOTP.key(email)}, []budget{budgetTOTP})
}

// cancel takes the attempt back: it was not a real credential check (e.g.
// the server was busy).
func (r *reservation) cancel() {
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	r.release()
}

// release removes the reservation's timestamps. The caller holds l.mu.
func (r *reservation) release() {
	for _, k := range r.keys {
		fs := r.l.failures[k]
		if i := slices.IndexFunc(fs, func(t time.Time) bool { return t.Equal(r.at) }); i >= 0 {
			r.l.failures[k] = slices.Delete(fs, i, i+1)
		}
		if len(r.l.failures[k]) == 0 {
			delete(r.l.failures, k)
			delete(r.l.windows, k)
		}
	}
	r.keys = nil
}

// succeed takes the attempt back, clears the failures of email from ip,
// and remembers ip as a known address of email.
func (l *loginLimiter) succeed(email, ip string, rs ...*reservation) {
	now := l.clock.Now()
	addr := addressKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rs {
		if r != nil {
			r.release()
		}
	}
	k := budgetPair.key(email, addr)
	delete(l.failures, k)
	delete(l.windows, k)
	kk := email + "|" + addr
	if _, ok := l.known[kk]; ok || len(l.failures)+len(l.known) < maxLoginKeys {
		l.known[kk] = now
	}
}
