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
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

func NewHandler(c Config, httpClient *http.Client) (http.Handler, func() error, error) {
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	if c.APIKey == "" || c.AccessToken == "" || c.APIKey == c.AccessToken {
		return nil, nil, errors.New("distinct resolved credentials are required")
	}
	l, err := ledger.Open(c.LedgerPath)
	if err != nil {
		return nil, nil, errors.New("cannot open ledger")
	}
	gate, _ := limiter.NewConcurrencyGate(c.MaxInFlight)
	return &handler{config: c, ledger: l, client: &adapter.Client{Protocol: c.Protocol, BaseURL: c.BaseURL, APIKey: c.APIKey, APIVersion: c.APIVersion, Upstream: c.UpstreamID, Prices: c.Prices, Limits: c.Limits, HTTP: httpClient, Ledger: l, Gate: gate}}, l.Close, nil
}
func Open(c Config, client *http.Client) (net.Listener, *http.Server, func() error, error) {
	h, closeLedger, err := NewHandler(c, client)
	if err != nil {
		return nil, nil, nil, err
	}
	listener, err := net.Listen("tcp", c.ListenAddr)
	if err != nil {
		closeLedger()
		return nil, nil, nil, errors.New("cannot bind loopback listener")
	}
	return listener, &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}, func() error { listener.Close(); return closeLedger() }, nil
}

type handler struct {
	config Config
	client *adapter.Client
	ledger *ledger.Ledger
}

func (h *handler) fail(w http.ResponseWriter, status int, code string) {
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
	if h.config.Protocol == "anthropic" {
		json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": code}})
	} else {
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": code, "type": kind, "code": code, "param": nil}})
	}
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	b := sha256.Sum256([]byte("Bearer " + h.config.AccessToken))
	valid := subtle.ConstantTimeCompare(a[:], b[:]) == 1
	if h.config.Protocol == "anthropic" {
		a = sha256.Sum256([]byte(r.Header.Get("x-api-key")))
		b = sha256.Sum256([]byte(h.config.AccessToken))
		valid = valid || subtle.ConstantTimeCompare(a[:], b[:]) == 1
	}
	if !valid {
		h.reject(w, r, 401, "invalid_api_key")
		return
	}
	modelList := r.URL.Path == "/v1/models" || r.URL.Path == "/models"
	modelDetail := strings.HasPrefix(r.URL.Path, "/v1/models/") || strings.HasPrefix(r.URL.Path, "/models/")
	if r.Method == "GET" && (modelList || modelDetail) {
		if modelDetail {
			modelID := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1"), "/models/")
			if modelID != h.config.Model {
				h.reject(w, r, 404, "model_not_found")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		model := map[string]any{"id": h.config.Model, "object": "model", "created": 0, "owned_by": h.config.UpstreamID}
		if h.config.Protocol == "anthropic" {
			model = map[string]any{"id": h.config.Model, "type": "model", "display_name": h.config.Model}
			h.config.ModelCapabilities.addToModel(model)
			if modelDetail {
				json.NewEncoder(w).Encode(model)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{model}, "has_more": false, "first_id": h.config.Model, "last_id": h.config.Model})
		} else {
			h.config.ModelCapabilities.addToModel(model)
			if modelDetail {
				json.NewEncoder(w).Encode(model)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{model}})
		}
		return
	}
	path := "/v1/chat/completions"
	if h.config.Protocol == "anthropic" {
		path = "/v1/messages"
	}
	if r.URL.Path != path || r.Method != "POST" {
		h.reject(w, r, 404, "unsupported_endpoint")
		return
	}
	options := adapter.CallOptions{AnthropicBeta: strings.Join(r.Header.Values("anthropic-beta"), ",")}
	if err := options.Validate(h.config.Protocol); err != nil {
		h.reject(w, r, 400, "invalid_beta_header")
		return
	}
	defer r.Body.Close()
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.config.Limits.Effective().RequestBytes))
	if err != nil {
		h.reject(w, r, 413, "request_too_large")
		return
	}
	body, model, err := adapter.Request(h.config.Protocol, data, h.config.Model)
	if err != nil {
		h.reject(w, r, 400, err.Error())
		return
	}
	reservationID := ""
	if h.config.Budget != (ledger.BudgetPolicy{}) {
		reservationID = newRequestID()
		_, err := h.ledger.ReserveBudget(r.Context(), reservationID, h.config.Budget, r.Header.Get("X-TideMux-Budget-Confirm") == "1", time.Now())
		if err != nil {
			code := "budget_reservation_failed"
			if err.Error() == "budget_hard_limit" || err.Error() == "budget_confirmation_required" {
				code = err.Error()
			}
			h.reject(w, r, 429, code)
			return
		}
	}
	settle := func(auditID string) {
		if reservationID != "" {
			_ = h.ledger.SettleBudget(context.Background(), reservationID, auditID)
		}
	}
	var mode struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &mode)
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
			return http.NewResponseController(w).Flush()
		}
		terminal, id, err := h.client.CallWithOptions(r.Context(), body, model, send, options)
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
			h.fail(w, ce.Status, ce.Code)
			return
		}
		payload := map[string]any{"error": map[string]string{"type": "api_error", "message": ce.Code}}
		if h.config.Protocol == "anthropic" {
			payload["type"] = "error"
		}
		encoded, _ := json.Marshal(payload)
		send(id, append(append([]byte("event: error\ndata: "), encoded...), []byte("\n\n")...))
		return
	}
	response, id, err := h.client.CallWithOptions(r.Context(), body, model, nil, options)
	settle(id)
	if id != "" {
		w.Header().Set("X-TideMux-Request-ID", id)
	}
	if err != nil {
		var ce *adapter.CallError
		if errors.As(err, &ce) {
			h.fail(w, ce.Status, ce.Code)
		} else {
			h.fail(w, 500, "internal_error")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(response)
}

func newRequestID() string {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return ""
	}
	return hex.EncodeToString(nonce)
}

func (h *handler) reject(w http.ResponseWriter, r *http.Request, status int, code string) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		h.fail(w, 500, "request_id_failed")
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
	if err := h.client.Ledger.AppendDiagnostic(ledger.Diagnostic{ID: id, TimestampMS: time.Now().UnixMilli(), Protocol: h.config.Protocol, Method: method, Endpoint: endpoint, Status: status, ErrorCode: code}); err != nil {
		h.fail(w, 500, "local_diagnostic_failed")
		return
	}
	h.fail(w, status, code)
}
