package gateway

import (
	"errors"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/diagnostics"
)

type routeStep struct {
	provider       string
	model          string
	sessionKey     *sharedModelSessionKey
	autoChainIndex *int
	dynamic        bool
	holdAffinity   bool
	decision       *diagnostics.RoutingDecision
}

func (h *handler) resolveRoutes(modelID, clientProtocol string, body []byte, sessionKey *sharedModelSessionKey) ([]routeStep, string, int) {
	requiresNativeTools := clientProtocol == "anthropic" && adapter.HasAnthropicServerTools(body)
	if ref, model, qualified := parseQualifiedModelID(modelID); qualified {
		if _, exists := h.providers[ref]; exists {
			if model == "auto" {
				return nil, "auto_model_must_be_unqualified", http.StatusBadRequest
			}
			if requiresNativeTools && !h.supportsNativeAnthropicTools(ref) {
				return nil, "unsupported_request_feature", http.StatusBadRequest
			}
			return []routeStep{{provider: ref, model: model}}, "", 0
		}
		allCandidates := h.bareModelCandidates(modelID, true)
		candidates := h.routingCandidates(modelID, true, clientProtocol, body)
		if len(candidates) == 0 && len(allCandidates) > 0 && (requiresNativeTools || h.config.EffectiveRouting().SharedModelStrategy != "") {
			return nil, "unsupported_request_feature", http.StatusBadRequest
		}
		return h.routesForBareModel(modelID, candidates, true, clientProtocol, body, sessionKey)
	}
	if modelID == "auto" {
		if len(h.config.AutoChain) == 0 {
			return nil, "auto_chain_unconfigured", http.StatusBadRequest
		}
		compatible := false
		for _, entry := range h.config.AutoChain {
			compatible = compatible || h.supportScopeAllows(entry.Provider, entry.Model) && h.routeRequestCompatible(entry.Provider, entry.Model, clientProtocol, body)
		}
		if !compatible {
			return nil, "unsupported_request_feature", http.StatusBadRequest
		}
		var compatibleRoutes []routeStep
		for i, entry := range h.config.AutoChain {
			if h.supportScopeAllows(entry.Provider, entry.Model) && h.routeRequestCompatible(entry.Provider, entry.Model, clientProtocol, body) {
				index := i
				compatibleRoutes = append(compatibleRoutes, routeStep{provider: entry.Provider, model: entry.Model, autoChainIndex: &index, dynamic: true})
			}
		}
		preferred, found := h.autoChain.boundRoute(sessionKey)
		var decision *diagnostics.RoutingDecision
		if found && h.supportScopeAllows(preferred.provider, preferred.model) && h.routeRequestCompatible(preferred.provider, preferred.model, clientProtocol, body) {
			held, retain := h.retainedRoute(preferred, compatibleRoutes)
			if retain {
				return []routeStep{held}, "", 0
			}
			decision = held.decision
		}
		route, ok := h.autoChain.selectRouteAvailable(sessionKey, func(entry AutoChainEntry) bool {
			return h.supportScopeAllows(entry.Provider, entry.Model) && h.routeRequestCompatible(entry.Provider, entry.Model, clientProtocol, body) && h.modelRouteAvailable(entry.Provider, entry.Model)
		})
		if !ok {
			return nil, "auto_chain_exhausted", http.StatusServiceUnavailable
		}
		route.sessionKey = sessionKey
		if decision != nil {
			if route.provider != decision.FromProvider {
				decision.ToProvider = route.provider
				if route.model != decision.Model {
					decision.ToModel = route.model
				}
				route.decision = decision
			}
		}
		return []routeStep{route}, "", 0
	}

	allCandidates := h.bareModelCandidates(modelID, false)
	candidates := h.routingCandidates(modelID, false, clientProtocol, body)
	if len(candidates) == 0 && len(allCandidates) > 0 && (requiresNativeTools || h.config.EffectiveRouting().SharedModelStrategy != "") {
		return nil, "unsupported_request_feature", http.StatusBadRequest
	}
	return h.routesForBareModel(modelID, candidates, false, clientProtocol, body, sessionKey)
}

func (h *handler) routesForBareModel(model string, candidates []string, qualifiedUnknown bool, clientProtocol string, body []byte, sessionKey *sharedModelSessionKey) ([]routeStep, string, int) {
	if len(candidates) == 0 {
		if qualifiedUnknown {
			return nil, "provider_not_found", http.StatusNotFound
		}
		return nil, "model_not_found", http.StatusNotFound
	}
	if h.config.EffectiveRouting().SharedModelStrategy == "random" && sessionKey != nil {
		var decision *diagnostics.RoutingDecision
		var alternatives []routeStep
		for _, provider := range candidates {
			alternatives = append(alternatives, routeStep{provider: provider, model: model})
		}
		if bound, ok := h.sharedAffinity.boundRoute(*sessionKey, model, h.routingEpoch, candidates, h.providerGenerations); ok {
			held, retain := h.retainedRoute(bound, alternatives)
			if retain {
				return []routeStep{held}, "", 0
			}
			decision = held.decision
		}
		selected, ok := h.sharedAffinity.selectProviderForView(*sessionKey, model, h.routingEpoch, candidates, func(provider string) bool { return h.modelRouteAvailable(provider, model) }, h.chooseRandomProvider, func(provider string) uint64 { return h.providerGenerations[provider] }, h.reusableRandomCandidates)
		if !ok {
			return nil, "provider_keys_cooling_down", http.StatusServiceUnavailable
		}
		// Session affinity selects one provider for this downstream request.
		if decision != nil {
			if selected == decision.FromProvider {
				decision = nil
			} else {
				decision.ToProvider = selected
			}
		}
		return []routeStep{{provider: selected, model: model, sessionKey: sessionKey, dynamic: true, decision: decision}}, "", 0
	}
	if len(candidates) == 1 {
		route := routeStep{provider: candidates[0], model: model, dynamic: true}
		if h.config.EffectiveRouting().SharedModelStrategy == "price_priority" {
			if held, retain := h.retainedRoute(route, nil); retain {
				return []routeStep{held}, "", 0
			}
		}
		return []routeStep{route}, "", 0
	}
	if h.config.EffectiveRouting().SharedModelStrategy == "" {
		return nil, "model_ambiguous", http.StatusBadRequest
	}
	var decision *diagnostics.RoutingDecision
	if h.config.EffectiveRouting().SharedModelStrategy == "price_priority" {
		if preferred, err := h.orderSharedModelProviders(model, candidates); err == nil && len(preferred) > 0 {
			var alternatives []routeStep
			for _, provider := range preferred {
				alternatives = append(alternatives, routeStep{provider: provider, model: model, dynamic: true})
			}
			held, retain := h.retainedRoute(alternatives[0], alternatives)
			if retain {
				return []routeStep{held}, "", 0
			}
			decision = held.decision
		}
	}
	available := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if h.modelRouteAvailable(name, model) {
			available = append(available, name)
		}
	}
	if len(available) == 0 {
		return nil, "provider_keys_cooling_down", http.StatusServiceUnavailable
	}
	if h.config.EffectiveRouting().SharedModelStrategy == "random" {
		available = h.reusableRandomCandidates(available)
	}
	ordered, err := h.orderSharedModelProviders(model, available)
	if err != nil {
		return nil, "routing_price_unavailable", http.StatusServiceUnavailable
	}
	selected := ordered[0]
	if decision != nil {
		if selected == decision.FromProvider {
			decision = nil
		} else {
			decision.ToProvider = selected
		}
	}
	return []routeStep{{provider: selected, model: model, dynamic: true, decision: decision}}, "", 0
}

func (h *handler) routingCandidates(model string, requireKnownScope bool, clientProtocol string, body []byte) []string {
	candidates := h.bareModelCandidates(model, requireKnownScope)
	filtered := candidates[:0]
	for _, name := range candidates {
		if clientProtocol == "anthropic" && adapter.HasAnthropicServerTools(body) && !h.supportsNativeAnthropicTools(name) {
			continue
		}
		if h.config.EffectiveRouting().SharedModelStrategy != "" && len(body) > 0 && h.clients[name] != nil {
			client := h.clients[name]
			upstreamModel := h.providers[name].UpstreamModel(model)
			if _, _, _, err := adapter.PrepareRequestWithWarnings(clientProtocol, client.Protocol, body, upstreamModel, client.OutputTokenDefault(upstreamModel)); err != nil {
				continue
			}
		}
		filtered = append(filtered, name)
	}
	return filtered
}

func (h *handler) routeErrorParameter(model, protocol string, body []byte) string {
	if protocol == "anthropic" && adapter.HasAnthropicServerTools(body) {
		return "tools"
	}
	candidates := h.bareModelCandidates(model, false)
	if model == "auto" {
		for _, entry := range h.config.AutoChain {
			candidates = append(candidates, entry.Provider)
		}
	}
	for _, ref := range candidates {
		if client := h.clients[ref]; client != nil {
			upstreamModel := h.providers[ref].UpstreamModel(model)
			if _, _, _, err := adapter.PrepareRequestWithWarnings(protocol, client.Protocol, body, upstreamModel, client.OutputTokenDefault(upstreamModel)); err != nil {
				if field := adapter.ValidationParameter(err); field != "" {
					return field
				}
			}
		}
	}
	return "model"
}

func (h *handler) routeRequestCompatible(provider, model, protocol string, body []byte) bool {
	client := h.clients[provider]
	if client == nil || protocol == "anthropic" && adapter.HasAnthropicServerTools(body) && !h.supportsNativeAnthropicTools(provider) {
		return false
	}
	if len(body) == 0 {
		return true
	}
	upstreamModel := h.providers[provider].UpstreamModel(model)
	_, _, _, err := adapter.PrepareRequestWithWarnings(protocol, client.Protocol, body, upstreamModel, client.OutputTokenDefault(upstreamModel))
	return err == nil
}

func (h *handler) supportsNativeAnthropicTools(provider string) bool {
	client := h.clients[provider]
	return client != nil && client.Protocol == "anthropic"
}

func (h *handler) orderSharedModelProviders(model string, names []string) ([]string, error) {
	ordered := append([]string(nil), names...)
	sort.Strings(ordered)
	switch h.config.EffectiveRouting().SharedModelStrategy {
	case "random":
		start := h.chooseRandomProvider(len(ordered))
		if start < 0 || start >= len(ordered) {
			start = 0
		}
		return append(ordered[start:], ordered[:start]...), nil
	case "price_priority":
		type pricedProvider struct {
			name string
			cost float64
		}
		priced := make([]pricedProvider, 0, len(ordered))
		currency := ""
		for _, name := range ordered {
			provider := h.providers[name]
			upstreamModel := provider.UpstreamModel(model)
			price, ok := provider.Prices[upstreamModel]
			if !ok {
				price, ok = adapter.BuiltInPrice(provider.BaseURL, upstreamModel, time.Now())
			}
			if !ok || price.Validate() != nil || price.InputCacheHit == nil || price.InputCacheMiss == nil || price.Output == nil {
				return nil, errRoutingPriceUnavailable
			}
			if currency == "" {
				currency = price.Currency
			} else if currency != price.Currency {
				return nil, errRoutingPriceUnavailable
			}
			cost := *price.InputCacheHit + *price.InputCacheMiss + *price.Output
			if math.IsInf(cost, 0) || math.IsNaN(cost) {
				return nil, errRoutingPriceUnavailable
			}
			priced = append(priced, pricedProvider{name: name, cost: cost})
		}
		sort.Slice(priced, func(i, j int) bool {
			if priced[i].cost == priced[j].cost {
				return priced[i].name < priced[j].name
			}
			return priced[i].cost < priced[j].cost
		})
		for index := range priced {
			ordered[index] = priced[index].name
		}
		return ordered, nil
	default:
		return nil, errSharedModelStrategyDisabled
	}
}

func (h *handler) chooseRandomProvider(size int) int {
	if h.randomIndex != nil {
		return h.randomIndex(size)
	}
	return rand.Intn(size)
}

func (h *handler) supportScopeAllows(providerName, model string) bool {
	provider, ok := h.providers[providerName]
	return ok && (len(provider.SupportedModels) == 0 || containsModel(provider.SupportedModels, model))
}

func (h *handler) providerRouteAvailable(providerName string) bool {
	if _, unavailable := h.unavailableProviders[providerName]; unavailable || h.clients[providerName] == nil {
		return false
	}
	pool := h.keyPools[providerName]
	return pool == nil || pool.CooldownWaitAt(time.Now()) == 0
}

var (
	errRoutingPriceUnavailable     = errors.New("routing price unavailable")
	errSharedModelStrategyDisabled = errors.New("shared-model routing disabled")
)
