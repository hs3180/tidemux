package gateway

import (
	"math/rand"
	"net/http"
	"sort"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

func (h *handler) orderSharedModelProviders(names []string) []string {
	ordered := append([]string(nil), names...)
	sort.Strings(ordered)
	if len(ordered) < 2 {
		return ordered
	}
	choose := h.randomIndex
	if choose == nil {
		choose = rand.Intn
	}
	start := choose(len(ordered))
	if start < 0 || start >= len(ordered) {
		start = 0
	}
	return append(ordered[start:], ordered[:start]...)
}

func (h *handler) providerRouteAvailable(providerName string) bool {
	if _, unavailable := h.unavailableProviders[providerName]; unavailable || h.clients[providerName] == nil {
		return false
	}
	pool := h.keyPools[providerName]
	return pool == nil || pool.CooldownWaitAt(time.Now()) == 0
}

func (h *handler) resolveSharedModel(model string, requireKnownScope, qualifiedUnknown bool, clientProtocol string, body []byte) (providerName, resolvedModel, code string, status int) {
	allCandidates := h.bareModelCandidates(model, requireKnownScope)
	candidates := allCandidates
	requiresNativeTools := clientProtocol == "anthropic" && adapter.HasAnthropicServerTools(body)
	if requiresNativeTools {
		candidates = make([]string, 0, len(allCandidates))
		for _, name := range allCandidates {
			if client := h.clients[name]; client != nil && client.Protocol == "anthropic" {
				candidates = append(candidates, name)
			}
		}
	}
	if len(candidates) == 0 {
		if requiresNativeTools && len(allCandidates) > 0 {
			return "", "", "unsupported_request_feature", 400
		}
		if qualifiedUnknown {
			return "", "", "provider_not_found", http.StatusNotFound
		}
		return "", "", "model_not_found", http.StatusNotFound
	}
	if len(candidates) == 1 {
		return candidates[0], model, "", 0
	}
	if h.config.EffectiveRouting().SharedModelStrategy == "" {
		return "", "", "model_ambiguous", http.StatusBadRequest
	}
	available := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if h.providerRouteAvailable(name) {
			available = append(available, name)
		}
	}
	if len(available) == 0 {
		return "", "", "provider_keys_cooling_down", http.StatusServiceUnavailable
	}
	ordered := h.orderSharedModelProviders(available)
	return ordered[0], model, "", 0
}
