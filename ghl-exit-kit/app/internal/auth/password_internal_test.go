package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"mailroom/internal/testutil"
)

// fillHashSlots occupies every hash slot and n waiter places until the
// returned func is called.
func fillHashSlots(t *testing.T, waiters int32) func() {
	t.Helper()
	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	hashWaiters.Add(waiters)
	return func() {
		hashWaiters.Add(-waiters)
		for range cap(hashSlots) {
			<-hashSlots
		}
	}
}

func TestHashSlotsHonourContextAndBound(t *testing.T) {
	p := Argon2Params{Time: 1, Memory: 64, Threads: 1, SaltLen: 16, KeyLen: 32}
	h, err := HashPassword(context.Background(), "correct horse battery", p)
	if err != nil {
		t.Fatal(err)
	}
	release := fillHashSlots(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := VerifyPassword(ctx, "x", h); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting past the deadline = %v", err)
	}
	if hashWaiters.Load() != 0 {
		t.Fatalf("waiter count leaked: %d", hashWaiters.Load())
	}
	hashWaiters.Add(maxHashWaiters)
	if _, err := HashPassword(context.Background(), "x", p); !errors.Is(err, ErrBusy) {
		t.Fatalf("full queue = %v", err)
	}
	hashWaiters.Add(-maxHashWaiters)
	release()
	if ok, err := VerifyPassword(context.Background(), "correct horse battery", h); err != nil || !ok {
		t.Fatalf("after release = %v %v", ok, err)
	}
}

// TestBusyLoginIsNotCounted: a login refused because the server is busy
// doesn't use up the account's failure budget.
func TestBusyLoginIsNotCounted(t *testing.T) {
	clock := testutil.NewClock(time.Time{})
	svc := New(Deps{DB: testutil.NewDB(t), Clock: clock, Argon2: Argon2Params{Time: 1, Memory: 64, Threads: 1, SaltLen: 16, KeyLen: 32}})
	if _, _, err := svc.CreateUser(context.Background(), CreateUserInput{Email: "owner@example.com", Password: "the right password"}); err != nil {
		t.Fatal(err)
	}
	release := fillHashSlots(t, maxHashWaiters)
	for range 2 * MaxLoginFailures {
		if _, err := svc.Login(context.Background(), LoginInput{Email: "owner@example.com", Password: "wrong", IP: "1.1.1.1"}); !errors.Is(err, ErrBusy) {
			release()
			t.Fatalf("busy login = %v", err)
		}
	}
	release()
	for i := range MaxLoginFailures {
		if _, err := svc.Login(context.Background(), LoginInput{Email: "owner@example.com", Password: "wrong", IP: "1.1.1.1"}); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d after busy = %v", i, err)
		}
	}
	if _, err := svc.Login(context.Background(), LoginInput{Email: "owner@example.com", Password: "wrong", IP: "1.1.1.1"}); !errors.Is(err, ErrLoginLocked) {
		t.Fatalf("after the budget = %v", err)
	}
}
