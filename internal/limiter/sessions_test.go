package limiter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionLimiterCountsDistinctSessions(t *testing.T) {
	limiter, err := NewSessionLimiter(1)
	if err != nil {
		t.Fatal(err)
	}
	first, err := limiter.Acquire(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := limiter.Acquire(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Acquire(context.Background(), "session-b"); !errors.Is(err, ErrActiveSessionLimit) {
		t.Fatalf("new session error = %v", err)
	}
	first.Release(false)
	if _, err := limiter.Acquire(context.Background(), "session-b"); !errors.Is(err, ErrActiveSessionLimit) {
		t.Fatalf("session remained admitted after one request ended: %v", err)
	}
	second.Release(false)
	third, err := limiter.Acquire(context.Background(), "session-b")
	if err != nil {
		t.Fatal(err)
	}
	third.Release(false)
}

func TestSessionLimiterRetainsSuccessfulSessionUntilIdleTTL(t *testing.T) {
	limiter, err := NewSessionLimiter(1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return now }
	limiter.idleTTL = time.Minute
	lease, err := limiter.Acquire(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(true)
	if _, err := limiter.Acquire(context.Background(), "session-b"); !errors.Is(err, ErrActiveSessionLimit) {
		t.Fatalf("new session error = %v", err)
	}
	now = now.Add(time.Minute)
	other, err := limiter.Acquire(context.Background(), "session-b")
	if err != nil {
		t.Fatal(err)
	}
	other.Release(false)
}

func TestSessionLimiterDisabledAndCancellation(t *testing.T) {
	limiter, err := NewSessionLimiter(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := limiter.Acquire(ctx, "session-a"); err != nil {
		t.Fatalf("disabled acquire error = %v", err)
	}
	lease, err := limiter.Acquire(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(false)
	if limit, current, rejected := limiter.Stats(); limit != 0 || current != 0 || rejected != 0 {
		t.Fatalf("disabled stats = %d/%d/%d", limit, current, rejected)
	}
}
