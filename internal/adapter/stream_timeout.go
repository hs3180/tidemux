package adapter

import (
	"context"
	"io"
	"sync"
	"time"
)

const defaultStreamIdleTimeout = 10 * time.Minute

// streamIdleTimer only runs while waiting for upstream headers or body bytes.
// Generation fencing prevents an already scheduled callback from canceling a
// later read after progress, or a completed request after stop.
type streamIdleTimer struct {
	mu         sync.Mutex
	timer      *time.Timer
	generation uint64
	cancel     context.CancelCauseFunc
}

func (t *streamIdleTimer) start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		return // A zero-byte read must not refresh the inactivity budget.
	}
	t.generation++
	generation := t.generation
	t.timer = time.AfterFunc(defaultStreamIdleTimeout, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.generation == generation {
			t.cancel(context.DeadlineExceeded)
		}
	})
}

func (t *streamIdleTimer) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.generation++
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

func (t *streamIdleTimer) close() {
	t.stop()
	t.cancel(nil)
}

type streamIdleBody struct {
	io.ReadCloser
	idle *streamIdleTimer
}

func (b streamIdleBody) Read(p []byte) (int, error) {
	b.idle.start()
	n, err := b.ReadCloser.Read(p)
	if n > 0 || err != nil {
		b.idle.stop()
	}
	return n, err
}
