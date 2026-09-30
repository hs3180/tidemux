package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func addRoutingProvider(c *Config, ref, baseURL, protocol string, models ...string) {
	if c.Providers == nil {
		c.Providers = make(map[string]Provider)
	}
	c.Providers[ref] = Provider{
		Protocol: protocol, BaseURL: baseURL + "/v1", UpstreamID: ref,
		APIKey:           "provider-key-" + ref,
		UpstreamKeychain: KeychainReference{Service: "test.provider", Account: ref},
		SupportedModels:  models,
	}
}

func routingTestConfig(path string) Config {
	c := testConfig(path, "https://legacy.example/v1")
	c.Protocol, c.BaseURL, c.APIVersion, c.APIKey = "", "", "", ""
	c.UpstreamKeychain, c.UpstreamID, c.Model = KeychainReference{}, "", ""
	c.Providers = nil
	return c
}

func configureRouting(c *Config, settings RoutingConfig) {
	if settings == (RoutingConfig{}) {
		c.Routing = nil
		return
	}
	c.Routing = &settings
}

func routingRequest(t *testing.T, h http.Handler, protocol, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/chat/completions"
	if protocol == "anthropic" {
		path = "/v1/messages"
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if protocol == "anthropic" {
		req.Header.Set("x-api-key", "local-secret")
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer local-secret")
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	return response
}

func openAIResponseForModel(model string) string {
	return strings.Replace(responseBody("openai"), `"model":"custom-model"`, `"model":"`+model+`"`, 1)
}

func TestRandomSharedModelRoutingChoosesOneEligibleProvider(t *testing.T) {
	callsA, callsB := 0, 0
	newUpstream := func(calls *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"}]}`)
				return
			}
			if r.Method == http.MethodPost {
				*calls++
			}
			_, _ = io.WriteString(w, openAIResponseForModel("shared-model"))
		}))
	}
	upA, upB := newUpstream(&callsA), newUpstream(&callsB)
	defer upA.Close()
	defer upB.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", upA.URL, "openai", "shared-model")
	addRoutingProvider(&c, "b", upB.URL, "openai", "shared-model")
	configureRouting(&c, RoutingConfig{SharedModelStrategy: "random"})
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	h.(*handler).randomIndex = func(size int) int {
		if size != 2 {
			t.Errorf("random choice size=%d", size)
		}
		return 1
	}
	response := routingRequest(t, h, "openai", `{"model":"shared-model","messages":[{"role":"user","content":"hello"}]}`)
	if response.Code != http.StatusOK || callsA != 0 || callsB != 1 {
		t.Fatalf("status=%d calls=%d/%d body=%s", response.Code, callsA, callsB, response.Body.String())
	}
}

func TestNativeAnthropicToolsRouteOnlyToAnthropicProvider(t *testing.T) {
	openAICalls, anthropicCalls := 0, 0
	openAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"}]}`)
			return
		}
		openAICalls++
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer openAI.Close()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"shared-model","type":"model"}],"has_more":false}`)
			return
		}
		anthropicCalls++
		_, _ = io.WriteString(w, responseBody("anthropic"))
	}))
	defer anthropic.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a-openai", openAI.URL, "openai", "shared-model")
	addRoutingProvider(&c, "b-anthropic", anthropic.URL, "anthropic", "shared-model")
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"shared-model","max_tokens":32,"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":2}]}`
	response := routingRequest(t, h, "anthropic", body)
	if response.Code != http.StatusOK || openAICalls != 0 || anthropicCalls != 1 {
		t.Fatalf("status=%d calls openai=%d anthropic=%d body=%s", response.Code, openAICalls, anthropicCalls, response.Body.String())
	}
}
