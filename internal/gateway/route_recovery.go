package gateway

import (
	"errors"
	"net/http"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

const routeRecoveryCooldown = 30 * time.Second

// A confirmed failure remains remembered until a later request succeeds.
// Only one request may test a failed route after its cooldown expires.
type routeFailure struct {
	retryAt time.Time
	probing bool
}

func (f *routeFailure) eligible(now time.Time) bool {
	return f == nil || !f.probing && !now.Before(f.retryAt)
}

func recoveryCooldown(callErr *adapter.CallError) time.Duration {
	delay := routeRecoveryCooldown
	if callErr != nil {
		if callErr.Category == adapter.ProviderErrorInsufficientBalance {
			delay = billingExhaustionCooldown
		}
		if callErr.Cooldown > delay {
			delay = callErr.Cooldown
		}
	}
	return delay
}

// Claim at dispatch, after route selection and global session admission. A
// rejected or cancelled local request releases the claim without health data.
// Completion and failure recording happen before another probe can start.
func (h *handler) callRouteWithRecovery(w http.ResponseWriter, r *http.Request, protocol string, route routeStep, body []byte, options adapter.CallOptions, sink adapter.StreamSink, delivered *bool) (response []byte, id string, callErr error, handled bool) {
	var probe *routeFailure
	var allowed bool
	if route.autoChainIndex != nil {
		probe, allowed = h.autoChain.beginRecovery(*route.autoChainIndex)
	} else if route.sessionKey != nil {
		probe, allowed = h.sharedAffinity.beginRecovery(h.routingEpoch, sharedModelRoute{provider: route.provider, model: route.model, generation: h.providerGenerations[route.provider]})
	} else {
		allowed = true
	}
	if !allowed {
		return nil, "", &adapter.CallError{Status: http.StatusServiceUnavailable, Code: "provider_keys_cooling_down", UpstreamNotAttempted: true}, false
	}
	completed := false
	defer func() {
		if !completed {
			handled = true
		}
		var classified *adapter.CallError
		_ = errors.As(callErr, &classified)
		output := delivered != nil && *delivered
		if route.autoChainIndex != nil {
			h.autoChain.recordFailure(*route.autoChainIndex, len(h.config.AutoChain), classified, r.Context().Err(), output)
			h.autoChain.finishRecovery(*route.autoChainIndex, probe, handled, callErr, r.Context().Err())
		} else if route.sessionKey != nil {
			generation := h.providerGenerations[route.provider]
			h.sharedAffinity.recordFailure(h.routingEpoch, route.provider, route.model, generation, classified, r.Context().Err(), output)
			h.sharedAffinity.finishRecovery(h.routingEpoch, sharedModelRoute{provider: route.provider, model: route.model, generation: generation}, probe, handled, callErr, r.Context().Err())
		}
	}()
	response, id, callErr, handled = h.callRouteCandidate(w, r, protocol, route, body, options, sink)
	completed = true
	return
}

// Returns success only for a completed upstream call. Local rejection,
// cancellation and proven pre-dispatch errors do not establish recovery.
func completeRecovery(probe *routeFailure, now time.Time, handled bool, callErr, ctxErr error) bool {
	probe.probing = false
	if handled || ctxErr != nil {
		return false
	}
	if callErr == nil {
		return true
	}
	var upstream *adapter.CallError
	if !errors.As(callErr, &upstream) || !upstream.UpstreamNotAttempted {
		probe.retryAt = now.Add(recoveryCooldown(upstream))
	}
	return false
}
