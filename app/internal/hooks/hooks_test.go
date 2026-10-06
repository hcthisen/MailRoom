package hooks

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"mailroom/internal/core"
)

func TestPublishOrderAndStop(t *testing.T) {
	ctx := context.Background()
	errStop := errors.New("stop")
	var b Bus
	var calls []string
	b.OnTagAdded(func(_ context.Context, _ *sql.Tx, e TagAdded) error {
		calls = append(calls, "first:"+e.TagName)
		return nil
	})
	b.OnTagAdded(func(context.Context, *sql.Tx, TagAdded) error {
		calls = append(calls, "second")
		return errStop
	})
	b.OnTagAdded(func(context.Context, *sql.Tx, TagAdded) error {
		calls = append(calls, "third")
		return nil
	})
	err := b.PublishTagAdded(ctx, nil, TagAdded{ContactID: 1, TagName: "vip"})
	if !errors.Is(err, errStop) {
		t.Fatalf("err = %v, want errStop", err)
	}
	if len(calls) != 2 || calls[0] != "first:vip" || calls[1] != "second" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestAllEvents(t *testing.T) {
	ctx := context.Background()
	b := New()
	var got []any
	b.OnStatusChanged(func(_ context.Context, _ *sql.Tx, e StatusChanged) error { got = append(got, e); return nil })
	b.OnContactCreated(func(_ context.Context, _ *sql.Tx, e ContactCreated) error { got = append(got, e); return nil })
	sc := StatusChanged{ContactID: 1, From: core.StatusActive, To: core.StatusBounced, Source: "bounce", MessageID: 9}
	cc := ContactCreated{ContactID: 2, Source: "api"}
	if err := b.PublishStatusChanged(ctx, nil, sc); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishContactCreated(ctx, nil, cc); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishTagAdded(ctx, nil, TagAdded{}); err != nil {
		t.Fatal("no handlers must be a no-op")
	}
	if len(got) != 2 || got[0] != sc || got[1] != cc {
		t.Fatalf("got = %v", got)
	}
}

func TestNilBus(t *testing.T) {
	var b *Bus
	ctx := context.Background()
	if b.PublishTagAdded(ctx, nil, TagAdded{}) != nil ||
		b.PublishStatusChanged(ctx, nil, StatusChanged{}) != nil ||
		b.PublishContactCreated(ctx, nil, ContactCreated{}) != nil {
		t.Fatal("nil bus must publish to nobody")
	}
}

func TestConcurrentRegistration(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.OnContactCreated(func(context.Context, *sql.Tx, ContactCreated) error { return nil })
			_ = b.PublishContactCreated(context.Background(), nil, ContactCreated{})
		}()
	}
	wg.Wait()
	if len(b.contactCreated) != 50 {
		t.Fatalf("registered %d handlers, want 50", len(b.contactCreated))
	}
}
