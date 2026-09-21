package limiter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionLimiterCountsDistinctSessions(t *testing.T) {
	limiter, err := NewSessionLimiter(1, 0)
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
	limiter, err := NewSessionLimiter(1, 0)
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
	limiter, err := NewSessionLimiter(0, 0)
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

func TestSessionLimiterUsesFiveMinuteDefaultAndConfigurableIdleTTL(t *testing.T) {
	limiter, err := NewSessionLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if limiter.idleTTL != 5*time.Minute {
		t.Fatalf("default idle TTL = %s", limiter.idleTTL)
	}
	custom, err := NewSessionLimiter(1, 37*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if custom.idleTTL != 37*time.Second {
		t.Fatalf("custom idle TTL = %s", custom.idleTTL)
	}
	if _, err := NewSessionLimiter(1, -time.Second); err == nil {
		t.Fatal("negative idle TTL accepted")
	}
}

func TestSessionLimiterTreatsInputOrOutputAsActivity(t *testing.T) {
	for _, activity := range []string{"input", "output"} {
		t.Run(activity, func(t *testing.T) {
			limiter, err := NewSessionLimiter(1, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
			limiter.now = func() time.Time { return now }
			lease, err := limiter.Acquire(context.Background(), "session-a")
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(30 * time.Second)
			if activity == "input" {
				lease.TouchInput()
			} else {
				lease.TouchOutput()
			}
			lease.Release(true)

			now = now.Add(40 * time.Second)
			if _, err := limiter.Acquire(context.Background(), "session-b"); !errors.Is(err, ErrActiveSessionLimit) {
				t.Fatalf("session expired with recent %s activity: %v", activity, err)
			}
			now = now.Add(21 * time.Second)
			other, err := limiter.Acquire(context.Background(), "session-b")
			if err != nil {
				t.Fatal(err)
			}
			other.Release(false)
		})
	}
}
