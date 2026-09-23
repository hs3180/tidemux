package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentProvidersRouteByClientProtocolAndDiscoverModels(t *testing.T) {
	openAICalls, anthropicCalls := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path != "/shared/v1/models" {
				t.Errorf("model discovery path=%s", r.URL.Path)
			}
			if r.Header.Get("Authorization") == "Bearer openai-provider-key" {
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"openai-only","object":"model"}]}`)
				return
			}
			if r.Header.Get("x-api-key") == "anthropic-provider-key" && r.Header.Get("anthropic-version") == defaultAnthropicAPIVersion {
				_, _ = io.WriteString(w, `{"data":[{"id":"anthropic-only","type":"model"}],"has_more":false}`)
				return
			}
			t.Errorf("unexpected discovery credentials auth=%q x-api-key=%q version=%q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
			return
		}
		switch r.URL.Path {
		case "/shared/v1/chat/completions":
			openAICalls++
			if r.Header.Get("Authorization") != "Bearer openai-provider-key" || r.Header.Get("x-api-key") != "" {
				t.Errorf("OpenAI request auth=%q x-api-key=%q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
			}
			_, _ = io.WriteString(w, responseBody("openai"))
		case "/shared/v1/messages":
			anthropicCalls++
			if r.Header.Get("x-api-key") != "anthropic-provider-key" || r.Header.Get("Authorization") != "" {
				t.Errorf("Anthropic request x-api-key=%q auth=%q", r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, responseBody("anthropic"))
		default:
			t.Errorf("unexpected upstream path=%s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "https://legacy.example/v1")
	providerURL := upstream.URL + "/shared/v1"
	c.BaseURL = ""
	c.Protocol = ""
	c.APIVersion = ""
	c.APIKey = ""
	c.UpstreamKeychain = KeychainReference{}
	c.Model = ""
	c.UpstreamID = ""
	c.ModelCapabilities = ModelCapabilities{}
	c.Prices = nil
	c.Providers = map[string]Provider{
		"openai-main": {
			Protocol: "openai", BaseURL: providerURL,
			APIKey: "openai-provider-key",
			Model:  "openai-only", UpstreamID: "openai-provider",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"},
		},
		"anthropic-main": {
			Protocol: "anthropic", BaseURL: providerURL,
			APIKey: "anthropic-provider-key",
			Model:  "anthropic-only", UpstreamID: "anthropic-provider",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"},
		},
	}
	c.DefaultProviders = map[string]string{"openai": "openai-main", "anthropic": "anthropic-main"}
	h, closeGateway, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()

	for _, test := range []struct {
		protocol string
		want     string
	}{
		{protocol: "openai", want: "openai-only"},
		{protocol: "anthropic", want: "anthropic-only"},
	} {
		t.Run(test.protocol+" model list", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if test.protocol == "anthropic" {
				req.Header.Set("x-api-key", "local-secret")
				req.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
			} else {
				req.Header.Set("Authorization", "Bearer local-secret")
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), test.want) {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
			other := "anthropic-only"
			if test.protocol == "anthropic" {
				other = "openai-only"
			}
			if strings.Contains(out.Body.String(), other) {
				t.Fatalf("model from other endpoint was advertised: %s", out.Body.String())
			}
		})
	}

	for _, test := range []struct {
		protocol string
		model    string
	}{
		{protocol: "openai", model: "openai-only"},
		{protocol: "anthropic", model: "anthropic-only"},
	} {
		body := strings.Replace(requestBody(test.protocol), "custom-model", test.model, 1)
		path := "/v1/chat/completions"
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer local-secret")
		if test.protocol == "anthropic" {
			path = "/v1/messages"
			req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("x-api-key", "local-secret")
			req.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
		}
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if out.Code != http.StatusOK {
			t.Fatalf("%s request status=%d body=%s", test.protocol, out.Code, out.Body.String())
		}
	}
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol+" provider-specific default model", func(t *testing.T) {
			body := strings.Replace(requestBody(protocol), `"model":"custom-model",`, "", 1)
			path := "/v1/chat/completions"
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-secret")
			if protocol == "anthropic" {
				req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
				req.Header.Set("x-api-key", "local-secret")
				req.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
		})
	}
	if openAICalls != 2 || anthropicCalls != 2 {
		t.Fatalf("requests routed openai=%d anthropic=%d", openAICalls, anthropicCalls)
	}

	// A discovered model catalogue is informational by default. Without an
	// explicit supported_models allowlist, requests continue to be forwarded to
	// this protocol's configured provider even when a model is absent from GET /models.
	wrongModel := strings.Replace(requestBody("openai"), "custom-model", "anthropic-only", 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(wrongModel))
	req.Header.Set("Authorization", "Bearer local-secret")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusOK || openAICalls != 3 || anthropicCalls != 2 {
		t.Fatalf("unlisted model status=%d calls=%d/%d body=%s", out.Code, openAICalls, anthropicCalls, out.Body.String())
	}

	t.Run("explicit supported_models restricts discovery and requests", func(t *testing.T) {
		scoped := c
		scoped.LedgerPath = filepath.Join(t.TempDir(), "scoped-ledger.db")
		provider := scoped.Providers["openai-main"]
		provider.SupportedModels = []string{"openai-only"}
		scoped.Providers["openai-main"] = provider
		scopedHandler, closeScoped, err := NewHandler(scoped, upstream.Client())
		if err != nil {
			t.Fatal(err)
		}
		defer closeScoped()

		modelReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		modelReq.Header.Set("Authorization", "Bearer local-secret")
		modelOut := httptest.NewRecorder()
		scopedHandler.ServeHTTP(modelOut, modelReq)
		if modelOut.Code != http.StatusOK || !strings.Contains(modelOut.Body.String(), "openai-only") || strings.Contains(modelOut.Body.String(), "anthropic-only") {
			t.Fatalf("scoped model list status=%d body=%s", modelOut.Code, modelOut.Body.String())
		}

		callsBefore := openAICalls
		denied := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(wrongModel))
		denied.Header.Set("Authorization", "Bearer local-secret")
		deniedOut := httptest.NewRecorder()
		scopedHandler.ServeHTTP(deniedOut, denied)
		if deniedOut.Code != http.StatusNotFound || openAICalls != callsBefore {
			t.Fatalf("restricted model status=%d calls=%d/%d body=%s", deniedOut.Code, openAICalls, callsBefore, deniedOut.Body.String())
		}
	})
}

func TestMultipleSameProtocolProvidersUseConfiguredDefault(t *testing.T) {
	providerCalls := map[string]int{"primary": 0, "secondary": 0}
	providerDiscoveries := map[string]int{"primary": 0, "secondary": 0}
	newProvider := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				providerDiscoveries[name]++
				model := name + "-model"
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+model+`","object":"model"}]}`)
				return
			}
			providerCalls[name]++
			if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+name+"-key" {
				t.Errorf("%s received %s %s auth=%q", name, r.Method, r.URL.Path, r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, responseBody("openai"))
		}))
	}
	primary := newProvider("primary")
	defer primary.Close()
	secondary := newProvider("secondary")
	defer secondary.Close()

	for _, selected := range []string{"primary", "secondary"} {
		t.Run(selected, func(t *testing.T) {
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "https://legacy.example/v1")
			c.BaseURL = ""
			c.Protocol = ""
			c.APIVersion = ""
			c.APIKey = ""
			c.UpstreamKeychain = KeychainReference{}
			c.Model = ""
			c.UpstreamID = ""
			c.ModelCapabilities = ModelCapabilities{}
			c.Prices = nil
			c.Providers = map[string]Provider{
				"primary":   {Protocol: "openai", BaseURL: primary.URL + "/v1", APIKey: "primary-key", Model: "primary-model", UpstreamID: "primary", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "primary"}},
				"secondary": {Protocol: "openai", BaseURL: secondary.URL + "/v1", APIKey: "secondary-key", Model: "secondary-model", UpstreamID: "secondary", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "secondary"}},
			}
			c.DefaultProviders = map[string]string{"openai": selected}
			h, closeGateway, err := NewHandler(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeGateway()

			modelReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			modelReq.Header.Set("Authorization", "Bearer local-secret")
			modelOut := httptest.NewRecorder()
			h.ServeHTTP(modelOut, modelReq)
			if modelOut.Code != http.StatusOK || !strings.Contains(modelOut.Body.String(), selected+"-model") || strings.Contains(modelOut.Body.String(), otherProviderName(selected)+"-model") {
				t.Fatalf("model route status=%d body=%s", modelOut.Code, modelOut.Body.String())
			}

			body := strings.Replace(requestBody("openai"), "custom-model", selected+"-model", 1)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-secret")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != http.StatusOK {
				t.Fatalf("completion status=%d body=%s", out.Code, out.Body.String())
			}

			unavailable := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody("anthropic")))
			unavailable.Header.Set("x-api-key", "local-secret")
			unavailable.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
			unavailableOut := httptest.NewRecorder()
			h.ServeHTTP(unavailableOut, unavailable)
			if unavailableOut.Code != http.StatusServiceUnavailable || !strings.Contains(unavailableOut.Body.String(), "provider_not_configured") {
				t.Fatalf("unconfigured protocol status=%d body=%s", unavailableOut.Code, unavailableOut.Body.String())
			}
		})
	}
	if providerCalls["primary"] != 1 || providerCalls["secondary"] != 1 || providerDiscoveries["primary"] != 1 || providerDiscoveries["secondary"] != 1 {
		t.Fatalf("selected provider calls=%v discoveries=%v", providerCalls, providerDiscoveries)
	}
}

func otherProviderName(name string) string {
	if name == "primary" {
		return "secondary"
	}
	return "primary"
}

func TestSingleProviderFallsBackAcrossClientProtocols(t *testing.T) {
	var providerPath, providerAuth, providerBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"openai-model"}]}`)
			return
		}
		providerPath = r.URL.Path
		providerAuth = r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		providerBody = string(data)
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer upstream.Close()

	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL+"/shared/v1")
	c.Protocol = "openai"
	c.APIKey = "openai-provider-key"
	c.Model = "openai-model"
	c.UpstreamID = "openai-provider"
	h, closeGateway, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()

	body := strings.Replace(requestBody("anthropic"), "custom-model", "openai-model", 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "local-secret")
	req.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), `"type":"message"`) {
		t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
	}
	if providerPath != "/shared/v1/chat/completions" || providerAuth != "Bearer openai-provider-key" || !strings.Contains(providerBody, `"messages"`) {
		t.Fatalf("fallback path=%q auth=%q body=%s", providerPath, providerAuth, providerBody)
	}
}

func TestIndependentProvidersNativeStreamingUsesMatchingProvider(t *testing.T) {
	openAIStream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"openai\"},\"finish_reason\":\"stop\"}]}\n\n" + "data: [DONE]\n\n"
	anthropicStream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"custom-model\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	openAICalls, anthropicCalls := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.Header.Get("Authorization") == "Bearer openai-provider-key" {
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
			} else {
				_, _ = io.WriteString(w, `{"data":[{"id":"custom-model","type":"model"}],"has_more":false}`)
			}
			return
		}
		if r.URL.Path == "/shared/v1/chat/completions" {
			openAICalls++
			if r.Header.Get("Authorization") != "Bearer openai-provider-key" {
				t.Errorf("OpenAI stream auth=%q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, openAIStream)
			return
		}
		anthropicCalls++
		if r.URL.Path != "/shared/v1/messages" || r.Header.Get("x-api-key") != "anthropic-provider-key" || r.Header.Get("anthropic-version") != defaultAnthropicAPIVersion {
			t.Errorf("Anthropic stream request = %s %s x-api-key=%q version=%q", r.Method, r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicStream)
	}))
	defer upstream.Close()

	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "https://legacy.example/v1")
	providerURL := upstream.URL + "/shared/v1"
	c.BaseURL = ""
	c.Protocol = ""
	c.APIVersion = ""
	c.APIKey = ""
	c.UpstreamKeychain = KeychainReference{}
	c.Model = ""
	c.UpstreamID = ""
	c.ModelCapabilities = ModelCapabilities{}
	c.Prices = nil
	c.Providers = map[string]Provider{
		"openai-main":    {Protocol: "openai", BaseURL: providerURL, APIKey: "openai-provider-key", Model: "custom-model", UpstreamID: "openai-provider", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"}},
		"anthropic-main": {Protocol: "anthropic", BaseURL: providerURL, APIKey: "anthropic-provider-key", Model: "custom-model", UpstreamID: "anthropic-provider", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"}},
	}
	c.DefaultProviders = map[string]string{"openai": "openai-main", "anthropic": "anthropic-main"}
	h, closeGateway, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()

	for _, test := range []struct {
		protocol string
		want     string
	}{
		{protocol: "openai", want: openAIStream},
		{protocol: "anthropic", want: anthropicStream},
	} {
		t.Run(test.protocol, func(t *testing.T) {
			body := strings.TrimSuffix(requestBody(test.protocol), "}") + `,"stream":true}`
			path := "/v1/chat/completions"
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-secret")
			if test.protocol == "anthropic" {
				req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
				req.Header.Set("x-api-key", "local-secret")
				req.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != http.StatusOK || out.Header().Get("Content-Type") != "text/event-stream" || out.Body.String() != test.want {
				t.Fatalf("status=%d content-type=%q body=%q want=%q", out.Code, out.Header().Get("Content-Type"), out.Body.String(), test.want)
			}
		})
	}
	if openAICalls != 1 || anthropicCalls != 1 {
		t.Fatalf("stream requests routed openai=%d anthropic=%d", openAICalls, anthropicCalls)
	}
}

func TestIndependentProviderFailureDoesNotRetryOtherProvider(t *testing.T) {
	for _, failedProtocol := range []string{"openai", "anthropic"} {
		t.Run(failedProtocol, func(t *testing.T) {
			calls := map[string]int{"openai": 0, "anthropic": 0}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if r.Header.Get("Authorization") == "Bearer openai-provider-key" {
						_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
					} else {
						_, _ = io.WriteString(w, `{"data":[{"id":"custom-model","type":"model"}],"has_more":false}`)
					}
					return
				}
				protocol := "openai"
				if r.URL.Path == "/shared/v1/messages" {
					protocol = "anthropic"
				} else if r.URL.Path != "/shared/v1/chat/completions" {
					t.Errorf("unexpected upstream path=%s", r.URL.Path)
					return
				}
				calls[protocol]++
				if protocol == failedProtocol {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
					return
				}
				_, _ = io.WriteString(w, responseBody(protocol))
			}))
			defer upstream.Close()

			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "https://legacy.example/v1")
			providerURL := upstream.URL + "/shared/v1"
			c.BaseURL = ""
			c.Protocol = ""
			c.APIVersion = ""
			c.APIKey = ""
			c.UpstreamKeychain = KeychainReference{}
			c.Model = ""
			c.UpstreamID = ""
			c.ModelCapabilities = ModelCapabilities{}
			c.Prices = nil
			c.Providers = map[string]Provider{
				"openai-main":    {Protocol: "openai", BaseURL: providerURL, APIKey: "openai-provider-key", Model: "custom-model", UpstreamID: "openai-provider", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"}},
				"anthropic-main": {Protocol: "anthropic", BaseURL: providerURL, APIKey: "anthropic-provider-key", Model: "custom-model", UpstreamID: "anthropic-provider", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"}},
			}
			c.DefaultProviders = map[string]string{"openai": "openai-main", "anthropic": "anthropic-main"}
			h, closeGateway, err := NewHandler(c, upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeGateway()

			path := "/v1/chat/completions"
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(requestBody(failedProtocol)))
			req.Header.Set("Authorization", "Bearer local-secret")
			if failedProtocol == "anthropic" {
				req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody(failedProtocol)))
				req.Header.Set("x-api-key", "local-secret")
				req.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != http.StatusBadGateway || calls[failedProtocol] != 1 || calls[otherProtocol(failedProtocol)] != 0 {
				t.Fatalf("status=%d upstream calls=%v body=%s", out.Code, calls, out.Body.String())
			}
		})
	}
}

func otherProtocol(protocol string) string {
	if protocol == "openai" {
		return "anthropic"
	}
	return "openai"
}
