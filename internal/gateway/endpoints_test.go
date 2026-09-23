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
	openAIUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path != "/openai/v1/models" || r.Header.Get("Authorization") != "Bearer openai-provider-key" {
				t.Errorf("OpenAI model discovery request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"openai-only","object":"model"}]}`)
			return
		}
		openAICalls++
		if r.URL.Path != "/openai/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer openai-provider-key" || r.Header.Get("x-api-key") != "" {
			t.Errorf("OpenAI request = %s %s auth=%q x-api-key=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer openAIUpstream.Close()
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path != "/anthropic/v1/models" || r.Header.Get("x-api-key") != "anthropic-provider-key" || r.Header.Get("anthropic-version") != defaultAnthropicAPIVersion {
				t.Errorf("Anthropic model discovery request = %s %s x-api-key=%q version=%q", r.Method, r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"anthropic-only","type":"model"}],"has_more":false}`)
			return
		}
		anthropicCalls++
		if r.URL.Path != "/anthropic/v1/messages" || r.Header.Get("x-api-key") != "anthropic-provider-key" || r.Header.Get("Authorization") != "" {
			t.Errorf("Anthropic request = %s %s x-api-key=%q auth=%q", r.Method, r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, responseBody("anthropic"))
	}))
	defer anthropicUpstream.Close()

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
		"openai": {
			BaseURL: openAIUpstream.URL + "/openai/v1", APIKey: "openai-provider-key",
			Model: "openai-only", UpstreamID: "openai-provider",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"},
		},
		"anthropic": {
			BaseURL: anthropicUpstream.URL + "/anthropic/v1", APIKey: "anthropic-provider-key",
			Model: "anthropic-only", UpstreamID: "anthropic-provider",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"},
		},
	}
	h, closeGateway, err := NewHandler(c, openAIUpstream.Client())
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

	wrongModel := strings.Replace(requestBody("openai"), "custom-model", "anthropic-only", 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(wrongModel))
	req.Header.Set("Authorization", "Bearer local-secret")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusNotFound || openAICalls != 2 || anthropicCalls != 2 {
		t.Fatalf("unavailable model status=%d calls=%d/%d body=%s", out.Code, openAICalls, anthropicCalls, out.Body.String())
	}
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
		"openai": {
			BaseURL: upstream.URL + "/openai/v1", APIKey: "openai-provider-key",
			Model: "openai-model", UpstreamID: "openai-provider",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"},
		},
	}
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
	if providerPath != "/openai/v1/chat/completions" || providerAuth != "Bearer openai-provider-key" || !strings.Contains(providerBody, `"messages"`) {
		t.Fatalf("fallback path=%q auth=%q body=%s", providerPath, providerAuth, providerBody)
	}
}

func TestIndependentProvidersNativeStreamingUsesMatchingProvider(t *testing.T) {
	openAIStream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"openai\"},\"finish_reason\":\"stop\"}]}\n\n" + "data: [DONE]\n\n"
	anthropicStream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"custom-model\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	openAICalls, anthropicCalls := 0, 0
	openAIUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
			return
		}
		openAICalls++
		if r.URL.Path != "/openai/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer openai-provider-key" {
			t.Errorf("OpenAI stream request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openAIStream)
	}))
	defer openAIUpstream.Close()
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"custom-model","type":"model"}],"has_more":false}`)
			return
		}
		anthropicCalls++
		if r.URL.Path != "/anthropic/v1/messages" || r.Header.Get("x-api-key") != "anthropic-provider-key" || r.Header.Get("anthropic-version") != defaultAnthropicAPIVersion {
			t.Errorf("Anthropic stream request = %s %s x-api-key=%q version=%q", r.Method, r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicStream)
	}))
	defer anthropicUpstream.Close()

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
		"openai":    {BaseURL: openAIUpstream.URL + "/openai/v1", APIKey: "openai-provider-key", Model: "custom-model", UpstreamID: "openai-provider", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"}},
		"anthropic": {BaseURL: anthropicUpstream.URL + "/anthropic/v1", APIKey: "anthropic-provider-key", Model: "custom-model", UpstreamID: "anthropic-provider", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"}},
	}
	h, closeGateway, err := NewHandler(c, openAIUpstream.Client())
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
			openAIUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
					return
				}
				calls["openai"]++
				if failedProtocol == "openai" {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
					return
				}
				_, _ = io.WriteString(w, responseBody("openai"))
			}))
			defer openAIUpstream.Close()
			anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"data":[{"id":"custom-model","type":"model"}],"has_more":false}`)
					return
				}
				calls["anthropic"]++
				if failedProtocol == "anthropic" {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
					return
				}
				_, _ = io.WriteString(w, responseBody("anthropic"))
			}))
			defer anthropicUpstream.Close()

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
				"openai":    {BaseURL: openAIUpstream.URL + "/openai/v1", APIKey: "openai-provider-key", Model: "custom-model", UpstreamID: "openai-provider", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"}},
				"anthropic": {BaseURL: anthropicUpstream.URL + "/anthropic/v1", APIKey: "anthropic-provider-key", Model: "custom-model", UpstreamID: "anthropic-provider", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"}},
			}
			h, closeGateway, err := NewHandler(c, openAIUpstream.Client())
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
