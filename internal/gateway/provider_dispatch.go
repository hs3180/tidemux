package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
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
	admission, _ := r.Context().Value(requestSessionAdmissionKey{}).(*requestSessionAdmission)
	if admission != nil {
		if err := admission.acquireProvider(r.Context(), route.provider, provider.MaxActiveSessions); err != nil {
			if errors.Is(err, limiter.ErrActiveSessionLimit) {
				h.reject(w, r, clientProtocol, http.StatusTooManyRequests, "active_session_limit", route.provider)
			} else if errors.Is(context.Cause(r.Context()), adapter.ErrServerShuttingDown) {
				h.reject(w, r, clientProtocol, http.StatusServiceUnavailable, "server_shutting_down")
			} else {
				h.reject(w, r, clientProtocol, http.StatusBadRequest, "invalid_session_id")
			}
			return nil, "", nil, true
		}
	}
	keepProvider := false
	defer func() {
		if admission != nil && !keepProvider {
			admission.releaseProvider(route.provider)
		}
	}()

	pool := h.keyPools[route.provider]
	var candidates []providerKeyCandidate
	if pool != nil {
		var retryDelay time.Duration
		candidates, retryDelay = pool.Candidates()
		if len(candidates) == 0 {
			callErr := &adapter.CallError{Status: http.StatusServiceUnavailable, Code: "provider_keys_cooling_down", Cooldown: retryDelay, UpstreamNotAttempted: true}
			if route.sessionKey != nil || route.autoChainIndex != nil {
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
			if errors.Is(context.Cause(r.Context()), adapter.ErrServerShuttingDown) {
				h.reject(w, r, clientProtocol, http.StatusServiceUnavailable, "server_shutting_down")
				return nil, "", nil, true
			}
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
			candidates = []providerKeyCandidate{{key: providerClient.APIKey}}
		}
		keys := make([]string, len(candidates))
		for i, candidate := range candidates {
			keys[i] = candidate.key
		}
		observations := make([]rateLimitObservation, len(candidates))
		callbacks := adapter.KeyCandidateCallbacks{
			YieldUnavailable: route.dynamic || route.sessionKey != nil || route.autoChainIndex != nil,
			Completed: func(candidateIndex int, err error) {
				staleRate := false
				current := h.availability.withCurrent(h.routingEpoch, route.provider, h.providerGenerations[route.provider], func() {
					if err == nil && pool != nil && candidateIndex >= 0 && candidateIndex < len(candidates) {
						staleRate = !pool.rateSucceeded(candidates[candidateIndex].index, observations[candidateIndex])
					}
				})
				if result, ok := r.Context().Value(availabilityResultContextKey{}).(*availabilityResult); ok {
					result.finish(err, !current || staleRate)
				}
			},
			Ready: func(candidateIndex int) (bool, time.Duration) {
				if lease, ok := r.Context().Value(availabilityLeaseContextKey{}).(*availabilityLease); ok {
					if blocked := h.availability.dispatch(lease); blocked != nil {
						return false, blocked.Cooldown
					}
				}
				if pool == nil {
					return true, 0
				}
				if candidateIndex < 0 || candidateIndex >= len(candidates) {
					return false, 0
				}
				ready, delay, observation := pool.candidateStateAt(candidates[candidateIndex].index, time.Now())
				if ready {
					observations[candidateIndex] = observation
				}
				return ready, delay
			},
			Failed: func(candidateIndex int, callErr *adapter.CallError) {
				if candidateIndex < 0 || candidateIndex >= len(candidates) || callErr == nil || pool == nil {
					return
				}
				if callErr.UpstreamNotAttempted && callErr.Code == "upstream_transport_error" {
					return
				}
				h.availability.withCurrent(h.routingEpoch, route.provider, h.providerGenerations[route.provider], func() {
					now := time.Now()
					var serverUntil time.Time
					if callErr.RetryProvided {
						serverUntil = pool.holdProviderUntil(callErr.CooldownUntil)
					}
					if callErr.Category == adapter.ProviderErrorRateLimited {
						upstreamDelay := callErr.Cooldown
						if callErr.RetryProvided {
							upstreamDelay = max(callErr.CooldownUntil.Sub(now), 0)
						}
						delay, providerDelay, streak := pool.rateLimitedAt(candidates[candidateIndex].index, upstreamDelay, callErr.RetryProvided, now)
						callErr.Cooldown = delay
						callErr.RateLimitFailures = streak
						if providerDelay > 0 {
							callErr.AllKeysCooling = true
							callErr.Cooldown = providerDelay
						}
						callErr.CooldownUntil = now.Add(callErr.Cooldown)
						if serverUntil.After(now) {
							callErr.AllKeysCooling = true
							callErr.CooldownUntil = now.Add(providerDelay)
							if serverUntil.After(callErr.CooldownUntil) {
								callErr.CooldownUntil = serverUntil
							}
							callErr.Cooldown = callErr.CooldownUntil.Sub(now)
						}
						setCooldownFailure(callErr)
						return
					}
					if !serverUntil.IsZero() {
						callErr.AllKeysCooling, callErr.CooldownUntil = true, serverUntil
						callErr.Cooldown = serverUntil.Sub(now)
						setCooldownFailure(callErr)
						return
					}
					// Only credential-scoped failures should cool a key. Model, request,
					// balance, and generic upstream errors affect the route response or
					// route health, but must not make this key unavailable for unrelated
					// models on a future downstream request.
					if callErr.Category != adapter.ProviderErrorAuthentication && callErr.Category != adapter.ProviderErrorRateLimited && callErr.UpstreamStatus != http.StatusUnauthorized {
						return
					}
					pool.Cooldown(candidates[candidateIndex].index, callErr.Cooldown, adapter.SafeFailureClass(callErr))
				})
			},
		}
		if h.config.HealthDiagnosticsEnabled() && pool != nil {
			seen := make(map[int]bool, len(candidates))
			last, reason := -1, "upstream_error"
			callbacks.Attempt = func(candidateIndex int) func(adapter.HTTPAttemptResult) {
				index := candidates[candidateIndex].index
				finish := pool.beginAttempt(index, !seen[index], last, reason, time.Now())
				seen[index], last = true, index
				return func(result adapter.HTTPAttemptResult) {
					reason = result.FailureClass
					if finish != nil {
						finish(result)
					}
				}
			}
		}
		return providerClient.CallFromKeyCandidates(clientProtocol, r.Context(), routeBody, route.model, sink, options, keys, callbacks)
	}
	response, id, callErr := call()
	keepProvider = callErr == nil
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
				slog.String("requestId", id),
				slog.String("provider_ref", route.provider),
				slog.String("failure_code", "budget_settlement_unresolved"),
			)
		}
	}
	return response, id, callErr, false
}

func setCooldownFailure(callErr *adapter.CallError) {
	if callErr.Failure == nil {
		return
	}
	callErr.Failure = callErr.Failure.Normalize()
	callErr.Failure.CooldownScope = "key"
	if callErr.AllKeysCooling {
		callErr.Failure.CooldownScope = "provider"
	}
	callErr.Failure.CooldownSeconds = retryDeadlineSeconds(callErr.CooldownUntil, time.Now())
	callErr.Failure.CooldownUntilMS = callErr.CooldownUntil.UnixMilli()
	callErr.Failure.ProviderRateLimitStreak = callErr.RateLimitFailures
}
