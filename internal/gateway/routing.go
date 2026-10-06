package gateway

import (
	"errors"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

const maxBillingProviderAttempts = 4

type routeTrigger uint8

const (
	routeInitial routeTrigger = iota
	routeBillingProvider
)

type routeStep struct {
	provider       string
	model          string
	trigger        routeTrigger
	sessionKey     *sharedModelSessionKey
	autoChainIndex *int
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
			routes := []routeStep{{provider: ref, model: model, trigger: routeInitial}}
			return h.appendBillingRoutes(routes, ref, model, clientProtocol, body), "", 0
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
		route, ok := h.autoChain.selectRoute(sessionKey)
		if !ok {
			return nil, "auto_chain_exhausted", http.StatusServiceUnavailable
		}
		if requiresNativeTools && !h.supportsNativeAnthropicTools(route.provider) {
			return nil, "unsupported_request_feature", http.StatusBadRequest
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
		candidates = h.reusableRandomCandidates(candidates)
		selected, ok := h.sharedAffinity.selectProviderForView(*sessionKey, model, h.routingEpoch, candidates, h.providerRouteAvailable, h.chooseRandomProvider, func(provider string) uint64 { return h.providerGenerations[provider] })
		if !ok {
			return nil, "provider_keys_cooling_down", http.StatusServiceUnavailable
		}
		// Session affinity never adds a provider retry to a dispatched request,
		// including when the independent billing-failover option is enabled.
		return []routeStep{{provider: selected, model: model, trigger: routeInitial, sessionKey: sessionKey}}, "", 0
	}
	if len(candidates) == 1 {
		return h.appendBillingRoutes([]routeStep{{provider: candidates[0], model: model, trigger: routeInitial}}, candidates[0], model, clientProtocol, body), "", 0
	}
	if h.config.EffectiveRouting().SharedModelStrategy == "" {
		return nil, "model_ambiguous", http.StatusBadRequest
	}
	available := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if h.providerRouteAvailable(name) {
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
	routes := []routeStep{{provider: selected, model: model, trigger: routeInitial}}
	return h.appendBillingRoutes(routes, selected, model, clientProtocol, body), "", 0
}

func (h *handler) appendBillingRoutes(routes []routeStep, selectedProvider, model, clientProtocol string, body []byte) []routeStep {
	if !h.config.EffectiveRouting().BillingExhaustionFailover {
		return routes
	}
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		if route.model == model {
			seen[route.provider] = struct{}{}
		}
	}
	candidates := h.bareModelCandidates(model, true)
	added := 0
	for _, name := range candidates {
		if _, exists := seen[name]; exists || name == selectedProvider || !h.providerRouteAvailable(name) {
			continue
		}
		if clientProtocol != "" && h.clients[name].Protocol != clientProtocol {
			continue
		}
		routes = append(routes, routeStep{provider: name, model: model, trigger: routeBillingProvider})
		seen[name] = struct{}{}
		added++
		if added == maxBillingProviderAttempts {
			break
		}
	}
	return routes
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
			if _, _, _, err := adapter.PrepareRequestWithWarnings(clientProtocol, client.Protocol, body, model, client.MaxOutputTokens); err != nil {
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
	for _, ref := range h.bareModelCandidates(model, false) {
		if client := h.clients[ref]; client != nil {
			if _, _, _, err := adapter.PrepareRequestWithWarnings(protocol, client.Protocol, body, model, client.MaxOutputTokens); err != nil {
				if field := adapter.ValidationParameter(err); field != "" {
					return field
				}
			}
		}
	}
	return "model"
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
			price, ok := provider.Prices[model]
			if !ok {
				price, ok = adapter.BuiltInPrice(provider.BaseURL, model, time.Now())
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

func routeCanAdvance(config RoutingConfig, next routeStep, callErr *adapter.CallError, ctxErr error, delivered bool) bool {
	if callErr == nil || !callErr.FailoverSafe || ctxErr != nil || delivered {
		return false
	}
	switch next.trigger {
	case routeBillingProvider:
		return config.BillingExhaustionFailover && callErr.Category == adapter.ProviderErrorInsufficientBalance
	default:
		return false
	}
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
