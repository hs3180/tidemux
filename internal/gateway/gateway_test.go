package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/ledger"
)

func testConfig(ledgerPath, baseURL string) Config {
	return Config{
		ListenAddr: "127.0.0.1:0", DeepSeekAPIKey: "test-key", AccessToken: "gateway-test-token",
		DeepSeekKeychain:    KeychainReference{Service: "test.deepseek", Account: "default"},
		AccessTokenKeychain: KeychainReference{Service: "test.gateway", Account: "default"},
		DeepSeekBaseURL:     baseURL, MaxInFlight: 1, LedgerPath: ledgerPath,
	}
}

func TestHandlerForwardsMinimalChatCompletionAndWritesLedger(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected upstream request: %s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"deepseek-chat"`) {
			t.Fatalf("model missing from forwarded request: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"upstream-1","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	config := testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL)
	h, closeLedger, err := NewHandler(config, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeLedger()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer gateway-test-token")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"content":"hello"`) || !strings.Contains(response.Body.String(), `"object":"chat.completion"`) {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
	store, err := ledger.Open(config.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var count int
	if err := store.QueryRow(request.Context(), "SELECT count(*) FROM ledger_requests").Scan(&count); err != nil || count != 1 {
		t.Fatalf("ledger requests=%d err=%v", count, err)
	}
}

func TestOpenBindsLoopbackGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer upstream.Close()
	listener, server, closeGateway, err := Open(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL), upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	go server.Serve(listener)
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer gateway-test-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
}

func TestConfigRejectsNonLoopbackListener(t *testing.T) {
	config := testConfig("ledger.db", "")
	config.ListenAddr = "0.0.0.0:8080"
	if err := config.Validate(); err == nil {
		t.Fatal("expected non-loopback listener to be rejected")
	}
}

func TestHandlerReturnsSafeStructuredErrors(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		authorization string
		upstreamCode  int
		cancel        bool
		wantStatus    int
		wantType      string
		wantCode      string
	}{
		{name: "missing credentials", body: `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`, wantStatus: http.StatusUnauthorized, wantType: "authentication_error", wantCode: "invalid_api_key"},
		{name: "invalid request", authorization: "Bearer gateway-test-token", body: `{"model":"","messages":[]}`, wantStatus: http.StatusBadRequest, wantType: "invalid_request_error", wantCode: "model"},
		{name: "stream unsupported", authorization: "Bearer gateway-test-token", body: `{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`, wantStatus: http.StatusBadRequest, wantType: "invalid_request_error", wantCode: "stream"},
		{name: "upstream rate limited", authorization: "Bearer gateway-test-token", body: `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`, upstreamCode: http.StatusTooManyRequests, wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error", wantCode: "upstream_rate_limited"},
		{name: "upstream failure", authorization: "Bearer gateway-test-token", body: `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`, upstreamCode: http.StatusInternalServerError, wantStatus: http.StatusBadGateway, wantType: "api_error", wantCode: "upstream_error"},
		{name: "canceled request", authorization: "Bearer gateway-test-token", body: `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`, cancel: true, wantStatus: http.StatusRequestTimeout, wantType: "request_canceled", wantCode: "request_canceled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.cancel {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(test.upstreamCode)
				_, _ = w.Write([]byte(`{"error":{"message":"provider secret detail"}}`))
			}))
			defer upstream.Close()
			config := testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL)
			config.DeepSeekAPIKey = "provider-secret"
			h, closeLedger, err := NewHandler(config, upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeLedger()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
			request.Header.Set("Authorization", test.authorization)
			if test.cancel {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var payload struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Type != test.wantType || payload.Error.Code != test.wantCode {
				t.Fatalf("error=%+v", payload.Error)
			}
			if strings.Contains(response.Body.String(), "provider-secret") || strings.Contains(response.Body.String(), "provider secret detail") {
				t.Fatalf("secret leaked: %s", response.Body.String())
			}
		})
	}
}
