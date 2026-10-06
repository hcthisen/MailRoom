package testutil

import (
	"context"
	"fmt"
	"sync"

	"mailroom/internal/core"
)

// FakeProvider is an in-memory core.EmailProvider. It records every
// successful send and returns ids "fake-1", "fake-2", ... Errors can be
// queued with FailNext or made permanent with FailAlways. It is safe for
// concurrent use.
type FakeProvider struct {
	mu     sync.Mutex
	sent   []core.OutgoingEmail
	calls  int
	queue  []error
	always error
	seq    int
}

var _ core.EmailProvider = (*FakeProvider)(nil)

// NewFakeProvider returns an empty fake provider.
func NewFakeProvider() *FakeProvider { return &FakeProvider{} }

// Send records m and returns a new provider id, or the next programmed error.
func (p *FakeProvider) Send(ctx context.Context, m core.OutgoingEmail) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(p.queue) > 0 {
		err := p.queue[0]
		p.queue = p.queue[1:]
		if err != nil {
			return "", err
		}
	}
	if p.always != nil {
		return "", p.always
	}
	m.Raw = append([]byte(nil), m.Raw...)
	p.sent = append(p.sent, m)
	p.seq++
	return fmt.Sprintf("fake-%d", p.seq), nil
}

// FailNext makes the next len(errs) calls return errs in order; a nil entry
// lets that call succeed.
func (p *FakeProvider) FailNext(errs ...error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, errs...)
}

// FailAlways makes every call fail with err (nil restores success). Queued
// FailNext errors take precedence.
func (p *FakeProvider) FailAlways(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.always = err
}

// Sent returns a copy of every successfully sent message, in order.
func (p *FakeProvider) Sent() []core.OutgoingEmail {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]core.OutgoingEmail(nil), p.sent...)
}

// SentTo returns the successfully sent messages addressed to to.
func (p *FakeProvider) SentTo(to string) []core.OutgoingEmail {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []core.OutgoingEmail
	for _, m := range p.sent {
		if m.To == to {
			out = append(out, m)
		}
	}
	return out
}

// Count returns the number of successful sends.
func (p *FakeProvider) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

// Calls returns the number of Send calls, including failed ones.
func (p *FakeProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// Reset forgets sent messages, calls and programmed errors. Ids keep
// increasing, so they stay unique across a test.
func (p *FakeProvider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent, p.queue, p.always, p.calls = nil, nil, nil, 0
}
