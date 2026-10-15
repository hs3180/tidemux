package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

type recoveryRequestKey struct{}
type recoveryRequest struct {
	model    string
	protocol string
	body     []byte // retained only for this admitted request, never logged
}

func withRecoveryRequest(r *http.Request, model, protocol string, body []byte) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), recoveryRequestKey{}, recoveryRequest{model, protocol, body}))
}

type retryAdvice struct {
	Strategy                       string    `json:"strategy"`
	AfterSeconds                   *int64    `json:"after_seconds,omitempty"`
	EligibleAfterSeconds           *int64    `json:"eligible_after_seconds,omitempty"`
	ProviderRetryAt                time.Time `json:"provider_retry_at,omitzero"`
	InitialDelaySeconds            int       `json:"initial_delay_seconds,omitempty"`
	MaxDelaySeconds                int       `json:"max_delay_seconds,omitempty"`
	MaxAttempts                    int       `json:"max_attempts"`
	NewRequestRequired             bool      `json:"new_request_required"`
	ReselectProvider               bool      `json:"reselect_provider"`
	Alternatives                   []string  `json:"alternatives"`
	AlternativesRequireModelChange bool      `json:"alternatives_require_model_change"`
	AlternativesScope              string    `json:"alternatives_scope"`
	WaitSource                     string    `json:"wait_source"`
	AlternativeCount               int       `json:"alternative_count"`
	hasCooldown                    bool
}

type routeWait struct {
	blocked, unknown bool
	retryAt          time.Time
	temporary        bool
}

func (w *routeWait) add(until time.Time, unknown bool) {
	w.blocked = true
	w.unknown = w.unknown || unknown
	if until.After(w.retryAt) {
		w.retryAt = until
	}
}

// Called with availability.mu held when that state exists. Key and route
// cooldowns are independent gates: a route waits for the longest gate, while
// a shared request can use the earliest eligible compatible route.
func (h *handler) routeWaitLocked(route routeStep, now time.Time) routeWait {
	var wait routeWait
	if h.clients[route.provider] == nil {
		wait.add(time.Time{}, true)
		return wait
	}
	if _, unavailable := h.unavailableProviders[route.provider]; unavailable {
		wait.add(time.Time{}, true)
		return wait
	}
	if s := h.availability; s != nil {
		for _, model := range []string{"", route.model} {
			entry := s.entries[availabilityTarget{route.provider, model, h.providerGenerations[route.provider]}]
			if entry != nil && !entry.eligible(now) {
				wait.add(entry.retryAt, entry.probing)
			}
		}
	}
	if pool := h.keyPools[route.provider].diagnosticStatus(now); pool != nil {
		if pool.RecoveryInProgress {
			wait.add(time.Time{}, true) // probe completion has no known deadline
		}
		if pool.RateLimitRetryAt.After(now) {
			wait.add(pool.RateLimitRetryAt, false)
		}
		if pool.ProviderRetryAt.After(now) {
			wait.add(pool.ProviderRetryAt, false)
		}
		if pool.Capacity > 0 && pool.Eligible == 0 {
			var earliest time.Time
			for _, key := range pool.Keys {
				if earliest.IsZero() || key.CooldownUntil.Before(earliest) {
					earliest = key.CooldownUntil
				}
			}
			wait.add(earliest, earliest.IsZero())
		}
	}
	wait.temporary = wait.blocked
	return wait
}

func (h *handler) recoveryRoutes(request recoveryRequest) (routes, manual []routeStep, explicit bool) {
	if ref, model, qualified := parseQualifiedModelID(request.model); qualified {
		if _, exists := h.providers[ref]; exists {
			routes = []routeStep{{provider: ref, model: model}}
			for _, candidate := range h.routingCandidates(model, false, request.protocol, request.body) {
				if candidate != ref {
					manual = append(manual, routeStep{provider: candidate, model: model})
				}
			}
			return routes, manual, true
		}
	}
	if request.model == "auto" {
		for _, entry := range h.config.AutoChain {
			if h.supportScopeAllows(entry.Provider, entry.Model) && h.routeRequestCompatible(entry.Provider, entry.Model, request.protocol, request.body) {
				routes = append(routes, routeStep{provider: entry.Provider, model: entry.Model})
			}
		}
		return routes, nil, false
	}
	for _, candidate := range h.routingCandidates(request.model, false, request.protocol, request.body) {
		routes = append(routes, routeStep{provider: candidate, model: request.model})
	}
	return routes, nil, false
}

func (h *handler) retryAdviceFor(r *http.Request) *retryAdvice {
	// Recovery starts a new request and therefore uses the current routing view,
	// while the original request retains its admitted endpoint and settlement.
	if h.reload != nil {
		if current := h.reload.active.Load(); current != nil && current != h {
			return current.retryAdviceFor(r)
		}
	}
	request, ok := r.Context().Value(recoveryRequestKey{}).(recoveryRequest)
	advice := &retryAdvice{Strategy: "exponential_backoff_with_jitter", InitialDelaySeconds: 1, MaxDelaySeconds: 30,
		MaxAttempts: 3, NewRequestRequired: true, Alternatives: []string{}, AlternativesScope: "model_protocol_health", WaitSource: "unknown"}
	if !ok {
		return advice
	}
	routes, manual, explicit := h.recoveryRoutes(request)
	details, _ := r.Context().Value(requestLogContextKey{}).(requestLogContext)
	advice.ReselectProvider = !explicit && (request.model == "auto" || len(routes) > 1 && h.config.EffectiveRouting().SharedModelStrategy != "")
	advice.AlternativesRequireModelChange = explicit
	now := time.Now()
	if s := h.availability; s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.epoch != h.routingEpoch {
			return advice // a new request will use the newly published view
		}
		now = s.now()
	}
	var earliest time.Time
	unknown := len(routes) == 0
	ready := false
	for _, route := range routes {
		wait := h.routeWaitLocked(route, now)
		advice.hasCooldown = advice.hasCooldown || wait.temporary
		if !wait.blocked {
			ready = true
			if route.provider != details.providerRef {
				advice.Alternatives = append(advice.Alternatives, route.provider+"/"+route.model)
			}
		} else if wait.unknown {
			unknown = true
		} else if earliest.IsZero() || wait.retryAt.Before(earliest) {
			earliest = wait.retryAt
		}
	}
	for _, route := range manual {
		if !h.routeWaitLocked(route, now).blocked {
			advice.Alternatives = append(advice.Alternatives, route.provider+"/"+route.model)
		}
	}
	sort.Strings(advice.Alternatives)
	advice.AlternativeCount = len(advice.Alternatives)
	if len(advice.Alternatives) > 16 {
		advice.Alternatives = advice.Alternatives[:16]
	}
	if ready {
		zero := int64(0)
		advice.EligibleAfterSeconds = &zero
		advice.Strategy, advice.AfterSeconds = "new_request", &zero
		advice.WaitSource = "eligible_route"
	} else if !unknown && earliest.After(now) {
		actual := retryDeadlineSeconds(earliest, now)
		advice.EligibleAfterSeconds = &actual
		seconds := min(actual, 30)
		advice.Strategy, advice.AfterSeconds = "retry_after", &seconds
		advice.WaitSource = "gateway_cooldown"
	}
	if !ready && len(advice.Alternatives) > 0 && advice.AfterSeconds != nil {
		short := int64(2)
		advice.AfterSeconds = &short
	}
	if advice.AfterSeconds != nil {
		advice.InitialDelaySeconds, advice.MaxDelaySeconds = 0, 0
	}
	return advice
}

// Absolute deadlines keep accurate summaries even beyond time.Duration's range.
func retryDeadlineSeconds(until, now time.Time) int64 {
	if !until.After(now) {
		return 0
	}
	seconds := until.Unix() - now.Unix()
	if until.Nanosecond() > now.Nanosecond() {
		seconds++
	}
	return seconds
}

// Avoid overflowing a duration while rounding very long provider waits.
func retryDelaySeconds(delay time.Duration) int64 {
	if delay <= 0 {
		return 0
	}
	seconds := int64(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	return seconds
}

func retryMessage(advice *retryAdvice) string {
	message := "Retry as a new request"
	if advice.AfterSeconds == nil {
		message = "Recovery time is unknown. Retry as a new request with bounded exponential backoff and jitter (up to 30 seconds)"
	} else if *advice.AfterSeconds > 0 {
		message = fmt.Sprintf("Retry after %d seconds as a new request", *advice.AfterSeconds)
	}
	if advice.ReselectProvider {
		message += "; routing will reselect an eligible provider"
	} else if advice.AlternativesRequireModelChange && len(advice.Alternatives) > 0 {
		message += "; another listed provider requires an explicit model change"
	}
	if advice.EligibleAfterSeconds != nil && advice.AfterSeconds != nil && *advice.EligibleAfterSeconds > *advice.AfterSeconds {
		message += fmt.Sprintf("; the selected routes remain paused for %d seconds, and this shorter retry only rechecks routing", *advice.EligibleAfterSeconds)
	}
	return message + ". Limit retries to 3 attempts; request features, capacity and budgets are rechecked."
}

func addRequestID(payload map[string]any, id string) {
	if id == "" {
		return
	}
	payload["request_id"] = id
	if detail, ok := payload["error"].(map[string]any); ok {
		detail["request_id"] = id
		if _, retry := detail["retry"]; retry {
			detail["message"] = fmt.Sprintf("%s [request_id: %s]", detail["message"], id)
		}
	}
}

func setRequestIDHeaders(w http.ResponseWriter, id string) {
	w.Header().Set("X-TideMux-Request-ID", id)
	w.Header().Set("request-id", id)
	w.Header().Set("x-request-id", id)
}

func addRetryAdvice(detail map[string]any, advice *retryAdvice) {
	detail["retry"] = advice
	detail["message"] = fmt.Sprintf("%s. %s", detail["message"], retryMessage(advice))
}

func applyRetryHeader(w http.ResponseWriter, advice *retryAdvice) {
	if tracked, ok := w.(*trackedResponseWriter); ok && tracked.wroteHeader {
		return
	}
	if advice.AfterSeconds != nil && *advice.AfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(*advice.AfterSeconds, 10))
	} else {
		w.Header().Del("Retry-After")
	}
}

func (h *handler) upstreamErrorPayload(w http.ResponseWriter, r *http.Request, callErr *adapter.CallError, protocol string) map[string]any {
	payload := mappedUpstreamErrorPayload(callErr, protocol)
	localCooldown := isAvailabilityError(callErr.Code)
	if callErr.Category == "" {
		status := callErr.Status
		if localCooldown {
			status = http.StatusTooManyRequests
		}
		payload = h.errorPayload(status, callErr.Code, protocol, callErr.Failure, callErr.Param)
	}
	mayRetry := localCooldown || callErr.Category == adapter.ProviderErrorRateLimited || callErr.Category == adapter.ProviderErrorTemporarilyUnavailable
	if f := callErr.Failure; f != nil {
		mayRetry = mayRetry || f.Retryability == "retryable" || f.Source == "stream_error" && f.Retryability == "unknown"
	}
	advice := h.retryAdviceFor(r)
	if callErr.Category == adapter.ProviderErrorModelNotFound || callErr.Category == adapter.ProviderErrorInsufficientBalance {
		mayRetry = advice.ReselectProvider && len(advice.Alternatives) > 0
	}
	if mayRetry {
		if callErr.RetryProvided {
			advice.ProviderRetryAt = callErr.CooldownUntil
			if advice.AfterSeconds != nil && *advice.AfterSeconds == 0 {
				short := int64(2)
				advice.AfterSeconds = &short
			}
			if advice.AfterSeconds == nil && advice.WaitSource == "unknown" {
				seconds := min(max(retryDelaySeconds(callErr.Cooldown), 2), 30)
				advice.AfterSeconds, advice.Strategy = &seconds, "retry_after"
				advice.WaitSource = "provider_cooldown"
				advice.InitialDelaySeconds, advice.MaxDelaySeconds = 0, 0
			}
		}
		applyRetryHeader(w, advice)
		addRetryAdvice(payload["error"].(map[string]any), advice)
	}
	return payload
}
