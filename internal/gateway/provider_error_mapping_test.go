package gateway

import (
	"encoding/json"
	"github.com/hs3180/tidemux/internal/adapter"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestMappedProviderFailureCategoriesStayDistinctForClientProtocols(t *testing.T) {
	tests := []struct {
		name          string
		upstreamCode  string
		category      adapter.ProviderErrorCategory
		upstreamHTTP  int
		wantHTTP      int
		wantCode      string
		openAIType    string
		anthropicType string
		messageHint   string
	}{
		{name: "billing", upstreamCode: "credit_empty", category: adapter.ProviderErrorInsufficientBalance, upstreamHTTP: 403, wantHTTP: 403, wantCode: "provider_insufficient_balance", openAIType: "insufficient_quota", anthropicType: "billing_error", messageHint: "Check its account balance or billing status"},
		{name: "policy", upstreamCode: "policy_blocked", category: adapter.ProviderErrorPolicyDenied, upstreamHTTP: 403, wantHTTP: 403, wantCode: "upstream_policy_denied", openAIType: "permission_error", anthropicType: "permission_error", messageHint: "usage or content policy"},
		{name: "permission", upstreamCode: "model_forbidden", category: adapter.ProviderErrorPermissionDenied, upstreamHTTP: 403, wantHTTP: 403, wantCode: "upstream_permission_denied", openAIType: "permission_error", anthropicType: "permission_error", messageHint: "provider-side permissions"},
		{name: "model not found", upstreamCode: "model_missing", category: adapter.ProviderErrorModelNotFound, upstreamHTTP: 404, wantHTTP: 404, wantCode: "provider_model_not_found", openAIType: "not_found_error", anthropicType: "not_found_error", messageHint: "selected model"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.upstreamHTTP)
				_, _ = io.WriteString(w, `{"error":{"code":"`+test.upstreamCode+`","message":"private account diagnostic"}}`)
			}))
			defer upstream.Close()

			c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL+"/v1"), "p")
			provider := c.Providers["p"]
			provider.Protocol = "openai"
			provider.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: test.upstreamCode, HTTPStatus: test.upstreamHTTP, Category: test.category}}
			c.Providers["p"] = provider
			for _, clientProtocol := range []string{"openai", "anthropic"} {
				t.Run(clientProtocol, func(t *testing.T) {
					// Each assertion observes a first call. Subsequent calls
					// correctly share cooldowns across client protocols.
					current := c
					current.LedgerPath = filepath.Join(t.TempDir(), "ledger.db")
					h, closeDB, err := NewHandler(current, upstream.Client())
					if err != nil {
						t.Fatal(err)
					}
					defer closeDB()
					path := "/v1/chat/completions"
					if clientProtocol == "anthropic" {
						path = "/v1/messages"
					}
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(requestBodyFor(clientProtocol, "p", "custom-model")))
					if clientProtocol == "anthropic" {
						req.Header.Set("x-api-key", "local-secret")
						req.Header.Set("anthropic-version", "2023-06-01")
					} else {
						req.Header.Set("Authorization", "Bearer local-secret")
					}
					out := httptest.NewRecorder()
					h.ServeHTTP(out, req)
					var payload struct {
						Type  string `json:"type"`
						Error struct {
							Type         string `json:"type"`
							Code         string `json:"code"`
							ProviderCode string `json:"provider_code"`
							Message      string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
						t.Fatal(err)
					}
					wantType := test.openAIType
					if clientProtocol == "anthropic" {
						wantType = test.anthropicType
					}
					if out.Code != test.wantHTTP || payload.Error.Code != test.wantCode || payload.Error.ProviderCode != test.upstreamCode || payload.Error.Type != wantType || !strings.Contains(payload.Error.Message, test.messageHint) || strings.Contains(out.Body.String(), "private account diagnostic") {
						t.Fatalf("status=%d payload=%s", out.Code, out.Body.String())
					}
					if clientProtocol == "anthropic" && payload.Type != "error" {
						t.Fatalf("Anthropic envelope type=%q body=%s", payload.Type, out.Body.String())
					}
					if strings.TrimSpace(payload.Error.Message) == "" {
						t.Fatalf("missing actionable message: %s", out.Body.String())
					}
				})
			}
		})
	}
}

func TestMapped429RetriesOnlySelectedProviderProfile(t *testing.T) {
	var callsA, callsB int
	newUpstream := func(calls *int, rateLimited bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model","object":"model"}]}`)
				return
			}
			*calls++
			if rateLimited {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"code":"profile_throttled","message":"private rate-limit detail"}}`)
				return
			}
			_, _ = io.WriteString(w, responseBody("openai"))
		}))
	}
	upA := newUpstream(&callsA, true)
	upB := newUpstream(&callsB, false)
	defer upA.Close()
	defer upB.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upA.URL+"/v1"), "provider-a")
	providerA := c.Providers["provider-a"]
	providerA.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "profile_throttled", HTTPStatus: 429, Category: adapter.ProviderErrorRateLimited}}
	providerB := providerA
	providerB.BaseURL = upB.URL + "/v1"
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
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBodyFor("openai", "provider-a", "custom-model")))
	req.Header.Set("Authorization", "Bearer local-secret")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	var payload map[string]any
	if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	detail, _ := payload["error"].(map[string]any)
	if out.Code != http.StatusTooManyRequests || detail["code"] != "upstream_rate_limited" || out.Header().Get("Retry-After") != "0" || callsA != 3 || callsB != 0 {
		t.Fatalf("status=%d retry-after=%q callsA=%d callsB=%d body=%s", out.Code, out.Header().Get("Retry-After"), callsA, callsB, out.Body.String())
	}
}
