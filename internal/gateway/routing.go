package gateway

import (
	"errors"
	"math/rand"
	"net/http"
	"sort"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

const (
	maxBillingProviderAttempts = 4
	maxAutoRouteAttempts       = maxAutoModelChain * (maxBillingProviderAttempts + 1)
)

type routeTrigger uint8

const (
	routeInitial routeTrigger = iota
	routeAutoModel
	routeBillingProvider
)

type routeStep struct {
	provider string
	model    string
	trigger  routeTrigger
}

func (h *handler) resolveRoutes(modelID, clientProtocol string, body []byte) ([]routeStep, string, int) {
	requiresNativeTools := clientProtocol == "anthropic" && adapter.HasAnthropicServerTools(body)
	if ref, model, qualified := parseQualifiedModelID(modelID); qualified {
		if provider, exists := h.providers[ref]; exists {
			if model == "auto" {
				if _, unavailable := h.unavailableProviders[ref]; unavailable {
					return nil, "provider_unavailable", http.StatusServiceUnavailable
				}
				if requiresNativeTools && !h.supportsNativeAnthropicTools(ref) {
					return nil, "unsupported_request_feature", http.StatusBadRequest
				}
				if !h.providerRouteAvailable(ref) {
					return nil, "provider_keys_cooling_down", http.StatusServiceUnavailable
				}
				if len(provider.AutoModelChain) == 0 {
					return nil, "model_not_found", http.StatusNotFound
				}
				return h.autoModelRoutes(ref, provider.AutoModelChain, clientProtocol, body), "", 0
			}
			if requiresNativeTools && !h.supportsNativeAnthropicTools(ref) {
				return nil, "unsupported_request_feature", http.StatusBadRequest
			}
			routes := []routeStep{{provider: ref, model: model, trigger: routeInitial}}
			return h.appendBillingRoutes(routes, ref, model, clientProtocol, body), "", 0
		}
		allCandidates := h.bareModelCandidates(modelID, true)
		candidates := h.routingCandidates(modelID, true, clientProtocol, body)
		if len(candidates) == 0 && len(allCandidates) > 0 && requiresNativeTools {
			return nil, "unsupported_request_feature", http.StatusBadRequest
		}
		return h.routesForBareModel(modelID, candidates, true, clientProtocol, body)
	}
	if modelID == "auto" {
		var refs []string
		var configured, compatible bool
		for name, provider := range h.providers {
			if len(provider.AutoModelChain) == 0 {
				continue
			}
			configured = true
			if _, unavailable := h.unavailableProviders[name]; unavailable {
				continue
			}
			if requiresNativeTools && !h.supportsNativeAnthropicTools(name) {
				continue
			}
			compatible = true
			refs = append(refs, name)
		}
		sort.Strings(refs)
		if len(refs) == 0 {
			if configured && requiresNativeTools && !compatible {
				return nil, "unsupported_request_feature", http.StatusBadRequest
			}
			if configured {
				return nil, "provider_unavailable", http.StatusServiceUnavailable
			}
			return nil, "model_not_found", http.StatusNotFound
		}
		switch len(refs) {
		case 1:
			if !h.providerRouteAvailable(refs[0]) {
				return nil, "provider_keys_cooling_down", http.StatusServiceUnavailable
			}
			return h.autoModelRoutes(refs[0], h.providers[refs[0]].AutoModelChain, clientProtocol, body), "", 0
		default:
			return nil, "model_ambiguous", http.StatusBadRequest
		}
	}

	allCandidates := h.bareModelCandidates(modelID, false)
	candidates := h.routingCandidates(modelID, false, clientProtocol, body)
	if len(candidates) == 0 && len(allCandidates) > 0 && requiresNativeTools {
		return nil, "unsupported_request_feature", http.StatusBadRequest
	}
	return h.routesForBareModel(modelID, candidates, false, clientProtocol, body)
}

func (h *handler) routesForBareModel(model string, candidates []string, qualifiedUnknown bool, clientProtocol string, body []byte) ([]routeStep, string, int) {
	if len(candidates) == 0 {
		if qualifiedUnknown {
			return nil, "provider_not_found", http.StatusNotFound
		}
		return nil, "model_not_found", http.StatusNotFound
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
	ordered, err := h.orderSharedModelProviders(model, available)
	if err != nil {
		return nil, "routing_price_unavailable", http.StatusServiceUnavailable
	}
	selected := ordered[0]
	routes := []routeStep{{provider: selected, model: model, trigger: routeInitial}}
	return h.appendBillingRoutes(routes, selected, model, clientProtocol, body), "", 0
}

func (h *handler) autoModelRoutes(ref string, chain []string, clientProtocol string, body []byte) []routeStep {
	routes := make([]routeStep, 0, min(maxAutoRouteAttempts, len(chain)*(maxBillingProviderAttempts+1)))
	for index, model := range chain {
		if len(routes) >= maxAutoRouteAttempts {
			break
		}
		trigger := routeAutoModel
		if len(routes) == 0 && index == 0 {
			trigger = routeInitial
		}
		routes = append(routes, routeStep{provider: ref, model: model, trigger: trigger})
		if h.config.EffectiveRouting().BillingExhaustionFailover {
			routes = h.appendBillingRoutes(routes, ref, model, clientProtocol, body)
		}
	}
	return routes
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
	candidates := h.bareModelCandidates(model, false)
	added := 0
	for _, name := range candidates {
		if _, exists := seen[name]; exists || name == selectedProvider || !h.providerRouteAvailable(name) {
			continue
		}
		if clientProtocol != "" && clientProtocol == "anthropic" && adapter.HasAnthropicServerTools(body) && !h.supportsNativeAnthropicTools(name) {
			continue
		}
		routes = append(routes, routeStep{provider: name, model: model, trigger: routeBillingProvider})
		seen[name] = struct{}{}
		added++
		if added == maxBillingProviderAttempts || len(routes) == maxAutoRouteAttempts {
			break
		}
	}
	return routes
}

func (h *handler) routingCandidates(model string, requireKnownScope bool, clientProtocol string, body []byte) []string {
	candidates := h.bareModelCandidates(model, requireKnownScope)
	if clientProtocol != "anthropic" || !adapter.HasAnthropicServerTools(body) {
		return candidates
	}
	filtered := candidates[:0]
	for _, name := range candidates {
		if h.supportsNativeAnthropicTools(name) {
			filtered = append(filtered, name)
		}
	}
	return filtered
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
		choose := h.randomIndex
		if choose == nil {
			choose = rand.Intn
		}
		start := choose(len(ordered))
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

func routeCanAdvance(config RoutingConfig, next routeStep, callErr *adapter.CallError, ctxErr error, delivered bool) bool {
	if callErr == nil || !callErr.FailoverSafe || ctxErr != nil || delivered {
		return false
	}
	switch next.trigger {
	case routeAutoModel:
		return callErr.UpstreamNotAttempted && callErr.Code == "upstream_transport_error" || callErr.Category == adapter.ProviderErrorModelNotFound || callErr.Category == adapter.ProviderErrorTemporarilyUnavailable || callErr.Category == adapter.ProviderErrorInsufficientBalance
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
