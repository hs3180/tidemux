package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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
	return &handler{config: c, client: &adapter.Client{Protocol: c.Protocol, BaseURL: c.BaseURL, APIKey: c.APIKey, APIVersion: c.APIVersion, Upstream: c.UpstreamID, Prices: c.Prices, HTTP: httpClient, Ledger: l, Gate: gate}}, l.Close, nil
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
		h.fail(w, 401, "invalid_api_key")
		return
	}
	path := "/v1/chat/completions"
	if h.config.Protocol == "anthropic" {
		path = "/v1/messages"
	}
	if r.URL.Path != path || r.Method != "POST" {
		h.fail(w, 404, "unsupported_endpoint")
		return
	}
	defer r.Body.Close()
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		h.fail(w, 413, "request_too_large")
		return
	}
	body, model, err := adapter.Request(h.config.Protocol, data, h.config.Model)
	if err != nil {
		h.fail(w, 400, err.Error())
		return
	}
	response, id, err := h.client.Call(r.Context(), body, model)
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
