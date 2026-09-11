package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

// NewHandler wires the configured DeepSeek provider, local ledger, and
// concurrency gate into the smallest supported OpenAI-compatible route.
func NewHandler(config Config, client *http.Client) (http.Handler, func() error, error) {
	if err := config.Validate(); err != nil {
		return nil, nil, err
	}
	store, err := ledger.Open(config.LedgerPath)
	if err != nil {
		return nil, nil, err
	}
	gate, err := limiter.NewConcurrencyGate(config.MaxInFlight)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	provider, err := adapter.NewDeepSeekClient(adapter.DeepSeekConfig{
		APIKey: config.DeepSeekAPIKey, BaseURL: config.DeepSeekBaseURL, HTTPClient: client,
		Ledger: store, Limiter: gate,
	})
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return &handler{provider: provider, accessToken: config.AccessToken}, store.Close, nil
}

// Open binds a validated loopback listener and returns the server for the
// caller to run. Keeping Serve under the caller makes shutdown explicit.
func Open(config Config, client *http.Client) (net.Listener, *http.Server, func() error, error) {
	h, closeLedger, err := NewHandler(config, client)
	if err != nil {
		return nil, nil, nil, err
	}
	listener, err := net.Listen("tcp", config.ListenAddr)
	if err != nil {
		_ = closeLedger()
		return nil, nil, nil, fmt.Errorf("listen on TideMux loopback address: %w", err)
	}
	return listener, &http.Server{Handler: h}, func() error {
		listener.Close()
		return closeLedger()
	}, nil
}

type handler struct {
	provider    *adapter.DeepSeekClient
	accessToken string
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func writeError(w http.ResponseWriter, status int, message, kind, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: apiError{Message: message, Type: kind, Code: code}})
}

func validChatRequest(request adapter.ChatRequest) (string, bool) {
	if strings.TrimSpace(request.Model) == "" {
		return "model", false
	}
	if request.Stream {
		return "stream", false
	}
	if len(request.Messages) == 0 {
		return "messages", false
	}
	for _, message := range request.Messages {
		if strings.TrimSpace(message.Role) == "" || message.Content == "" {
			return "messages", false
		}
	}
	return "", true
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+h.accessToken {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "invalid gateway credentials", "authentication_error", "invalid_api_key")
		return
	}
	defer r.Body.Close()
	var request adapter.ChatRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid chat completion request", "invalid_request_error", "invalid_request")
		return
	}
	if param, ok := validChatRequest(request); !ok {
		writeError(w, http.StatusBadRequest, "invalid chat completion request", "invalid_request_error", param)
		return
	}
	choice, err := h.provider.Chat(r.Context(), request)
	if err != nil {
		var upstream *adapter.UpstreamError
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			writeError(w, http.StatusRequestTimeout, "request canceled before completion", "request_canceled", "request_canceled")
		case errors.As(err, &upstream) && upstream.StatusCode == http.StatusTooManyRequests:
			writeError(w, http.StatusTooManyRequests, "upstream provider is rate limited; retry later", "rate_limit_error", "upstream_rate_limited")
		default:
			writeError(w, http.StatusBadGateway, "upstream chat completion failed", "api_error", "upstream_error")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		ID      string               `json:"id"`
		Object  string               `json:"object"`
		Created int64                `json:"created"`
		Model   string               `json:"model"`
		Choices []adapter.ChatChoice `json:"choices"`
	}{ID: fmt.Sprintf("tidemux-%d", time.Now().UnixNano()), Object: "chat.completion", Created: time.Now().Unix(), Model: request.Model, Choices: []adapter.ChatChoice{choice}})
}
