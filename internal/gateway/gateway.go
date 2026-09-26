package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

type handler struct {
	config      Config
	ledger      *ledger.Ledger
	sessions    *limiter.SessionLimiter
	providers   map[string]Provider
	clients     map[string]*adapter.Client
	keyPools    map[string]*providerKeyPool
	models      map[string][]string
	modelsKnown map[string]bool
}

func (h *handler) modelsForProvider(providerName string) ([]string, bool) {
	if provider, ok := h.providers[providerName]; ok && len(provider.SupportedModels) > 0 {
		return provider.SupportedModels, true
	}
	return h.models[providerName], h.modelsKnown[providerName]
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
		models, _ := h.modelsForProvider(providerName)
		for _, model := range models {
			result = append(result, qualifiedModelID(providerName, model))
		}
	}
	sort.Strings(result)
	return result
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
	if protocol == "anthropic" {
		detail := map[string]any{"type": kind, "message": code}
		if field != "" {
			detail["param"] = field
		}
		json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": detail})
	} else {
		detail := map[string]any{"message": code, "type": kind, "code": code, "param": nil}
		if field != "" {
			detail["param"] = field
		}
		json.NewEncoder(w).Encode(map[string]any{"error": detail})
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
	options := adapter.CallOptions{AnthropicBeta: strings.Join(r.Header.Values("anthropic-beta"), ","), SessionID: strings.TrimSpace(r.Header.Get(adapter.SessionIDHeader))}
	if err := options.Validate(protocol); err != nil {
		h.reject(w, r, protocol, 400, err.Error(), adapter.ValidationParameter(err))
		return
	}
	defer r.Body.Close()
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.config.Limits.Effective().RequestBytes))
	if err != nil {
		h.reject(w, r, protocol, 413, "request_too_large")
		return
	}
	body, qualifiedModel, ignoredFields, err := adapter.RequestWithWarnings(protocol, data, "")
	if len(ignoredFields) > 0 {
		log.Printf("tidemux: ignored unsupported request fields protocol=%s fields=%q", protocol, ignoredFields)
	}
	if err != nil {
		h.reject(w, r, protocol, 400, err.Error(), adapter.ValidationParameter(err))
		return
	}
	providerName, model, qualified := parseQualifiedModelID(qualifiedModel)
	if !qualified {
		h.reject(w, r, protocol, 400, "model_must_include_provider", "model")
		return
	}
	provider, providerExists := h.providers[providerName]
	providerClient := h.clients[providerName]
	if !providerExists || providerClient == nil {
		h.reject(w, r, protocol, 404, "provider_not_found", "model")
		return
	}
	body, err = replaceRequestModel(body, model)
	if err != nil {
		h.reject(w, r, protocol, 400, "invalid_request", "model")
		return
	}
	if len(provider.SupportedModels) > 0 && !containsModel(provider.SupportedModels, model) {
		h.reject(w, r, protocol, 404, "model_not_found")
		return
	}
	if options.SessionID == "" {
		options.SessionID = adapter.SessionID(protocol, body)
	}
	if err := options.Validate(protocol); err != nil {
		h.reject(w, r, protocol, 400, "invalid_session_id")
		return
	}
	var mode struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &mode)
	persistentSession := strings.TrimSpace(options.SessionID) != ""
	if h.config.MaxActiveSessions > 0 && !persistentSession {
		options.SessionID, err = newRequestID()
		if err != nil {
			h.reject(w, r, protocol, 500, "request_id_failed")
			return
		}
	}
	lease, err := h.sessions.Acquire(r.Context(), options.SessionID)
	if err != nil {
		if errors.Is(err, limiter.ErrActiveSessionLimit) {
			w.Header().Set("Retry-After", "1")
			h.reject(w, r, protocol, 429, limiter.ErrActiveSessionLimit.Error())
			return
		}
		h.reject(w, r, protocol, 400, "invalid_session_id")
		return
	}
	retainSession := false
	defer func() { lease.Release(retainSession) }()
	client := providerClient
	if pool := h.keyPools[providerName]; pool != nil {
		selected := *providerClient
		selected.APIKey = pool.Next()
		client = &selected
	}
	if client == nil {
		h.reject(w, r, protocol, 500, "protocol_client_unavailable")
		return
	}
	reservationID := ""
	var budget ledger.BudgetPolicy
	if provider.Budget != nil {
		budget = *provider.Budget
	}
	if budget != (ledger.BudgetPolicy{}) {
		price, priced := provider.Prices[model]
		if !priced {
			price, priced = adapter.BuiltInPrice(provider.BaseURL, model, time.Now())
		}
		if !priced || price.Currency != budget.Currency {
			h.reject(w, r, protocol, 503, "budget_pricing_unconfigured")
			return
		}
		reservationID, err = newRequestID()
		if err != nil {
			h.reject(w, r, protocol, 500, "request_id_failed")
			return
		}
		decision, err := h.ledger.CheckBudget(r.Context(), reservationID, providerName, budget, r.Header.Get("X-TideMux-Budget-Confirm") == "1", time.Now())
		if err != nil {
			code := "budget_reservation_failed"
			if err.Error() == "budget_hard_limit" || err.Error() == "budget_confirmation_required" || err.Error() == "budget_usage_unknown" {
				code = err.Error()
			}
			h.reject(w, r, protocol, 429, code)
			return
		}
		if decision.Warning {
			w.Header().Set("X-TideMux-Budget-Warning", "1")
		}
	}
	settle := func(auditID string) {
		if reservationID != "" {
			_ = h.ledger.RecordBudgetCharge(context.Background(), reservationID, auditID, providerName, budget.Currency, time.Now())
		}
	}
	if mode.Stream {
		sent := false
		send := func(id string, frame []byte) error {
			if !sent {
				w.Header().Set("X-TideMux-Request-ID", id)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(200)
				sent = true
			}
			if _, err := w.Write(frame); err != nil {
				return err
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return err
			}
			lease.TouchOutput()
			return nil
		}
		terminal, id, err := client.CallFrom(protocol, r.Context(), body, model, send, options)
		settle(id)
		if err == nil {
			send(id, terminal)
			return
		}
		var ce *adapter.CallError
		if !errors.As(err, &ce) {
			ce = &adapter.CallError{Status: 500, Code: "internal_error"}
		}
		if !sent {
			if id != "" {
				w.Header().Set("X-TideMux-Request-ID", id)
			}
			h.fail(w, ce.Status, ce.Code, protocol, ce.Param)
			return
		}
		detail := map[string]any{"type": "api_error", "message": ce.Code}
		if ce.Param != "" {
			detail["param"] = ce.Param
		}
		payload := map[string]any{"error": detail}
		if protocol == "anthropic" {
			payload["type"] = "error"
		}
		encoded, _ := json.Marshal(payload)
		send(id, append(append([]byte("event: error\ndata: "), encoded...), []byte("\n\n")...))
		return
	}
	response, id, err := client.CallFrom(protocol, r.Context(), body, model, nil, options)
	settle(id)
	if id != "" {
		w.Header().Set("X-TideMux-Request-ID", id)
	}
	if err != nil {
		var ce *adapter.CallError
		if errors.As(err, &ce) {
			h.fail(w, ce.Status, ce.Code, protocol, ce.Param)
		} else {
			h.fail(w, 500, "internal_error", protocol)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
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
	lease.TouchOutput()
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
	if err := h.ledger.AppendDiagnostic(ledger.Diagnostic{ID: id, TimestampMS: time.Now().UnixMilli(), Protocol: protocol, Method: method, Endpoint: endpoint, Status: status, ErrorCode: code}); err != nil {
		h.fail(w, 500, "local_diagnostic_failed", protocol)
		return
	}
	h.fail(w, status, code, protocol, param...)
}
