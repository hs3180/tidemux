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

// autoChainState preserves valid logical-session routes and discards bindings
// to routes that are unavailable or have a safely classified failure.
type autoChainState struct {
	mu        sync.Mutex
	entries   []AutoChainEntry
	next      int
	ttl       time.Duration
	lastPrune time.Time
	sessions  map[string]autoChainBinding
	failed    map[int]bool
	now       func() time.Time // injectable clock, sampled while mu is held
}

func newAutoChainState(entries []AutoChainEntry, idleTTL time.Duration) *autoChainState {
	if idleTTL < defaultAutoChainSessionTTL {
		idleTTL = defaultAutoChainSessionTTL
	}
	return &autoChainState{
		entries:  append([]AutoChainEntry(nil), entries...),
		ttl:      idleTTL,
		sessions: make(map[string]autoChainBinding),
		failed:   make(map[int]bool),
	}
}

// reconfigured copies only unexpired, compatible semantic routes. Changing the
// chain is an explicit new ordering for new sessions, including an exhausted
// chain. Existing valid sessions remain pinned to their provider/model rather
// than to an index that may now mean a different route. The old view is intact.
func (s *autoChainState) reconfigured(entries []AutoChainEntry, idleTTL time.Duration, preserve map[AutoChainEntry]bool, resetPreference bool) *autoChainState {
	return s.reconfiguredWithFailures(entries, idleTTL, preserve, preserve, resetPreference)
}

func (s *autoChainState) reconfiguredWithFailures(entries []AutoChainEntry, idleTTL time.Duration, preserve, preserveFailures map[AutoChainEntry]bool, resetPreference bool) *autoChainState {
	next := newAutoChainState(entries, idleTTL)
	if s == nil {
		return next
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next.now = s.now
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	indices := make(map[AutoChainEntry]int, len(entries))
	for index, entry := range entries {
		indices[entry] = index
	}
	for index := range s.failed {
		entry := s.entries[index]
		if newIndex, exists := indices[entry]; exists && preserveFailures[entry] {
			next.failed[newIndex] = true
		}
	}
	for key, binding := range s.sessions {
		if now.Sub(binding.lastTouch) >= s.ttl || binding.index < 0 || binding.index >= len(s.entries) {
			continue
		}
		entry := s.entries[binding.index]
		if index, exists := indices[entry]; exists && preserve[entry] {
			binding.index = index
			next.sessions[key] = binding
		}
	}
	if !resetPreference {
		next.next = s.next
		// A previously failed route with new credentials/connection settings
		// is eligible again. Healthy semantic bindings remain untouched.
		for index := range s.failed {
			entry := s.entries[index]
			if newIndex, exists := indices[entry]; exists && !preserveFailures[entry] && newIndex < next.next {
				next.next = newIndex
			}
		}
	}
	next.lastPrune = now
	return next
}

func (s *autoChainState) selectForSession(sessionID string, sticky bool) (autoChainSelection, bool) {
	return s.selectForSessionAvailable(sessionID, sticky, nil)
}

func (s *autoChainState) selectForSessionAvailable(sessionID string, sticky bool, available func(AutoChainEntry) bool) (autoChainSelection, bool) {
	if s == nil || sessionID == "" || len(s.entries) == 0 {
		return autoChainSelection{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if sticky && (s.lastPrune.IsZero() || now.Sub(s.lastPrune) >= autoChainPruneInterval) {
		for id, binding := range s.sessions {
			if now.Sub(binding.lastTouch) >= s.ttl {
				delete(s.sessions, id)
			}
		}
		s.lastPrune = now
	}
	valid := func(index int) bool {
		return index >= 0 && index < len(s.entries) && !s.failed[index] && (available == nil || available(s.entries[index]))
	}
	if sticky {
		key := hashAutoChainSessionID(sessionID)
		if binding, ok := s.sessions[key]; ok && now.Sub(binding.lastTouch) < s.ttl && valid(binding.index) {
			binding.lastTouch = now
			s.sessions[key] = binding
			return s.selection(binding.index), true
		}
		delete(s.sessions, key)
	}
	index := s.next
	for index < len(s.entries) && !valid(index) {
		index++
	}
	if index >= len(s.entries) {
		return autoChainSelection{}, false
	}
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
	if callErr == nil || !callErr.FailoverSafe || ctxErr != nil {
		return false
	}
	if callErr.Code == "upstream_transport_error" && callErr.UpstreamNotAttempted {
		return !delivered
	}
	return callErr.Category == adapter.ProviderErrorModelNotFound || callErr.Category == adapter.ProviderErrorInsufficientBalance || callErr.Category == adapter.ProviderErrorTemporarilyUnavailable
}

func (s *autoChainState) selectRoute(key *sharedModelSessionKey) (routeStep, bool) {
	return s.selectRouteAvailable(key, nil)
}

func (s *autoChainState) selectRouteAvailable(key *sharedModelSessionKey, available func(AutoChainEntry) bool) (routeStep, bool) {
	id := "request-scoped"
	if key != nil {
		id = hex.EncodeToString(key[:])
	}
	selection, ok := s.selectForSessionAvailable(id, key != nil, available)
	if !ok {
		return routeStep{}, false
	}
	index := selection.index
	return routeStep{provider: selection.provider, model: selection.model, trigger: routeInitial, autoChainIndex: &index}, true
}

func (s *autoChainState) recordFailure(index, _ int, callErr *adapter.CallError, ctxErr error, delivered bool) {
	if s == nil || !autoChainFailureAdvances(callErr, ctxErr, delivered) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.entries) {
		return
	}
	s.failed[index] = true
	for key, binding := range s.sessions {
		if binding.index == index {
			delete(s.sessions, key)
		}
	}
	for s.next < len(s.entries) && s.failed[s.next] {
		s.next++
	}
}

func autoChainFailureCanAdvance(callErr *adapter.CallError, ctxErr error, delivered bool) bool {
	return autoChainFailureAdvances(callErr, ctxErr, delivered)
}

func hashAutoChainSessionID(sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(digest[:])
}
