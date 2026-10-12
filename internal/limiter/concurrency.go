// Package limiter contains small, explicit gateway control primitives.
package limiter

import (
	"context"
	"errors"
	"time"
)

// ConcurrencyGate optionally caps in-flight provider calls. It never retries,
// changes a request, or silently drops work: callers receive cancellation from
// their context while waiting and can expose QueueWait in their activity log.
type ConcurrencyGate struct{ slots chan struct{} }

// Admission describes the observable effect of acquiring a gate slot.
type Admission struct {
	Queued    bool
	QueueWait time.Duration
}

// NewConcurrencyGate creates a gate; zero disables the in-flight limit.
func NewConcurrencyGate(maxInFlight int) (*ConcurrencyGate, error) {
	if maxInFlight < 0 {
		return nil, errors.New("concurrency limit must not be negative")
	}
	if maxInFlight == 0 {
		return &ConcurrencyGate{}, nil
	}
	return &ConcurrencyGate{slots: make(chan struct{}, maxInFlight)}, nil
}

// Acquire waits for one slot or returns the caller's cancellation. Release
// must be called exactly once after a successful acquisition.
func (g *ConcurrencyGate) Acquire(ctx context.Context) (Admission, error) {
	if g == nil {
		return Admission{}, errors.New("concurrency gate is nil")
	}
	if err := ctx.Err(); err != nil {
		return Admission{}, err
	}
	if g.slots == nil {
		return Admission{}, nil
	}
	select {
	case g.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			g.Release()
			return Admission{}, err
		}
		return Admission{}, nil
	default:
	}
	started := time.Now()
	select {
	case g.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			g.Release()
			return Admission{}, err
		}
		return Admission{Queued: true, QueueWait: time.Since(started)}, nil
	case <-ctx.Done():
		return Admission{Queued: true, QueueWait: time.Since(started)}, ctx.Err()
	}
}

// Release returns one previously acquired slot.
func (g *ConcurrencyGate) Release() {
	if g.slots != nil {
		<-g.slots
	}
}
