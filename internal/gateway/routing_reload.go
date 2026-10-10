package gateway

import (
	"strconv"
	"time"
)

// A provider generation changes when its connection/credential configuration
// changes or a removed profile is added again. It contains no secret material.
func providerCacheNamespace(generation uint64) string {
	return "provider:" + strconv.FormatUint(generation, 10)
}

type routingReload struct {
	source           *autoChainState
	preserve         map[AutoChainEntry]bool
	preserveFailures map[AutoChainEntry]bool
	reset            bool
}

func (h *handler) prepareRouting(previous *handler, changedChain bool) {
	preserve := make(map[AutoChainEntry]bool, len(h.config.AutoChain))
	preserveFailures := make(map[AutoChainEntry]bool, len(h.config.AutoChain))
	for _, entry := range h.config.AutoChain {
		preserve[entry] = h.supportScopeAllows(entry.Provider, entry.Model) && h.providerRouteAvailable(entry.Provider)
		preserveFailures[entry] = h.reusedConnections[entry.Provider] && h.supportScopeAllows(entry.Provider, entry.Model)
	}
	// Each published view owns its chain; old completions cannot mutate it.
	h.preparedRouting = &routingReload{source: previous.autoChain, preserve: preserve, preserveFailures: preserveFailures, reset: changedChain}
	h.autoChain = previous.autoChain.reconfiguredWithFailures(h.config.AutoChain, time.Duration(h.config.ActiveSessionIdleTimeoutSeconds)*time.Second, preserve, preserveFailures, changedChain)
}

// activateRouting is called only after validation succeeds, immediately before
// publishing the immutable view. Copy at publication, not before slow probes,
// so bindings created while preparation was in progress are included. Old
// requests keep the old auto-chain state and cannot overwrite current affinity.
func (h *handler) activateRouting() {
	h.availability.activate(h.routingEpoch, h.providerGenerations, h.supportScopeAllows)
	if prepared := h.preparedRouting; prepared != nil {
		for _, entry := range h.config.AutoChain {
			prepared.preserve[entry] = h.supportScopeAllows(entry.Provider, entry.Model) && h.providerRouteAvailable(entry.Provider)
		}
		h.autoChain = prepared.source.reconfiguredWithFailures(h.config.AutoChain, time.Duration(h.config.ActiveSessionIdleTimeoutSeconds)*time.Second, prepared.preserve, prepared.preserveFailures, prepared.reset)
		h.preparedRouting = nil
	}
	h.sharedAffinity.activateForView(h.routingEpoch, func(binding sharedModelBinding) bool {
		return h.clients[binding.provider] != nil && h.supportScopeAllows(binding.provider, binding.model)
	}, func(provider string) uint64 { return h.providerGenerations[provider] }, h.providerRouteAvailable)
}

// New bindings among rule-equivalent random candidates prefer connections preserved by this
// reload. Explicit providers, price order and auto-chain order are rules and
// therefore never pass through this tie-breaker. This is configuration reuse,
// not a guess about the live TCP pool or a new health/failover policy.
func (h *handler) reusableRandomCandidates(candidates []string) []string {
	reusable := make([]string, 0, len(candidates))
	for _, provider := range candidates {
		if h.reusedConnections[provider] && h.providerRouteAvailable(provider) {
			reusable = append(reusable, provider)
		}
	}
	if len(reusable) > 0 {
		return reusable
	}
	return candidates
}
