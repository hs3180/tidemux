package limiter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConcurrencyGateWaitsThenAdmits(t *testing.T) {
	gate, err := NewConcurrencyGate(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := make(chan Admission, 1)
	go func() {
		admission, acquireErr := gate.Acquire(context.Background())
		if acquireErr != nil {
			t.Error(acquireErr)
			return
		}
		result <- admission
	}()
	select {
	case <-result:
		t.Fatal("second call bypassed concurrency gate")
	case <-time.After(15 * time.Millisecond):
	}
	gate.Release()
	admission := <-result
	if admission.QueueWait < 15*time.Millisecond {
		t.Fatalf("queue wait = %s, expected observable wait", admission.QueueWait)
	}
	gate.Release()
}

func TestConcurrencyGateHonorsCancellation(t *testing.T) {
	gate, err := NewConcurrencyGate(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := gate.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire error = %v", err)
	}
	gate.Release()
}

func TestAlreadyCanceledNeverAdmitted(t *testing.T) {
	gate, _ := NewConcurrencyGate(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 100; i++ {
		if _, err := gate.Acquire(ctx); err == nil {
			gate.Release()
			t.Fatal("canceled context admitted")
		}
	}
	if _, err := gate.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.Release()
}
