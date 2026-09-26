package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
)

func testConfig(path, url string) Config {
	return Config{ListenAddr: "127.0.0.1:0", Protocol: "openai", BaseURL: url, Model: "custom-model", UpstreamID: "test", APIKey: "provider-secret", AccessToken: "local-secret", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "default"}, AccessTokenKeychain: KeychainReference{Service: "test.gateway", Account: "default"}, MaxInFlight: 1, LedgerPath: path, APIVersion: "2023-06-01"}
}

func namedProviderConfig(c Config, name string) Config {
	protocol, version := c.Protocol, c.APIVersion
	if protocol == "" {
		protocol = "openai"
	}
	if protocol == "openai" {
		version = ""
	}
	provider := Provider{Protocol: protocol, BaseURL: c.BaseURL, UpstreamKeychain: c.UpstreamKeychain, APIVersion: version, UpstreamID: c.UpstreamID, ModelCapabilities: c.ModelCapabilities, Prices: c.Prices, APIKey: c.APIKey}
	c.Protocol, c.BaseURL, c.APIVersion = "", "", ""
	c.UpstreamKeychain, c.Model, c.UpstreamID = KeychainReference{}, "", ""
	c.ModelCapabilities, c.Prices, c.APIKey = ModelCapabilities{}, nil, ""
	c.Providers = map[string]Provider{name: provider}
	return c
}
func requestBody(protocol string) string {
	return requestBodyFor(protocol, "legacy", "custom-model")
}

func requestBodyFor(protocol, providerName, model string) string {
	if protocol == "anthropic" {
		return `{"model":"` + providerName + "/" + model + `","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	}
	return `{"model":"` + providerName + "/" + model + `","messages":[{"role":"user","content":"hello"}]}`
}
func responseBody(protocol string) string {
	if protocol == "anthropic" {
		return `{"id":"msg1","type":"message","role":"assistant","model":"custom-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	}
	return `{"id":"chat1","object":"chat.completion","model":"custom-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
}

func TestOpenAllowsExplicitExternalListen(t *testing.T) {
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "https://example.com/v1")
	c.ListenAddr = "0.0.0.0:0"
	listener, server, closeGateway, err := Open(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	if server.Handler == nil {
		t.Fatal("server handler is nil")
	}
	host, _, err := net.SplitHostPort(listener.Addr().String())
	ip := net.ParseIP(host)
	if err != nil || ip == nil || ip.IsLoopback() {
		t.Fatalf("listener address=%s err=%v", listener.Addr(), err)
	}
}

func endpoint(protocol string) string {
	return "/v1/chat/completions"
}
func TestBothProtocolsAndFailureAudit(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, scenario := range []string{"success", "429", "500", "malformed", "missing-content", "missing-usage", "canceled", "transport", "read-failure"} {
			t.Run(protocol+"/"+scenario, func(t *testing.T) {
				calls := 0
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					want := "/custom/v1/chat/completions"
					if protocol == "anthropic" {
						want = "/custom/v1/messages"
					}
					if r.URL.Path != want {
						t.Errorf("path %s", r.URL.Path)
					}
					if protocol == "anthropic" {
						if r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
							t.Error("wrong Anthropic authentication")
						}
					} else if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("x-api-key") != "" {
						t.Error("wrong OpenAI authentication")
					}
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), "custom-model") {
						t.Error("model lost")
					}
					switch scenario {
					case "429":
						w.WriteHeader(429)
						io.WriteString(w, `{"error":"provider-secret private detail"}`)
					case "500":
						w.WriteHeader(500)
					case "malformed":
						io.WriteString(w, "invalid")
					case "missing-content":
						io.WriteString(w, `{"usage":{}}`)
					case "missing-usage":
						var payload map[string]any
						json.Unmarshal([]byte(responseBody(protocol)), &payload)
						delete(payload, "usage")
						json.NewEncoder(w).Encode(payload)
					case "read-failure":
						w.Header().Set("Content-Length", "10000")
						io.WriteString(w, "short")
					default:
						io.WriteString(w, responseBody(protocol))
					}
				}))
				defer up.Close()
				c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL+"/custom/v1/")
				c.Protocol = protocol
				if scenario == "transport" {
					up.Close()
				}
				h, closeDB, err := NewHandler(c, up.Client())
				if err != nil {
					t.Fatal(err)
				}
				defer closeDB()
				req := httptest.NewRequest("POST", endpoint(protocol), strings.NewReader(requestBody(protocol)))
				req.Header.Set("Authorization", "Bearer local-secret")
				if scenario == "canceled" {
					ctx, cancel := context.WithCancel(req.Context())
					cancel()
					req = req.WithContext(ctx)
				}
				out := httptest.NewRecorder()
				h.ServeHTTP(out, req)
				want := 502
				status := "error"
				if scenario == "success" || scenario == "missing-usage" {
					want = 200
					status = "ok"
				}
				if scenario == "429" {
					want = 429
				}
				if scenario == "canceled" {
					want = 408
					status = "canceled"
					if calls != 0 {
						t.Error("canceled request reached upstream")
					}
				}
				if out.Code != want {
					t.Fatalf("code=%d want %d body=%s", out.Code, want, out.Body.String())
				}
				if strings.Contains(out.Body.String(), "provider-secret") {
					t.Error("secret leaked")
				}
				l, err := ledger.Open(c.LedgerPath)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				rows, err := l.Recent(context.Background(), 10)
				if err != nil || len(rows) != 1 {
					t.Fatalf("audit count=%d err=%v", len(rows), err)
				}
				a := rows[0]
				if a.Status != status || a.Protocol != protocol || a.ID != out.Header().Get("X-TideMux-Request-ID") {
					t.Fatalf("audit %+v", a)
				}
				if a.EstimatedCost != nil {
					t.Error("unconfigured price produced cost")
				}
				if scenario != "success" && a.InputTokens != nil {
					t.Error("unknown usage presented as zero")
				}
			})
		}
	}
}

func TestProviderProtocolsShareOpenAIClientRoute(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			var paths []string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if protocol == "anthropic" {
					if r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
						t.Errorf("Anthropic authentication = %q/%q/%q", r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.Header.Get("Authorization"))
					}
					io.WriteString(w, responseBody("anthropic"))
					return
				}
				if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("x-api-key") != "" {
					t.Errorf("OpenAI authentication = %q/%q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
				}
				io.WriteString(w, responseBody("openai"))
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL+"/provider/v1")
			c.Protocol = protocol
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(requestBody("openai")))
			req.Header.Set("Authorization", "Bearer local-secret")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != 200 || !strings.Contains(out.Body.String(), `"chat.completion"`) {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
			wantPath := "/provider/v1/chat/completions"
			if protocol == "anthropic" {
				wantPath = "/provider/v1/messages"
			}
			if strings.Join(paths, ",") != wantPath {
				t.Fatalf("upstream paths=%v", paths)
			}
		})
	}
}

func TestProviderProtocolsShareAnthropicClientRoute(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			var path string
			var upstreamBody string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				upstreamBody = string(body)
				if protocol == "anthropic" {
					if r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
						t.Errorf("Anthropic authentication = %q/%q/%q", r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.Header.Get("Authorization"))
					}
					io.WriteString(w, responseBody("anthropic"))
					return
				}
				if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("x-api-key") != "" {
					t.Errorf("OpenAI authentication = %q/%q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
				}
				io.WriteString(w, responseBody("openai"))
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL+"/provider/v1")
			c.Protocol = protocol
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(requestBody("anthropic")))
			req.Header.Set("x-api-key", "local-secret")
			req.Header.Set("anthropic-version", "2023-06-01")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != 200 || !strings.Contains(out.Body.String(), `"type":"message"`) {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
			wantPath := "/provider/v1/chat/completions"
			if protocol == "anthropic" {
				wantPath = "/provider/v1/messages"
			}
			if path != wantPath {
				t.Fatalf("upstream path=%s want %s", path, wantPath)
			}
			if protocol == "openai" && (!strings.Contains(upstreamBody, `"messages"`) || !strings.Contains(upstreamBody, `"role":"user"`) || !strings.Contains(upstreamBody, `"max_tokens":16`)) {
				t.Fatalf("Anthropic request was not translated: %s", upstreamBody)
			}
		})
	}
}

func TestAnthropicToolHintIsAcceptedAndNotForwarded(t *testing.T) {
	body := `{"model":"legacy/custom-model","max_tokens":16,"store":true,"messages":[{"role":"user","content":"use a tool"}],"tools":[{"name":"read_file","input_schema":{"type":"object"},"eager_input_streaming":true}]}`
	for _, providerProtocol := range []string{"anthropic", "openai"} {
		t.Run(providerProtocol, func(t *testing.T) {
			var upstreamBody string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				upstreamBody = string(data)
				io.WriteString(w, responseBody(providerProtocol))
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL+"/provider/v1")
			c.Protocol = providerProtocol
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()

			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			req.Header.Set("x-api-key", "local-secret")
			req.Header.Set("anthropic-version", "2023-06-01")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != 200 {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
			if strings.Contains(upstreamBody, "eager_input_streaming") {
				t.Fatalf("client-only hint reached %s provider: %s", providerProtocol, upstreamBody)
			}
		})
	}
}

func TestAnthropicForeignDialectFieldsDoNotRejectRequest(t *testing.T) {
	body := `{"model":"legacy/custom-model","max_tokens":16,"messages":[{"role":"user","content":"hi","tool_calls":"invalid","tool_call_id":false,"reasoning_content":4}],"reasoning_effort":123,"response_format":"invalid","stop":{"invalid":true},"stream_options":"invalid","parallel_tool_calls":{},"max_completion_tokens":"invalid","user":false,"dsh_plugin_packages":false}`
	for _, providerProtocol := range []string{"anthropic", "openai"} {
		t.Run(providerProtocol, func(t *testing.T) {
			var upstreamBody string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				upstreamBody = string(data)
				io.WriteString(w, responseBody(providerProtocol))
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL+"/provider/v1")
			c.Protocol = providerProtocol
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()

			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			req.Header.Set("x-api-key", "local-secret")
			req.Header.Set("anthropic-version", "2023-06-01")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != 200 {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
			for _, field := range []string{"reasoning_effort", "response_format", "stop", "stream_options", "parallel_tool_calls", "max_completion_tokens", "tool_calls", "tool_call_id", "reasoning_content", "dsh_plugin_packages"} {
				if strings.Contains(upstreamBody, `"`+field+`":`) {
					t.Errorf("foreign field %q reached %s provider: %s", field, providerProtocol, upstreamBody)
				}
			}
			if strings.Contains(upstreamBody, `"user":`) {
				t.Errorf("foreign user field reached %s provider: %s", providerProtocol, upstreamBody)
			}
		})
	}
}

func TestValidationErrorIdentifiesParameter(t *testing.T) {
	for _, test := range []struct {
		name, protocol, path, body string
	}{
		{
			name:     "openai",
			protocol: "openai",
			path:     "/v1/chat/completions",
			body:     `{"model":"legacy/custom-model","messages":[{"role":"user","content":"hi"}],"temperature":"invalid"}`,
		},
		{
			name:     "anthropic",
			protocol: "anthropic",
			path:     "/v1/messages",
			body:     `{"model":"legacy/custom-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"temperature":"invalid"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "https://provider.example/v1")
			c.Protocol = test.protocol
			h, closeDB, err := NewHandler(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			req := httptest.NewRequest("POST", test.path, strings.NewReader(test.body))
			if test.protocol == "anthropic" {
				req.Header.Set("x-api-key", "local-secret")
			} else {
				req.Header.Set("Authorization", "Bearer local-secret")
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != 400 {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
			var payload struct {
				Error struct {
					Param string `json:"param"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Param != "temperature" {
				t.Fatalf("param=%q body=%s", payload.Error.Param, out.Body.String())
			}
		})
	}
}

func TestCrossProtocolResponseErrorsIdentifyFinishReason(t *testing.T) {
	for _, test := range []struct {
		name, clientProtocol, providerProtocol, response, field string
	}{
		{
			name:             "openai content filter to anthropic",
			clientProtocol:   "anthropic",
			providerProtocol: "openai",
			response:         `{"id":"chat1","object":"chat.completion","model":"custom-model","choices":[{"index":0,"message":{"role":"assistant","content":"filtered"},"finish_reason":"content_filter"}]}`,
			field:            "choices[0].finish_reason",
		},
		{
			name:             "anthropic pause turn to openai",
			clientProtocol:   "openai",
			providerProtocol: "anthropic",
			response:         `{"id":"msg1","type":"message","role":"assistant","model":"custom-model","content":[{"type":"text","text":"continuing"}],"stop_reason":"pause_turn"}`,
			field:            "stop_reason",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, test.response)
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
			c.Protocol = test.providerProtocol
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			path := endpoint(test.clientProtocol)
			if test.clientProtocol == "anthropic" {
				path = "/v1/messages"
			}
			req := httptest.NewRequest("POST", path, strings.NewReader(requestBody(test.clientProtocol)))
			if test.clientProtocol == "anthropic" {
				req.Header.Set("x-api-key", "local-secret")
				req.Header.Set("anthropic-version", "2023-06-01")
			} else {
				req.Header.Set("Authorization", "Bearer local-secret")
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			detail, ok := payload["error"].(map[string]any)
			if !ok || detail["param"] != test.field || detail["message"] != "unrepresentable_finish_reason" {
				t.Fatalf("error does not identify the lost field: %s", out.Body.String())
			}
		})
	}
}

func TestOpenAIUnknownFieldsAreAcceptedAndNotForwarded(t *testing.T) {
	body := `{"model":"legacy/custom-model","store":true,"dsh_plugin_packages":{"packages":["demo"]},"messages":[{"role":"user","content":[{"type":"text","text":"use a tool","client_extension":{"trace":true}}]}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"},"eager_input_streaming":true}}]}`
	for _, providerProtocol := range []string{"openai", "anthropic"} {
		t.Run(providerProtocol, func(t *testing.T) {
			var upstreamBody string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				upstreamBody = string(data)
				io.WriteString(w, responseBody(providerProtocol))
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL+"/provider/v1")
			c.Protocol = providerProtocol
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()

			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-secret")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != 200 {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
			if strings.Contains(upstreamBody, "store") || strings.Contains(upstreamBody, "dsh_plugin_packages") || strings.Contains(upstreamBody, "eager_input_streaming") || strings.Contains(upstreamBody, "client_extension") {
				t.Fatalf("ignored fields reached %s provider: %s", providerProtocol, upstreamBody)
			}
		})
	}
}

func TestValidationNeverCallsUpstream(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid request forwarded") }))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "l.db"), up.URL)
			c.Protocol = protocol
			h, closeDB, err := NewHandler(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			for _, body := range []string{`{}`, `{} {}`, `null`, `{"model":"legacy/m","model":"legacy/n"}`, `{"model":"legacy/m","messages":[{"role":"user","content":"hi"}],"stream":"invalid"}`, strings.Repeat("x", (1<<20)+1)} {
				req := httptest.NewRequest("POST", endpoint(protocol), strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer local-secret")
				out := httptest.NewRecorder()
				h.ServeHTTP(out, req)
				if out.Code != 400 && out.Code != 413 {
					t.Fatalf("invalid body accepted %d", out.Code)
				}
			}
			req := httptest.NewRequest("POST", endpoint(protocol), strings.NewReader(requestBody(protocol)))
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != 401 {
				t.Fatal("unauthorized")
			}
		})
	}
}

func TestHardBudgetRejectsBeforeUpstream(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			calls++
		}
		io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL), "openai-main")
	provider := c.Providers["openai-main"]
	provider.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: .000004, WeeklyLimit: .000004, AlertThreshold: .5, Mode: "hard"}
	provider.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	c.Providers["openai-main"] = provider
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(requestBodyFor("openai", "openai-main", "custom-model")))
		req.Header.Set("Authorization", "Bearer local-secret")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if i == 0 && out.Code != 200 {
			t.Fatalf("first=%d", out.Code)
		}
		if i == 1 && out.Code != 429 {
			t.Fatalf("second=%d %s", out.Code, out.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d", calls)
	}
}

func TestActiveBudgetsAreIsolatedBySelectedProvider(t *testing.T) {
	var callsA, callsB int
	newUpstream := func(calls *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				*calls++
			}
			io.WriteString(w, responseBody("openai"))
		}))
	}
	upA, upB := newUpstream(&callsA), newUpstream(&callsB)
	defer upA.Close()
	defer upB.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upA.URL), "provider-a")
	providerA := c.Providers["provider-a"]
	providerA.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: .000004, WeeklyLimit: .000004, AlertThreshold: .5, Mode: "hard"}
	providerA.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	providerB := providerA
	providerB.BaseURL = upB.URL
	providerB.UpstreamID = "provider-b"
	providerB.UpstreamKeychain = KeychainReference{Service: "test.provider", Account: "provider-b"}
	providerB.APIKey = "provider-secret-b"
	c.Providers["provider-a"] = providerA
	c.Providers["provider-b"] = providerB
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	handler := h.(*handler)
	send := func(providerName string) int {
		req := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(requestBodyFor("openai", providerName, "custom-model")))
		req.Header.Set("Authorization", "Bearer local-secret")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out.Code
	}
	if got := send("provider-a"); got != 200 {
		t.Fatalf("provider-a first request=%d", got)
	}
	if got := send("provider-a"); got != 429 {
		t.Fatalf("provider-a over-budget request=%d", got)
	}
	if got := send("provider-b"); got != 200 {
		t.Fatalf("provider-b request inherited provider-a budget: %d", got)
	}
	if callsA != 1 || callsB != 1 {
		t.Fatalf("upstream POST counts: provider-a=%d provider-b=%d", callsA, callsB)
	}
}

func TestActiveSessionLimitRejectsNewSessionsAndAllowsExistingSession(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
	c.MaxInFlight = 4
	c.MaxActiveSessions = 1
	h, closeDB, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	request := func(session string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(requestBody("openai")))
		req.Header.Set("Authorization", "Bearer local-secret")
		req.Header.Set("X-TideMux-Session-ID", session)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		return out
	}
	if out := request("session-a"); out.Code != 200 {
		t.Fatalf("first request = %d: %s", out.Code, out.Body.String())
	}
	rejected := request("session-b")
	if rejected.Code != 429 || rejected.Header().Get("Retry-After") != "1" || !strings.Contains(rejected.Body.String(), "active_session_limit") {
		t.Fatalf("rejected request = %d/%s/%s", rejected.Code, rejected.Header().Get("Retry-After"), rejected.Body.String())
	}
	if out := request("session-a"); out.Code != 200 {
		t.Fatalf("existing session = %d: %s", out.Code, out.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestActiveSessionConfiguredLimitEndToEnd(t *testing.T) {
	providerCalls := 0
	providerSessions := []string{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		providerSessions = append(providerSessions, r.Header.Get(adapter.SessionIDHeader))
		io.WriteString(w, responseBody("openai"))
	}))
	defer provider.Close()

	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), provider.URL)
	c.MaxInFlight = 2
	c.MaxActiveSessions = 1
	c.ActiveSessionIdleTimeoutSeconds = 1
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	gatewayServer := httptest.NewServer(h)
	defer gatewayServer.Close()

	request := func(session string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, gatewayServer.URL+endpoint("openai"), strings.NewReader(requestBody("openai")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer local-secret")
		req.Header.Set(adapter.SessionIDHeader, session)
		resp, err := gatewayServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if status := request("session-a"); status != http.StatusOK {
		t.Fatalf("first request status = %d", status)
	}
	if status := request("session-b"); status != http.StatusTooManyRequests {
		t.Fatalf("blocked request status = %d", status)
	}
	if providerCalls != 1 || len(providerSessions) != 1 || providerSessions[0] != "session-a" {
		t.Fatalf("provider calls=%d sessions=%v", providerCalls, providerSessions)
	}

	time.Sleep(1200 * time.Millisecond)
	if status := request("session-b"); status != http.StatusOK {
		t.Fatalf("request after configured idle timeout status = %d", status)
	}
	if providerCalls != 2 || len(providerSessions) != 2 || providerSessions[1] != "session-b" {
		t.Fatalf("provider calls after expiry=%d sessions=%v", providerCalls, providerSessions)
	}
}

func TestActiveSessionLimitReleasesFailedAndStreamingSessions(t *testing.T) {
	for _, scenario := range []string{"failed", "stream"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if scenario == "failed" && calls == 1 {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				if scenario == "stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
					return
				}
				io.WriteString(w, responseBody("openai"))
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
			c.MaxActiveSessions = 1
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			request := func(session string, stream bool) *httptest.ResponseRecorder {
				body := requestBody("openai")
				if stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				req := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer local-secret")
				req.Header.Set("X-TideMux-Session-ID", session)
				out := httptest.NewRecorder()
				h.ServeHTTP(out, req)
				return out
			}
			first := request("session-a", scenario == "stream")
			wantFirst := 502
			if scenario == "stream" {
				wantFirst = 200
			}
			if first.Code != wantFirst {
				t.Fatalf("first request = %d: %s", first.Code, first.Body.String())
			}
			second := request("session-b", scenario == "stream")
			if second.Code != 200 {
				t.Fatalf("second request = %d: %s", second.Code, second.Body.String())
			}
			if calls != 2 {
				t.Fatalf("upstream calls = %d, want 2", calls)
			}
		})
	}
}

func TestActiveSessionDisconnectDoesNotRetainSession(t *testing.T) {
	for _, failOnWrite := range []bool{true, false} {
		name := "flush-error"
		if failOnWrite {
			name = "write-error"
		}
		t.Run(name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, responseBody("openai"))
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
			c.MaxActiveSessions = 1
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()

			first := httptest.NewRequest(http.MethodPost, endpoint("openai"), strings.NewReader(requestBody("openai")))
			first.Header.Set("Authorization", "Bearer local-secret")
			first.Header.Set(adapter.SessionIDHeader, "session-a")
			w := &disconnectedResponseWriter{header: make(http.Header), failOnWrite: failOnWrite}
			h.ServeHTTP(w, first)
			if w.status != http.StatusOK {
				t.Fatalf("first response status = %d", w.status)
			}

			second := httptest.NewRequest(http.MethodPost, endpoint("openai"), strings.NewReader(requestBody("openai")))
			second.Header.Set("Authorization", "Bearer local-secret")
			second.Header.Set(adapter.SessionIDHeader, "session-b")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, second)
			if out.Code != http.StatusOK {
				t.Fatalf("new session after downstream disconnect = %d: %s", out.Code, out.Body.String())
			}
		})
	}
}

func TestActiveSessionCancellationReleasesSession(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	transport := gatewayRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get(adapter.SessionIDHeader) == "session-a" {
			close(upstreamStarted)
			<-r.Context().Done()
			close(upstreamCanceled)
			return nil, r.Context().Err()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(responseBody("openai"))),
			Request:    r,
		}, nil
	})
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "https://upstream.invalid/v1")
	c.MaxActiveSessions = 1
	h, closeDB, err := NewHandler(c, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	ctx, cancel := context.WithCancel(context.Background())
	first := httptest.NewRequest(http.MethodPost, endpoint("openai"), strings.NewReader(requestBody("openai"))).WithContext(ctx)
	first.Header.Set("Authorization", "Bearer local-secret")
	first.Header.Set(adapter.SessionIDHeader, "session-a")
	firstDone := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), first)
		close(firstDone)
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("canceled request did not reach upstream")
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request did not observe cancellation")
	}
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway handler did not finish after cancellation")
	}

	second := httptest.NewRequest(http.MethodPost, endpoint("openai"), strings.NewReader(requestBody("openai")))
	second.Header.Set("Authorization", "Bearer local-secret")
	second.Header.Set(adapter.SessionIDHeader, "session-b")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, second)
	if out.Code != http.StatusOK {
		t.Fatalf("new session after cancellation = %d: %s", out.Code, out.Body.String())
	}
}

type disconnectedResponseWriter struct {
	header      http.Header
	status      int
	failOnWrite bool
}

type gatewayRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f gatewayRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (w *disconnectedResponseWriter) Header() http.Header    { return w.header }
func (w *disconnectedResponseWriter) WriteHeader(status int) { w.status = status }
func (w *disconnectedResponseWriter) Write(data []byte) (int, error) {
	if w.failOnWrite {
		return 0, io.ErrClosedPipe
	}
	return len(data), nil
}
func (w *disconnectedResponseWriter) FlushError() error { return io.ErrClosedPipe }

func TestSessionIDIsForwardedToProvider(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get(adapter.SessionIDHeader); got != "provider-session" {
					t.Errorf("provider session header = %q", got)
				}
				io.WriteString(w, responseBody(protocol))
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
			c.Protocol = protocol
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			req := httptest.NewRequest("POST", endpoint(protocol), strings.NewReader(requestBody(protocol)))
			req.Header.Set("Authorization", "Bearer local-secret")
			req.Header.Set(adapter.SessionIDHeader, "provider-session")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
		})
	}
}

func testPrice() adapter.Price {
	input, output := 1.0, 1.0
	return adapter.Price{Currency: "USD", Source: "test", Version: "1", InputCacheHit: &input, InputCacheMiss: &input, Output: &output}
}

func TestBudgetRejectsUnpricedRequestedModel(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			calls++
		}
		io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL), "openai-main")
	provider := c.Providers["openai-main"]
	provider.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .5, Mode: "hard"}
	provider.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	c.Providers["openai-main"] = provider
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"openai-main/other-model","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer local-secret")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != 503 || !strings.Contains(out.Body.String(), "budget_pricing_unconfigured") {
		t.Fatalf("code=%d body=%s", out.Code, out.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls=%d", calls)
	}
}
func TestRedirectDoesNotLeakCredentials(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer up.Close()
	c := testConfig(filepath.Join(t.TempDir(), "l.db"), up.URL)
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	req := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(requestBody("openai")))
	req.Header.Set("Authorization", "Bearer local-secret")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != 502 {
		t.Fatal(out.Code)
	}
}
