package limiter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrActiveSessionLimit is returned when a new logical session cannot be
// admitted. The value is intentionally stable because it is exposed as an API
// error code by the gateway.
var ErrActiveSessionLimit = errors.New("active_session_limit")

const defaultSessionIdleTTL = 15 * time.Minute

type sessionEntry struct {
	requests   int
	lastActive time.Time
	retain     bool
}

// SessionLimiter limits distinct logical sessions while allowing concurrent
// requests belonging to an already admitted session. A successful non-stream
// request keeps its session alive until the idle TTL; a streaming request
// releases its session when the stream ends.
type SessionLimiter struct {
	mu       sync.Mutex
	max      int
	idleTTL  time.Duration
	now      func() time.Time
	sessions map[string]*sessionEntry
	rejected uint64
}

// SessionLease represents one request belonging to a logical session.
type SessionLease struct {
	limiter *SessionLimiter
	id      string
	once    sync.Once
}

// NewSessionLimiter creates a limiter. A zero maximum disables the limit.
func NewSessionLimiter(max int) (*SessionLimiter, error) {
	if max < 0 {
		return nil, errors.New("active session limit cannot be negative")
	}
	return &SessionLimiter{
		max:      max,
		idleTTL:  defaultSessionIdleTTL,
		now:      time.Now,
		sessions: make(map[string]*sessionEntry),
	}, nil
}

// Acquire admits a request for id. A request for an already admitted session
// does not consume another session slot.
func (l *SessionLimiter) Acquire(ctx context.Context, id string) (*SessionLease, error) {
	if l == nil {
		return nil, errors.New("session limiter is nil")
	}
	if l.max == 0 {
		return &SessionLease{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("session id is required")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.expireLocked(now)
	entry, ok := l.sessions[id]
	if !ok {
		if len(l.sessions) >= l.max {
			l.rejected++
			return nil, ErrActiveSessionLimit
		}
		entry = &sessionEntry{}
		l.sessions[id] = entry
	}
	entry.requests++
	entry.lastActive = now
	return &SessionLease{limiter: l, id: id}, nil
}

// Release finishes a request. Retain should be true for a successful
// non-stream request so that the logical session remains active until idleTTL.
// Stream completion, cancellation and upstream errors should pass false.
func (l *SessionLease) Release(retain bool) {
	if l == nil || l.limiter == nil {
		return
	}
	l.once.Do(func() {
		limiter := l.limiter
		limiter.mu.Lock()
		defer limiter.mu.Unlock()
		entry, ok := limiter.sessions[l.id]
		if !ok {
			return
		}
		if entry.requests > 0 {
			entry.requests--
		}
		if retain {
			entry.retain = true
			entry.lastActive = limiter.now()
		}
		if entry.requests == 0 && !entry.retain {
			delete(limiter.sessions, l.id)
		}
	})
}

// Close drops all in-memory session state during gateway shutdown.
func (l *SessionLimiter) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.sessions = make(map[string]*sessionEntry)
	l.mu.Unlock()
}

// Stats returns the configured limit, current sessions and rejected admissions.
func (l *SessionLimiter) Stats() (limit, current int, rejected uint64) {
	if l == nil {
		return 0, 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expireLocked(l.now())
	return l.max, len(l.sessions), l.rejected
}

func (l *SessionLimiter) expireLocked(now time.Time) {
	for id, entry := range l.sessions {
		if entry.requests == 0 && now.Sub(entry.lastActive) >= l.idleTTL {
			delete(l.sessions, id)
		}
	}
}
