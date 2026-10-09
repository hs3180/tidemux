package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
	"github.com/hs3180/tidemux/internal/observability"
)

type handler struct {
	config Config
	ledger *ledger.Ledger
	*handlerRuntime
	reload               *configReload
	sessions             *limiter.SessionLimiter
	providers            map[string]Provider
	clients              map[string]*adapter.Client
	keyPools             map[string]*providerKeyPool
	models               map[string][]string
	modelsKnown          map[string]bool
	unavailableProviders map[string]error
	logger               *slog.Logger
	randomIndex          func(int) int
	autoChain            *autoChainState
	providerGenerations  map[string]uint64
	reusedConnections    map[string]bool
	routingEpoch         uint64
	preparedRouting      *routingReload
}

// Mutable admission/accounting state is shared by all immutable config views.
type handlerRuntime struct {
	usageLog               *observability.UsageLog
	providerSessionMu      sync.Mutex
	providerSessions       map[string]*limiter.SessionLimiter
	budgetMu               sync.RWMutex
	budgetBlocked          map[string]struct{}
	sharedAffinity         sharedModelAffinity
	activeMu               sync.Mutex
	draining               bool
	activeCalls            map[uint64]context.CancelCauseFunc
	nextCallID             uint64
	gate                   *limiter.ConcurrencyGate
	cache                  *adapter.PromptCache
	nextProviderGeneration uint64 // config preparation is serialized by the watcher
}

type requestLogContextKey struct{}

type requestLogContext struct {
	started     time.Time
	providerRef string
	model       string
}

func withRequestLogDetails(r *http.Request, providerRef, model string) *http.Request {
	details, _ := r.Context().Value(requestLogContextKey{}).(requestLogContext)
	if details.started.IsZero() {
		details.started = time.Now()
	}
	details.providerRef = providerRef
	details.model = model
	return r.WithContext(context.WithValue(r.Context(), requestLogContextKey{}, details))
}

func (h *handler) modelsForProvider(providerName string) ([]string, bool) {
	if provider, ok := h.providers[providerName]; ok && len(provider.SupportedModels) > 0 {
		return provider.SupportedModels, true
	}
	return h.models[providerName], h.modelsKnown[providerName]
}

func budgetBlockKey(provider, currency string) string { return provider + "\x00" + currency }

func (h *handler) blockBudget(provider, currency string) {
	h.budgetMu.Lock()
	defer h.budgetMu.Unlock()
	if h.budgetBlocked == nil {
		h.budgetBlocked = map[string]struct{}{}
	}
	h.budgetBlocked[budgetBlockKey(provider, currency)] = struct{}{}
}

func (h *handler) isBudgetBlocked(provider, currency string) bool {
	h.budgetMu.RLock()
	defer h.budgetMu.RUnlock()
	_, blocked := h.budgetBlocked[budgetBlockKey(provider, currency)]
	return blocked
}

func parseQualifiedModelID(value string) (providerName, model string, ok bool) {
	separator := strings.IndexByte(value, '/')
	if separator <= 0 || separator == len(value)-1 {
		return "", "", false
	}
	providerName, model = value[:separator], value[separator+1:]
	if strings.TrimSpace(providerName) != providerName || strings.TrimSpace(model) != model || strings.ContainsAny(value, "\r\n\x00") {
		return "", "", false
	}
	return providerName, model, true
}

func qualifiedModelID(providerName, model string) string {
	return providerName + "/" + model
}

func (h *handler) allQualifiedModels() []string {
	var result []string
	for providerName := range h.providers {
		if _, unavailable := h.unavailableProviders[providerName]; unavailable {
			continue
		}
		models, _ := h.modelsForProvider(providerName)
		for _, model := range models {
			result = append(result, qualifiedModelID(providerName, model))
		}
	}
	sort.Strings(result)
	return result
}

func (h *handler) resolveRequestModel(modelID string) (providerName, model, code string, status int) {
	if ref, suffix, qualified := parseQualifiedModelID(modelID); qualified {
		if _, exists := h.providers[ref]; exists {
			return ref, suffix, "", 0
		}
		// An upstream model may itself contain slashes. Without a matching
		// TideMux provider prefix, accept it only when a configured scope or a
		// complete discovered catalog identifies the full model ID.
		candidates := h.bareModelCandidates(modelID, true)
		if len(candidates) == 1 {
			return candidates[0], modelID, "", 0
		}
		if len(candidates) > 1 {
			return "", "", "model_ambiguous", http.StatusBadRequest
		}
		return "", "", "provider_not_found", http.StatusNotFound
	}

	candidates := h.bareModelCandidates(modelID, false)
	switch len(candidates) {
	case 0:
		return "", "", "model_not_found", http.StatusNotFound
	case 1:
		return candidates[0], modelID, "", 0
	default:
		return "", "", "model_ambiguous", http.StatusBadRequest
	}
}

func (h *handler) bareModelCandidates(model string, requireKnownScope bool) []string {
	var candidates []string
	for name, provider := range h.providers {
		if _, unavailable := h.unavailableProviders[name]; unavailable {
			continue
		}
		if len(provider.SupportedModels) > 0 {
			if !containsModel(provider.SupportedModels, model) {
				continue
			}
		} else if requireKnownScope {
			models, known := h.modelsForProvider(name)
			if !known || !containsModel(models, model) {
				continue
			}
		}
		candidates = append(candidates, name)
	}
	sort.Strings(candidates)
	return candidates
}

func replaceRequestModel(body []byte, model string) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, errors.New("invalid_request")
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	request["model"] = encoded
	return json.Marshal(request)
}

func containsModel(models []string, model string) bool {
	for _, candidate := range models {
		if candidate == model {
			return true
		}
	}
	return false
}

func (h *handler) fail(w http.ResponseWriter, status int, code, protocol string, param ...string) {
	kind := "api_error"
	switch status {
	case 400, 413:
		kind = "invalid_request_error"
	case 401:
		kind = "authentication_error"
	case 404:
		kind = "not_found_error"
	case 429:
		kind = "rate_limit_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	field := ""
	if len(param) > 0 {
		field = param[0]
	}
	message := code
	if code == "unsupported_request_feature" && field != "" {
		message = "unsupported_request_feature: " + field + " cannot be represented by the configured upstream protocol"
	}
	detail := map[string]any{"type": kind, "message": message, "code": code}
	addToolHistoryRecovery(detail, code, field)
	switch code {
	case "request_too_large":
		limit := h.config.Limits.Effective().RequestBytes
		detail["message"] = fmt.Sprintf("request_too_large: body exceeds gateway limit of %d bytes; configure limits.request_bytes to change it", limit)
		detail["limit_bytes"] = limit
		if protocol == "anthropic" {
			detail["type"] = "request_too_large"
		}
	case "active_session_limit":
		limit, scope := h.config.MaxActiveSessions, "gateway"
		if provider, ok := h.providers[field]; ok && field != "" {
			limit, scope = provider.MaxActiveSessions, "provider"
			detail["provider_ref"] = field
			field = ""
		}
		detail["message"] = fmt.Sprintf("%s capacity of %d active sessions is full; capacity release time is unknown. Retry with bounded exponential backoff and jitter (up to 30 seconds).", scope, limit)
		detail["scope"], detail["limit"] = scope, limit
		detail["retry"] = map[string]any{"strategy": "exponential_backoff_with_jitter", "initial_delay_seconds": 1, "max_delay_seconds": 30}
	}
	if protocol == "anthropic" {
		if field != "" {
			detail["param"] = field
		}
		json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": detail})
	} else {
		detail["param"] = nil
		if field != "" {
			detail["param"] = field
		}
		json.NewEncoder(w).Encode(map[string]any{"error": detail})
	}
}

func mappedUpstreamErrorPayload(callErr *adapter.CallError, protocol string) map[string]any {
	_, code, message, kind := callErr.Category.ClientError(protocol)
	detail := map[string]any{"type": kind, "message": message, "code": code}
	if callErr.ProviderCode != "" {
		detail["provider_code"] = callErr.ProviderCode
	}
	if callErr.Param != "" {
		detail["param"] = callErr.Param
	}
	payload := map[string]any{"error": detail}
	if protocol == "anthropic" {
		payload["type"] = "error"
	}
	return payload
}

func (h *handler) failUpstream(w http.ResponseWriter, callErr *adapter.CallError, protocol string) {
	if callErr.Category == "" {
		h.fail(w, callErr.Status, callErr.Code, protocol, callErr.Param)
		return
	}
	status := callErr.Status
	if status < 400 || status > 599 {
		status, _, _, _ = callErr.Category.ClientError(protocol)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(mappedUpstreamErrorPayload(callErr, protocol))
}

func setRetryAfterHeader(w http.ResponseWriter, callErr *adapter.CallError) {
	if callErr.RetryProvided {
		w.Header().Set("Retry-After", strconv.FormatInt(callErr.RetryAfterSecs, 10))
		return
	}
	if callErr.RetryAfterSecs > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(callErr.RetryAfterSecs, 10))
		return
	}
	if callErr.Cooldown > 0 {
		seconds := int64((callErr.Cooldown + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
}

func (h *handler) protocolForRequest(r *http.Request) string {
	// Messages has a unique path. Model discovery shares /v1/models across
	// protocols, so use Anthropic's authentication/version headers only there;
	// other client-only headers must not change the selected client protocol.
	modelRoute := r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models" || strings.HasPrefix(r.URL.Path, "/v1/models/") || strings.HasPrefix(r.URL.Path, "/models/"))
	if r.URL.Path == "/v1/messages" || modelRoute && (r.Header.Get("anthropic-version") != "" || r.Header.Get("x-api-key") != "") {
		return "anthropic"
	}
	return "openai"
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.reload != nil {
		h = h.reload.active.Load()
	}
	r = withRequestLogDetails(r, "", "")
	tracked := &trackedResponseWriter{ResponseWriter: w}
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			observability.LoggerOrDiscard(h.logger).Error("request handler panic recovered",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "request_panic"),
			)
			protocol := h.protocolForRequest(r)
			if tracked.eventStream && !tracked.streamComplete {
				writeStreamFailure(tracked, protocol, "internal_error")
			} else if !tracked.wroteHeader {
				h.reject(tracked, r, protocol, http.StatusInternalServerError, "internal_error")
			}
		}
	}()
	h.serveHTTP(tracked, r)
}

func (h *handler) serveHTTP(w *trackedResponseWriter, r *http.Request) {
	protocol := h.protocolForRequest(r)
	a := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	b := sha256.Sum256([]byte("Bearer " + h.config.AccessToken))
	valid := subtle.ConstantTimeCompare(a[:], b[:]) == 1
	if protocol == "anthropic" {
		a = sha256.Sum256([]byte(r.Header.Get("x-api-key")))
		b = sha256.Sum256([]byte(h.config.AccessToken))
		valid = valid || subtle.ConstantTimeCompare(a[:], b[:]) == 1
	}
	if !valid {
		h.reject(w, r, protocol, 401, "invalid_api_key")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/tidemux/session-status" {
		h.sessionStatus(w)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/tidemux/config-status" && h.reload != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.reload.applicationStatus())
		return
	}
	modelList := r.URL.Path == "/v1/models" || r.URL.Path == "/models"
	modelDetail := strings.HasPrefix(r.URL.Path, "/v1/models/") || strings.HasPrefix(r.URL.Path, "/models/")
	if r.Method == "GET" && (modelList || modelDetail) {
		models := h.allQualifiedModels()
		var detailID string
		var detailProvider Provider
		if modelDetail {
			detailID = strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1"), "/models/")
			providerName, modelID, ok := parseQualifiedModelID(detailID)
			if !ok {
				h.reject(w, r, protocol, 404, "model_not_found")
				return
			}
			var exists bool
			detailProvider, exists = h.providers[providerName]
			if exists {
				if _, unavailable := h.unavailableProviders[providerName]; unavailable {
					h.reject(w, r, protocol, http.StatusServiceUnavailable, "provider_unavailable", "model")
					return
				}
			}
			providerModels, _ := h.modelsForProvider(providerName)
			if !exists || !containsModel(providerModels, modelID) {
				h.reject(w, r, protocol, 404, "model_not_found")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if protocol == "anthropic" {
			if modelDetail {
				model := map[string]any{"id": detailID, "type": "model", "display_name": detailID}
				detailProvider.ModelCapabilities.addToModel(model)
				json.NewEncoder(w).Encode(model)
				return
			}
			data := make([]map[string]any, 0, len(models))
			for _, id := range models {
				model := map[string]any{"id": id, "type": "model", "display_name": id}
				providerName, _, _ := parseQualifiedModelID(id)
				h.providers[providerName].ModelCapabilities.addToModel(model)
				data = append(data, model)
			}
			result := map[string]any{"data": data, "has_more": false}
			if len(models) > 0 {
				result["first_id"], result["last_id"] = models[0], models[len(models)-1]
			}
			json.NewEncoder(w).Encode(result)
		} else {
			if modelDetail {
				model := map[string]any{"id": detailID, "object": "model", "created": 0, "owned_by": detailProvider.UpstreamID}
				detailProvider.ModelCapabilities.addToModel(model)
				json.NewEncoder(w).Encode(model)
				return
			}
			data := make([]map[string]any, 0, len(models))
			for _, id := range models {
				providerName, _, _ := parseQualifiedModelID(id)
				provider := h.providers[providerName]
				model := map[string]any{"id": id, "object": "model", "created": 0, "owned_by": provider.UpstreamID}
				provider.ModelCapabilities.addToModel(model)
				data = append(data, model)
			}
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		}
		return
	}
	path := "/v1/chat/completions"
	if protocol == "anthropic" {
		path = "/v1/messages"
	}
	if r.URL.Path != path || r.Method != "POST" {
		h.reject(w, r, protocol, 404, "unsupported_endpoint")
		return
	}
	callCtx, finishCall, admitted := h.beginCall(r.Context())
	if !admitted {
		h.reject(w, r, protocol, http.StatusServiceUnavailable, "server_shutting_down")
		return
	}
	defer finishCall()
	r = r.WithContext(callCtx)
	stopReadCancellation := context.AfterFunc(callCtx, func() {
		if errors.Is(context.Cause(callCtx), adapter.ErrServerShuttingDown) {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		}
	})
	defer stopReadCancellation()
	options := adapter.CallOptions{AnthropicBeta: strings.Join(r.Header.Values("anthropic-beta"), ","), SessionID: strings.TrimSpace(r.Header.Get(adapter.SessionIDHeader))}
	if err := options.Validate(protocol); err != nil {
		h.reject(w, r, protocol, 400, err.Error(), adapter.ValidationParameter(err))
		return
	}
	defer r.Body.Close()
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.config.Limits.Effective().RequestBytes))
	if err != nil {
		var sizeErr *http.MaxBytesError
		if errors.Is(context.Cause(callCtx), adapter.ErrServerShuttingDown) {
			h.reject(w, r, protocol, http.StatusServiceUnavailable, "server_shutting_down")
		} else if errors.As(err, &sizeErr) {
			h.reject(w, r, protocol, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			h.reject(w, r, protocol, http.StatusBadRequest, "request_read_error")
		}
		return
	}
	if errors.Is(context.Cause(callCtx), adapter.ErrServerShuttingDown) {
		h.reject(w, r, protocol, http.StatusServiceUnavailable, "server_shutting_down")
		return
	}
	body, qualifiedModel, ignoredFields, err := adapter.RequestWithWarnings(protocol, data, "")
	if len(ignoredFields) > 0 {
		observability.LoggerOrDiscard(h.logger).Warn("request contained unsupported extension fields",
			slog.Int("schema_version", observability.SchemaVersion),
			slog.String("event", "request_extension_fields_ignored"),
			slog.String("protocol", protocol),
			slog.Int("field_count", len(ignoredFields)),
		)
	}
	if err != nil {
		h.reject(w, r, protocol, 400, err.Error(), adapter.ValidationParameter(err))
		return
	}
	qualifiedModel = strings.TrimSpace(qualifiedModel)
	if options.SessionID == "" {
		options.SessionID = adapter.SessionID(protocol, body)
	}
	if err := options.Validate(protocol); err != nil {
		h.reject(w, r, protocol, http.StatusBadRequest, "invalid_session_id")
		return
	}
	persistentSession := strings.TrimSpace(options.SessionID) != ""
	sessionKey := h.sharedSessionKey(qualifiedModel, protocol, options.SessionID)
	if qualifiedModel == "auto" {
		sessionKey = h.callerSessionKey("auto", protocol, options.SessionID)
		if !persistentSession {
			options.RequestScopedSession = true
			options.SessionID, err = newRequestID()
			if err != nil {
				h.reject(w, r, protocol, http.StatusInternalServerError, "request_id_failed")
				return
			}
		}
	}
	r = withRequestLogDetails(r, "", qualifiedModel)
	routes, routeError, routeStatus := h.resolveRoutes(qualifiedModel, protocol, body, sessionKey)
	if routeError != "" {
		parameter := "model"
		if routeError == "unsupported_request_feature" {
			parameter = h.routeErrorParameter(qualifiedModel, protocol, body)
		}
		h.reject(w, r, protocol, routeStatus, routeError, parameter)
		return
	}
	if len(routes) == 0 {
		h.reject(w, r, protocol, http.StatusServiceUnavailable, "provider_unavailable", "model")
		return
	}
	initial := routes[0]
	r = withRequestLogDetails(r, initial.provider, initial.model)
	if _, exists := h.providers[initial.provider]; !exists {
		h.reject(w, r, protocol, http.StatusNotFound, "provider_not_found", "model")
		return
	}
	if _, unavailable := h.unavailableProviders[initial.provider]; unavailable {
		if initial.autoChainIndex != nil {
			h.autoChain.recordFailure(*initial.autoChainIndex, len(h.config.AutoChain), &adapter.CallError{FailoverSafe: true, UpstreamNotAttempted: true, Category: adapter.ProviderErrorTemporarilyUnavailable}, r.Context().Err(), false)
		}
		h.reject(w, r, protocol, http.StatusServiceUnavailable, "provider_unavailable", "model")
		return
	}
	if h.clients[initial.provider] == nil {
		h.reject(w, r, protocol, http.StatusNotFound, "provider_not_found", "model")
		return
	}
	if !h.supportScopeAllows(initial.provider, initial.model) {
		h.reject(w, r, protocol, http.StatusNotFound, "model_not_found")
		return
	}
	var mode struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &mode)
	if h.sessionCapacityEnabled(routes) && options.SessionID == "" {
		options.RequestScopedSession = true
		options.SessionID, err = newRequestID()
		if err != nil {
			h.reject(w, r, protocol, http.StatusInternalServerError, "request_id_failed")
			return
		}
	}
	capacityID := options.SessionID
	if capacityID == "" {
		capacityID, err = newRequestID()
		if err != nil {
			h.reject(w, r, protocol, http.StatusInternalServerError, "request_id_failed")
			return
		}
	}
	// Capacity shares a caller/protocol namespace across models and providers.
	// Anonymous identity stays internal unless the route needs an upstream
	// request-scoped ID; it never becomes a retained conversation.
	capacityKey := h.callerSessionKey("", protocol, capacityID)
	lease, err := h.sessions.Acquire(r.Context(), hex.EncodeToString(capacityKey[:]))
	if err != nil {
		if errors.Is(context.Cause(callCtx), adapter.ErrServerShuttingDown) {
			h.reject(w, r, protocol, http.StatusServiceUnavailable, "server_shutting_down")
			return
		}
		if errors.Is(err, limiter.ErrActiveSessionLimit) {
			h.reject(w, r, protocol, http.StatusTooManyRequests, limiter.ErrActiveSessionLimit.Error())
			return
		}
		h.reject(w, r, protocol, http.StatusBadRequest, "invalid_session_id")
		return
	}
	retainSession := false
	admission := &requestSessionAdmission{handler: h, id: hex.EncodeToString(capacityKey[:]), global: lease, providers: map[string]*limiter.SessionLease{}}
	r = r.WithContext(context.WithValue(r.Context(), requestSessionAdmissionKey{}, admission))
	defer func() { admission.release(retainSession) }()

	var delivered bool
	send := func(id string, frame []byte) error {
		if !delivered {
			w.Header().Set("X-TideMux-Request-ID", id)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			delivered = true
		}
		if _, err := w.Write(frame); err != nil {
			return err
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return err
		}
		admission.touchOutput()
		return nil
	}

	attemptRoutes := func(sink adapter.StreamSink) ([]byte, string, error, bool) {
		var lastID string
		var lastErr *adapter.CallError
		var firstBalanceErr *adapter.CallError
		var firstBalanceID string
		for index, route := range routes {
			if index > 0 && !routeCanAdvance(h.config.EffectiveRouting(), route, lastErr, r.Context().Err(), delivered) {
				continue
			}
			if lastErr != nil && lastErr.Category == adapter.ProviderErrorInsufficientBalance {
				if pool := h.keyPools[route.provider]; pool != nil && pool.CooldownWaitAt(time.Now()) > 0 {
					continue
				}
			}
			var response []byte
			var id string
			var callErr error
			var handled bool
			for preDispatch := 0; ; preDispatch++ {
				if route.sessionKey != nil || route.autoChainIndex != nil {
					// Recheck after admission and on a proven pre-dispatch cooldown
					// race. Once the adapter may have sent a request, do not rebind.
					model, key := route.model, route.sessionKey
					if route.autoChainIndex != nil {
						model, key = "auto", sessionKey
					}
					rebound, code, status := h.resolveRoutes(model, protocol, body, key)
					if code != "" {
						parameter := "model"
						if code == "unsupported_request_feature" {
							parameter = h.routeErrorParameter(route.model, protocol, body)
						}
						h.reject(w, r, protocol, status, code, parameter)
						return nil, "", nil, true
					}
					route = rebound[0]
				}
				r = withRequestLogDetails(r, route.provider, route.model)
				response, id, callErr, handled = h.callRouteCandidate(w, r, protocol, route, body, options, sink)
				var preDispatchErr *adapter.CallError
				if route.sessionKey == nil && route.autoChainIndex == nil || handled || delivered || r.Context().Err() != nil || preDispatch >= len(h.providers) || !errors.As(callErr, &preDispatchErr) || !preDispatchErr.UpstreamNotAttempted || preDispatchErr.Code != "provider_keys_cooling_down" {
					break
				}
			}
			if handled {
				return nil, id, nil, true
			}
			lastID = id
			if callErr == nil {
				return response, id, nil, false
			}
			var adapterErr *adapter.CallError
			if !errors.As(callErr, &adapterErr) {
				return nil, id, callErr, false
			}
			lastErr = adapterErr
			if route.autoChainIndex != nil {
				h.autoChain.recordFailure(*route.autoChainIndex, len(h.config.AutoChain), adapterErr, r.Context().Err(), delivered)
			}
			if route.sessionKey != nil {
				h.sharedAffinity.recordFailure(h.routingEpoch, route.provider, route.model, h.providerGenerations[route.provider], adapterErr, r.Context().Err(), delivered)
			}
			if firstBalanceErr == nil && adapterErr.Category == adapter.ProviderErrorInsufficientBalance {
				firstBalanceErr, firstBalanceID = adapterErr, id
			}
			if delivered {
				break
			}
		}
		if firstBalanceErr != nil && !delivered {
			return nil, firstBalanceID, firstBalanceErr, false
		}
		return nil, lastID, lastErr, false
	}

	if mode.Stream {
		terminal, id, callErr, handled := attemptRoutes(send)
		if handled {
			return
		}
		if callErr == nil {
			if send(id, terminal) == nil {
				w.streamComplete = true
			}
			return
		}
		var upstreamErr *adapter.CallError
		if !errors.As(callErr, &upstreamErr) {
			upstreamErr = &adapter.CallError{Status: http.StatusInternalServerError, Code: "internal_error"}
		}
		setRetryAfterHeader(w, upstreamErr)
		if !delivered {
			if id != "" {
				w.Header().Set("X-TideMux-Request-ID", id)
			}
			h.failUpstream(w, upstreamErr, protocol)
			return
		}
		payload := mappedUpstreamErrorPayload(upstreamErr, protocol)
		if upstreamErr.Category == "" {
			detail := map[string]any{"type": "api_error", "message": upstreamErr.Code, "code": upstreamErr.Code}
			addToolHistoryRecovery(detail, upstreamErr.Code, upstreamErr.Param)
			if upstreamErr.Param != "" {
				detail["param"] = upstreamErr.Param
			}
			payload = map[string]any{"error": detail}
			if protocol == "anthropic" {
				payload["type"] = "error"
			}
		}
		if id != "" {
			payload["request_id"] = id
		}
		encoded, _ := json.Marshal(payload)
		send(id, append(append([]byte("event: error\ndata: "), encoded...), []byte("\n\n")...))
		w.streamComplete = true
		return
	}

	response, id, callErr, handled := attemptRoutes(nil)
	if handled {
		return
	}
	if id != "" {
		w.Header().Set("X-TideMux-Request-ID", id)
	}
	if callErr != nil {
		var upstreamErr *adapter.CallError
		if errors.As(callErr, &upstreamErr) {
			setRetryAfterHeader(w, upstreamErr)
			h.failUpstream(w, upstreamErr, protocol)
		} else {
			h.fail(w, http.StatusInternalServerError, "internal_error", protocol)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	n, writeErr := w.Write(response)
	if writeErr != nil || n != len(response) {
		return
	}
	if flushErr := http.NewResponseController(w).Flush(); flushErr != nil && !errors.Is(flushErr, http.ErrNotSupported) {
		return
	}
	if r.Context().Err() != nil {
		return
	}
	admission.touchOutput()
	retainSession = persistentSession && !mode.Stream
}

func newRequestID() (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce), nil
}

func (h *handler) reject(w http.ResponseWriter, r *http.Request, protocol string, status int, code string, param ...string) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		h.fail(w, 500, "request_id_failed", protocol)
		return
	}
	id := hex.EncodeToString(nonce)
	w.Header().Set("X-TideMux-Request-ID", id)
	details, _ := r.Context().Value(requestLogContextKey{}).(requestLogContext)
	method := r.Method
	if method != "GET" && method != "POST" {
		method = "OTHER"
	}
	endpoint := "unsupported"
	switch r.URL.Path {
	case "/v1/messages":
		endpoint = "messages"
	case "/v1/chat/completions":
		endpoint = "chat_completions"
	case "/v1/models", "/models":
		endpoint = "models"
	}
	timestamp := time.Now().UnixMilli()
	diagnosticErr := h.ledger.AppendDiagnostic(ledger.Diagnostic{ID: id, TimestampMS: timestamp, Protocol: protocol, Method: method, Endpoint: endpoint, Status: status, ErrorCode: code})
	if diagnosticErr != nil {
		status = http.StatusInternalServerError
		code = "local_diagnostic_failed"
	}
	latency := int64(0)
	if !details.started.IsZero() {
		latency = time.Since(details.started).Milliseconds()
	}
	observability.RequestSummary{
		Event: "local_rejection", RequestID: id, Protocol: protocol,
		Endpoint: endpoint, ProviderRef: details.providerRef, Model: details.model,
		Outcome: "rejected", ErrorCode: code, HTTPStatus: status,
		LatencyMS: latency, QueueTimeMS: 0, UpstreamAttempted: false,
		RecordPersisted: diagnosticErr == nil,
		TimestampMS:     timestamp,
	}.Log(h.logger)
	if diagnosticErr != nil {
		h.fail(w, status, code, protocol)
		return
	}
	h.fail(w, status, code, protocol, param...)
}
