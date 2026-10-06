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
	source   *autoChainState
	preserve map[AutoChainEntry]bool
	reset    bool
}

func (h *handler) prepareRouting(previous *handler, changedChain bool) {
	preserve := make(map[AutoChainEntry]bool, len(h.config.AutoChain))
	compatible := true
	for _, entry := range h.config.AutoChain {
		preserve[entry] = h.reusedConnections[entry.Provider] && h.supportScopeAllows(entry.Provider, entry.Model)
		compatible = compatible && preserve[entry]
	}
	if changedChain || !compatible {
		h.preparedRouting = &routingReload{source: previous.autoChain, preserve: preserve, reset: changedChain}
		h.autoChain = previous.autoChain.reconfigured(h.config.AutoChain, time.Duration(h.config.ActiveSessionIdleTimeoutSeconds)*time.Second, preserve, changedChain)
	}
}

// activateRouting is called only after validation succeeds, immediately before
// publishing the immutable view. Copy at publication, not before slow probes,
// so bindings created while preparation was in progress are included. Old
// requests keep the old auto-chain state and cannot overwrite current affinity.
func (h *handler) activateRouting() {
	if prepared := h.preparedRouting; prepared != nil {
		h.autoChain = prepared.source.reconfigured(h.config.AutoChain, time.Duration(h.config.ActiveSessionIdleTimeoutSeconds)*time.Second, prepared.preserve, prepared.reset)
		h.preparedRouting = nil
	}
	h.sharedAffinity.activate(h.routingEpoch, func(binding sharedModelBinding) bool {
		return h.clients[binding.provider] != nil && h.providerGenerations[binding.provider] == binding.generation && h.supportScopeAllows(binding.provider, binding.model)
	})
}

// Rule-equivalent random candidates prefer connections preserved by this
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
