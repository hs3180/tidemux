package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/observability"
)

const billingExhaustionCooldown = 5 * time.Minute

func (h *handler) callRouteCandidate(w http.ResponseWriter, r *http.Request, clientProtocol string, route routeStep, body []byte, options adapter.CallOptions, sink adapter.StreamSink) ([]byte, string, error, bool) {
	if route.autoChainIndex != nil {
		options.ResponseModel = route.model
	}
	provider, exists := h.providers[route.provider]
	if !exists {
		h.reject(w, r, clientProtocol, http.StatusNotFound, "provider_not_found", "model")
		return nil, "", nil, true
	}
	if _, unavailable := h.unavailableProviders[route.provider]; unavailable {
		h.reject(w, r, clientProtocol, http.StatusServiceUnavailable, "provider_unavailable", "model")
		return nil, "", nil, true
	}
	if !h.supportScopeAllows(route.provider, route.model) {
		h.reject(w, r, clientProtocol, http.StatusNotFound, "model_not_found")
		return nil, "", nil, true
	}
	providerClient := h.clients[route.provider]
	if providerClient == nil {
		h.reject(w, r, clientProtocol, http.StatusNotFound, "provider_not_found", "model")
		return nil, "", nil, true
	}
	routeBody, err := replaceRequestModel(body, route.model)
	if err != nil {
		h.reject(w, r, clientProtocol, http.StatusBadRequest, "invalid_request", "model")
		return nil, "", nil, true
	}

	pool := h.keyPools[route.provider]
	var candidates []providerKeyCandidate
	if pool != nil {
		var retryDelay time.Duration
		candidates, retryDelay = pool.Candidates()
		if len(candidates) == 0 {
			callErr := &adapter.CallError{Status: http.StatusServiceUnavailable, Code: "provider_keys_cooling_down", Cooldown: retryDelay, UpstreamNotAttempted: true}
			if route.sessionKey != nil {
				return nil, "", callErr, false
			}
			setRetryAfterHeader(w, callErr)
			h.reject(w, r, clientProtocol, callErr.Status, callErr.Code)
			return nil, "", nil, true
		}
	}

	var budget ledger.BudgetPolicy
	if provider.Budget != nil {
		budget = *provider.Budget
	}
	reservationID := ""
	if budget != (ledger.BudgetPolicy{}) {
		price, priced := provider.Prices[route.model]
		if !priced {
			price, priced = adapter.BuiltInPrice(provider.BaseURL, route.model, time.Now())
		}
		if !priced || price.Currency != budget.Currency {
			h.reject(w, r, clientProtocol, http.StatusServiceUnavailable, "budget_pricing_unconfigured")
			return nil, "", nil, true
		}
		if h.isBudgetBlocked(route.provider, budget.Currency) {
			h.reject(w, r, clientProtocol, http.StatusTooManyRequests, "budget_usage_unknown")
			return nil, "", nil, true
		}
		reservationID, err = newRequestID()
		if err != nil {
			h.reject(w, r, clientProtocol, http.StatusInternalServerError, "request_id_failed")
			return nil, "", nil, true
		}
		decision, err := h.ledger.CheckBudget(r.Context(), reservationID, route.provider, budget, r.Header.Get("X-TideMux-Budget-Confirm") == "1", time.Now())
		if err != nil {
			code := "budget_reservation_failed"
			if err.Error() == "budget_hard_limit" || err.Error() == "budget_confirmation_required" || err.Error() == "budget_usage_unknown" {
				code = err.Error()
			}
			h.reject(w, r, clientProtocol, http.StatusTooManyRequests, code)
			return nil, "", nil, true
		}
		if decision.Warning {
			w.Header().Set("X-TideMux-Budget-Warning", "1")
		}
	}

	call := func() ([]byte, string, error) {
		if pool == nil {
			return providerClient.CallFrom(clientProtocol, r.Context(), routeBody, route.model, sink, options)
		}
		keys := make([]string, len(candidates))
		for i, candidate := range candidates {
			keys[i] = candidate.key
		}
		callbacks := adapter.KeyCandidateCallbacks{
			Ready: func(candidateIndex int) (bool, time.Duration) {
				if candidateIndex < 0 || candidateIndex >= len(candidates) {
					return false, 0
				}
				return pool.CandidateReadyAt(candidates[candidateIndex].index, time.Now())
			},
			Failed: func(candidateIndex int, callErr *adapter.CallError) (bool, time.Duration) {
				if candidateIndex < 0 || candidateIndex >= len(candidates) || callErr == nil {
					return false, 0
				}
				if callErr.UpstreamNotAttempted && callErr.Code == "upstream_transport_error" {
					return pool.hasMultipleKeys() && candidateIndex+1 < len(candidates), 0
				}
				pool.Cooldown(candidates[candidateIndex].index, callErr.Cooldown)
				if !pool.hasMultipleKeys() {
					return false, 0
				}
				now := time.Now()
				hasReadyCandidate := false
				for index := candidateIndex + 1; index < len(candidates); index++ {
					ready, _ := pool.CandidateReadyAt(candidates[index].index, now)
					if ready {
						hasReadyCandidate = true
						continue
					}
					keys[index] = ""
				}
				if hasReadyCandidate {
					return true, 0
				}
				return false, pool.CooldownWaitAt(now)
			},
		}
		return providerClient.CallFromKeyCandidates(clientProtocol, r.Context(), routeBody, route.model, sink, options, keys, callbacks)
	}
	response, id, callErr := call()
	var classifiedErr *adapter.CallError
	if h.config.EffectiveRouting().BillingExhaustionFailover && errors.As(callErr, &classifiedErr) && classifiedErr.Category == adapter.ProviderErrorInsufficientBalance {
		pool.CooldownAll(billingExhaustionCooldown)
	}
	if reservationID != "" {
		var settleErr error
		var adapterErr *adapter.CallError
		if errors.As(callErr, &adapterErr) && adapterErr.UpstreamNotAttempted {
			settleErr = h.ledger.ReleaseBudgetReservation(context.Background(), reservationID)
		} else if errors.As(callErr, &adapterErr) && adapterErr.BudgetCost != nil {
			settleErr = h.ledger.RecordBudgetChargeWithCost(context.Background(), reservationID, id, route.provider, budget.Currency, time.Now(), *adapterErr.BudgetCost)
		} else {
			settleErr = h.ledger.RecordBudgetCharge(context.Background(), reservationID, id, route.provider, budget.Currency, time.Now())
		}
		if settleErr != nil {
			h.blockBudget(route.provider, budget.Currency)
			observability.LoggerOrDiscard(h.logger).Error("budget settlement unresolved",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "budget_settlement_failure"),
				slog.String("request_id", id),
				slog.String("provider_ref", route.provider),
				slog.String("failure_code", "budget_settlement_unresolved"),
			)
		}
	}
	return response, id, callErr, false
}
