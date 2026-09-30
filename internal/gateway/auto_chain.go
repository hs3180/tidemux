package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

const defaultAutoChainSessionTTL = 24 * time.Hour
const autoChainPruneInterval = time.Minute

type autoChainSelection struct {
	provider string
	model    string
	index    int
}

type autoChainBinding struct {
	index     int
	lastTouch time.Time
}

// autoChainState pins stable logical sessions while advancing the preferred
// entry only for sessions created after a classified failure.
type autoChainState struct {
	mu        sync.Mutex
	entries   []AutoChainEntry
	next      int
	ttl       time.Duration
	lastPrune time.Time
	sessions  map[string]autoChainBinding
}

func newAutoChainState(entries []AutoChainEntry, idleTTL time.Duration) *autoChainState {
	if idleTTL < defaultAutoChainSessionTTL {
		idleTTL = defaultAutoChainSessionTTL
	}
	return &autoChainState{
		entries:  append([]AutoChainEntry(nil), entries...),
		ttl:      idleTTL,
		sessions: make(map[string]autoChainBinding),
	}
}

func (s *autoChainState) selectForSession(sessionID string, sticky bool) (autoChainSelection, bool) {
	if s == nil || sessionID == "" || len(s.entries) == 0 {
		return autoChainSelection{}, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if sticky && (s.lastPrune.IsZero() || now.Sub(s.lastPrune) >= autoChainPruneInterval) {
		for id, binding := range s.sessions {
			if now.Sub(binding.lastTouch) >= s.ttl {
				delete(s.sessions, id)
			}
		}
		s.lastPrune = now
	}
	if sticky {
		key := hashAutoChainSessionID(sessionID)
		if binding, ok := s.sessions[key]; ok {
			binding.lastTouch = now
			s.sessions[key] = binding
			return s.selection(binding.index), true
		}
	}
	if s.next >= len(s.entries) {
		return autoChainSelection{}, false
	}
	index := s.next
	if sticky {
		s.sessions[hashAutoChainSessionID(sessionID)] = autoChainBinding{index: index, lastTouch: now}
	}
	return s.selection(index), true
}

func (s *autoChainState) selection(index int) autoChainSelection {
	entry := s.entries[index]
	return autoChainSelection{provider: entry.Provider, model: entry.Model, index: index}
}

func (s *autoChainState) advanceForNewSessions(index int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if index == s.next && s.next < len(s.entries) {
		s.next++
	}
}

func autoChainFailureAdvances(callErr *adapter.CallError, ctxErr error, delivered bool) bool {
	if callErr == nil || ctxErr != nil || delivered {
		return false
	}
	if callErr.Code == "upstream_transport_error" && callErr.Retryable {
		return true
	}
	return callErr.Category == adapter.ProviderErrorModelNotFound || callErr.Category == adapter.ProviderErrorInsufficientBalance || callErr.Category == adapter.ProviderErrorTemporarilyUnavailable
}

func hashAutoChainSessionID(sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(digest[:])
}
