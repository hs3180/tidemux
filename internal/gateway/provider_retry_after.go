package gateway

import (
	"strings"
	"sync"
	"time"
)

func sameProviderEndpoint(before, after Provider) bool {
	return before.Protocol == after.Protocol && strings.TrimRight(before.BaseURL, "/") == strings.TrimRight(after.BaseURL, "/")
}

// A continuous provider endpoint shares server waits across admitted views and
// credential generations. Key health and routing observations retain their own
// generation/epoch fences. The pointer is immutable after view preparation.
type providerRetryState struct {
	mu      sync.Mutex
	until   time.Time
	version uint64
	probe   *rateRecoveryLease
}

type providerRetrySnapshot struct {
	until   time.Time
	version uint64
	probe   *rateRecoveryLease
}

func (s *providerRetryState) snapshot() providerRetrySnapshot {
	if s == nil {
		return providerRetrySnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return providerRetrySnapshot{s.until, s.version, s.probe}
}
