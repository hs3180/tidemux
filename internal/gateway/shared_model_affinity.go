package gateway

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

const sharedModelSessionIdleTTL = 24 * time.Hour

type sharedModelSessionKey [sha256.Size]byte

type sharedModelBinding struct {
	provider string
	lastUsed time.Time
}

// sharedModelAffinity stores only hashed routing keys, provider references and
// timestamps. It belongs to one handler and is independent of model:auto and
// active-session admission. Selection and insertion share one lock so concurrent
// first requests cannot pick different providers for the same session.
type sharedModelAffinity struct {
	mu        sync.Mutex
	bindings  map[sharedModelSessionKey]sharedModelBinding
	nextSweep time.Time
	now       func() time.Time
}

func newSharedModelSessionKey(namespace, protocol, model, sessionID string) sharedModelSessionKey {
	hash := sha256.New()
	var size [8]byte
	for _, field := range []string{namespace, protocol, strings.TrimSpace(model), strings.TrimSpace(sessionID)} {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(field))
	}
	var key sharedModelSessionKey
	copy(key[:], hash.Sum(nil))
	return key
}

func (h *handler) sharedSessionKey(model, protocol, sessionID string) *sharedModelSessionKey {
	if h.config.EffectiveRouting().SharedModelStrategy != "random" || strings.TrimSpace(sessionID) == "" || model == "auto" {
		return nil
	}
	if ref, _, qualified := parseQualifiedModelID(model); qualified {
		if _, exists := h.providers[ref]; exists {
			return nil
		}
	}
	return h.callerSessionKey(model, protocol, sessionID)
}

func (h *handler) callerSessionKey(model, protocol, sessionID string) *sharedModelSessionKey {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	// The current gateway authenticates one local access token. Its digest is
	// the caller namespace; neither that credential nor its digest is stored.
	namespace := sha256.Sum256([]byte(h.config.AccessToken))
	key := newSharedModelSessionKey(string(namespace[:]), protocol, model, sessionID)
	return &key
}

func (a *sharedModelAffinity) selectProvider(key sharedModelSessionKey, candidates []string, available func(string) bool, choose func(int) int) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	if a.bindings == nil {
		a.bindings = make(map[sharedModelSessionKey]sharedModelBinding)
	}
	if !now.Before(a.nextSweep) {
		for key, binding := range a.bindings {
			if now.Sub(binding.lastUsed) >= sharedModelSessionIdleTTL {
				delete(a.bindings, key)
			}
		}
		a.nextSweep = now.Add(time.Minute)
	}
	eligible := make([]string, 0, len(candidates))
	for _, provider := range candidates {
		if available(provider) {
			eligible = append(eligible, provider)
		}
	}
	if binding, exists := a.bindings[key]; exists && now.Sub(binding.lastUsed) < sharedModelSessionIdleTTL && containsModel(eligible, binding.provider) {
		binding.lastUsed = now
		a.bindings[key] = binding
		return binding.provider, true
	}
	delete(a.bindings, key)
	if len(eligible) == 0 {
		return "", false
	}
	index := choose(len(eligible))
	if index < 0 || index >= len(eligible) {
		index = 0
	}
	provider := eligible[index]
	a.bindings[key] = sharedModelBinding{provider: provider, lastUsed: now}
	return provider, true
}
