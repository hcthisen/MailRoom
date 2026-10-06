package events

import (
	"errors"
	"sync"
	"time"
)

// UnknownMessageGrace is how old (by mail.timestamp) an SES event for a
// message Mailroom doesn't know yet may be for it to be deferred once. The
// sender stores the SES MessageId only after SES has accepted the email, so
// a fast bounce or delivery can arrive before that is committed.
const UnknownMessageGrace = 2 * time.Minute

// deferralTTL is how long a deferred SNS MessageId is remembered. SNS's
// default HTTP delivery policy retries 3 times 20 s apart, so the retry
// arrives well within it.
const deferralTTL = 15 * time.Minute

// maxDeferrals bounds the deferral memory; beyond it events are processed
// at once (with the unknown-message fallback) rather than deferred.
const maxDeferrals = 4096

// errRetryLater marks an SES event for a message not recorded yet: nothing
// was written and the webhook answers 503 so SNS redelivers it.
var errRetryLater = errors.New("SES event for a message not recorded yet")

// deferrals remembers which SNS messages were already deferred once, so
// each is deferred at most once per process and a redelivery is always
// processed.
type deferrals struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newDeferrals() *deferrals { return &deferrals{seen: make(map[string]time.Time)} }

// first reports whether snsID has not been deferred before (and records
// it). It returns false when the memory is full of live entries.
func (d *deferrals) first(snsID string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if at, ok := d.seen[snsID]; ok && now.Sub(at) < deferralTTL {
		return false
	}
	if len(d.seen) >= maxDeferrals {
		for id, at := range d.seen {
			if now.Sub(at) >= deferralTTL {
				delete(d.seen, id)
			}
		}
		if len(d.seen) >= maxDeferrals {
			return false
		}
	}
	d.seen[snsID] = now
	return true
}

// shouldDefer reports whether an event for an unknown message is deferred:
// it was sent recently (mail.timestamp within UnknownMessageGrace, or in
// the future through clock skew) and this SNS message wasn't deferred yet.
func (s *Service) shouldDefer(snsID string, ev parsedEvent, now time.Time) bool {
	if ev.SentAt.IsZero() || now.Sub(ev.SentAt) >= UnknownMessageGrace {
		return false
	}
	return s.deferred.first(snsID, now)
}
