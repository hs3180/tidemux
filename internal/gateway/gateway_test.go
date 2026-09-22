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
func requestBody(protocol string) string {
	if protocol == "anthropic" {
		return `{"model":"custom-model","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	}
	return `{"model":"custom-model","messages":[{"role":"user","content":"hello"}]}`
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
	body := `{"model":"custom-model","max_tokens":16,"store":true,"messages":[{"role":"user","content":"use a tool"}],"tools":[{"name":"read_file","input_schema":{"type":"object"},"eager_input_streaming":true}]}`
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

func TestValidationErrorIdentifiesParameter(t *testing.T) {
	for _, test := range []struct {
		name, protocol, path, body string
	}{
		{
			name:     "openai",
			protocol: "openai",
			path:     "/v1/chat/completions",
			body:     `{"model":"custom-model","messages":[{"role":"user","content":"hi"}],"temperature":"invalid"}`,
		},
		{
			name:     "anthropic",
			protocol: "anthropic",
			path:     "/v1/messages",
			body:     `{"model":"custom-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"temperature":"invalid"}`,
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

func TestOpenAIUnknownFieldsAreAcceptedAndNotForwarded(t *testing.T) {
	body := `{"model":"custom-model","store":true,"messages":[{"role":"user","content":"use a tool"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"},"eager_input_streaming":true}}]}`
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
			if strings.Contains(upstreamBody, "store") || strings.Contains(upstreamBody, "eager_input_streaming") {
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
			for _, body := range []string{`{}`, `{} {}`, `null`, `{"model":"m","model":"n"}`, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":"invalid"}`, strings.Repeat("x", (1<<20)+1)} {
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
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; io.WriteString(w, responseBody("openai")) }))
	defer up.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
	c.Budget = ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .5, Mode: "hard"}
	c.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(requestBody("openai")))
		req.Header.Set("Authorization", "Bearer local-secret")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if i == 0 && out.Code != 200 {
			t.Fatalf("first=%d", out.Code)
		}
		if i == 1 && out.Code != 200 {
			t.Fatalf("second=%d %s", out.Code, out.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d", calls)
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
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; io.WriteString(w, responseBody("openai")) }))
	defer up.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
	c.Budget = ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .5, Mode: "hard"}
	c.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"other-model","messages":[{"role":"user","content":"hello"}]}`
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
