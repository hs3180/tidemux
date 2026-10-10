package gateway

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

const sharedModelSessionIdleTTL = 24 * time.Hour

type sharedModelSessionKey [sha256.Size]byte

type sharedModelRoute struct {
	provider   string
	model      string
	generation uint64
}

type sharedModelBinding struct {
	provider   string
	lastUsed   time.Time
	model      string
	generation uint64
}

// sharedModelAffinity stores only hashed routing keys, model/provider references,
// connection generations and timestamps. It belongs to the shared runtime and
// is independent of model:auto and active-session admission. Selection and
// insertion share one lock so concurrent
// first requests cannot pick different providers for the same session.
type sharedModelAffinity struct {
	mu           sync.Mutex
	bindings     map[sharedModelSessionKey]sharedModelBinding
	nextSweep    time.Time
	now          func() time.Time
	activeEpoch  uint64
	failedRoutes map[sharedModelRoute]*routeFailure
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
	return a.selectProviderForView(key, "", 0, candidates, available, choose, func(string) uint64 { return 0 })
}

func (a *sharedModelAffinity) activate(epoch uint64, valid func(sharedModelBinding) bool, generation ...func(string) uint64) {
	var currentGeneration func(string) uint64
	if len(generation) > 0 {
		currentGeneration = generation[0]
	}
	a.activateForView(epoch, valid, currentGeneration, nil)
}

func (a *sharedModelAffinity) activateForView(epoch uint64, valid func(sharedModelBinding) bool, generation func(string) uint64, available func(string) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activeEpoch = epoch
	for route, failure := range a.failedRoutes {
		if !valid(sharedModelBinding{provider: route.provider, model: route.model, generation: route.generation}) || generation != nil && route.generation != generation(route.provider) {
			delete(a.failedRoutes, route)
		} else {
			// An old view's in-flight probe cannot own the new view's state.
			a.failedRoutes[route] = &routeFailure{retryAt: failure.retryAt}
		}
	}
	for key, binding := range a.bindings {
		if !valid(binding) || available != nil && !available(binding.provider) {
			delete(a.bindings, key)
		} else if generation != nil {
			binding.generation = generation(binding.provider)
			a.bindings[key] = binding
		}
	}
}

func (a *sharedModelAffinity) selectProviderForView(key sharedModelSessionKey, model string, epoch uint64, candidates []string, available func(string) bool, choose func(int) int, generation func(string) uint64, prefer ...func([]string) []string) (string, bool) {
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
		route := sharedModelRoute{provider: provider, model: model, generation: generation(provider)}
		if a.failedRoutes[route].eligible(now) && available(provider) {
			eligible = append(eligible, provider)
		}
	}
	if binding, exists := a.bindings[key]; exists && now.Sub(binding.lastUsed) < sharedModelSessionIdleTTL && containsModel(eligible, binding.provider) && binding.generation == generation(binding.provider) {
		binding.lastUsed = now
		if epoch == a.activeEpoch {
			a.bindings[key] = binding
		}
		return binding.provider, true
	}
	if epoch == a.activeEpoch {
		delete(a.bindings, key)
	}
	if len(eligible) == 0 {
		return "", false
	}
	if len(prefer) > 0 && prefer[0] != nil {
		eligible = prefer[0](eligible)
	}
	index := choose(len(eligible))
	if index < 0 || index >= len(eligible) {
		index = 0
	}
	provider := eligible[index]
	if epoch == a.activeEpoch {
		a.bindings[key] = sharedModelBinding{provider: provider, lastUsed: now, model: model, generation: generation(provider)}
	}
	return provider, true
}

// recordFailure invalidates only the failed provider/model and connection
// generation. It affects future requests, never replays a dispatched request.
// Old admitted views cannot invalidate the active view's bindings.
func (a *sharedModelAffinity) recordFailure(epoch uint64, provider, model string, generation uint64, callErr *adapter.CallError, ctxErr error, delivered bool) {
	if !autoChainFailureAdvances(callErr, ctxErr, delivered) || callErr.Category == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if epoch != a.activeEpoch {
		return
	}
	if a.failedRoutes == nil {
		a.failedRoutes = make(map[sharedModelRoute]*routeFailure)
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	a.failedRoutes[sharedModelRoute{provider: provider, model: model, generation: generation}] = &routeFailure{retryAt: now.Add(recoveryCooldown(callErr))}
	for key, binding := range a.bindings {
		if binding.provider == provider && binding.model == model && binding.generation == generation {
			delete(a.bindings, key)
		}
	}
}

func (a *sharedModelAffinity) beginRecovery(epoch uint64, route sharedModelRoute) (*routeFailure, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if epoch != a.activeEpoch {
		return nil, true // Old admitted views cannot mutate active health state.
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	probe := a.failedRoutes[route]
	if !probe.eligible(now) {
		return nil, false
	}
	if probe != nil {
		probe.probing = true
	}
	return probe, true
}

func (a *sharedModelAffinity) finishRecovery(epoch uint64, route sharedModelRoute, probe *routeFailure, handled bool, callErr, ctxErr error) {
	if probe == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if epoch != a.activeEpoch || a.failedRoutes[route] != probe {
		return
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	if completeRecovery(probe, now, handled, callErr, ctxErr) {
		delete(a.failedRoutes, route)
	}
}
