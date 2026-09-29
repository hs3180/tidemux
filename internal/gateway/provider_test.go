package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/observability"
)

func TestProviderErrorMappingsAreValidatedAndRoundTrip(t *testing.T) {
	config := Config{
		ListenAddr:          "127.0.0.1:4000",
		MaxInFlight:         1,
		LedgerPath:          filepath.Join(t.TempDir(), "ledger.db"),
		AccessTokenKeychain: KeychainReference{Service: "gateway", Account: "local"},
		Providers: map[string]Provider{
			"provider": {
				Protocol:         "openai",
				BaseURL:          "https://provider.example/v1",
				UpstreamID:       "provider",
				UpstreamKeychain: KeychainReference{Service: "provider", Account: "main"},
				ErrorCodeMappings: []adapter.ProviderErrorMapping{{
					UpstreamCode: "balance_low", HTTPStatus: 402, Category: adapter.ProviderErrorInsufficientBalance,
				}},
			},
		},
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded.Providers["provider"].ErrorCodeMappings
	if len(got) != 1 || got[0] != config.Providers["provider"].ErrorCodeMappings[0] {
		t.Fatalf("round-trip mappings=%+v", got)
	}

	for _, mappings := range [][]adapter.ProviderErrorMapping{
		{{UpstreamCode: "balance_low", Category: adapter.ProviderErrorInsufficientBalance}, {UpstreamCode: "balance_low", Category: adapter.ProviderErrorRateLimited}},
		{{UpstreamCode: "balance_low", HTTPStatus: 399, Category: adapter.ProviderErrorInsufficientBalance}},
		{{UpstreamCode: "balance_low", Category: "arbitrary"}},
	} {
		provider := config.Providers["provider"]
		provider.ErrorCodeMappings = mappings
		config.Providers["provider"] = provider
		if err := config.Validate(); err == nil {
			t.Fatalf("invalid mappings accepted: %+v", mappings)
		}
	}
}

func TestDetectProviderProtocolFromModels(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		want       string
		wantBearer bool
	}{
		{name: "openai", body: `{"object":"list","data":[{"id":"model","object":"model"}]}`, want: "openai", wantBearer: true},
		{name: "anthropic", body: `{"data":[{"type":"model","id":"model","display_name":"Model"}],"has_more":false}`, want: "anthropic", wantBearer: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/models" {
					t.Fatalf("path=%s", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "" {
					if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("x-api-key") != "" {
						t.Fatal("OpenAI probe authentication was not used")
					}
					if !test.wantBearer {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
				} else if r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != defaultAnthropicAPIVersion || r.Header.Get("Authorization") != "" {
					t.Fatal("Anthropic probe authentication was not used")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			got, err := detectProviderProtocol(context.Background(), server.URL+"/v1", "provider-secret", "", server.Client())
			if err != nil || got != test.want {
				t.Fatalf("protocol=%q err=%v, want %q", got, err, test.want)
			}
			if calls != 2 {
				t.Fatalf("probe calls=%d, want both protocol probes", calls)
			}
		})
	}
}

func TestProviderURLNamesDoNotOverrideObservedProtocol(t *testing.T) {
	for _, endpoint := range []string{"/anthropic/v1", "/openai/v1", "/deepseek/v1"} {
		t.Run(endpoint, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model","object":"model"}]}`))
			}))
			defer server.Close()

			protocol, err := detectProviderProtocol(context.Background(), server.URL+endpoint, "provider-secret", "", server.Client())
			if err != nil || protocol != "openai" {
				t.Fatalf("protocol=%q err=%v, want openai from the response schema", protocol, err)
			}
			if calls != 2 {
				t.Fatalf("probe calls=%d, want both protocol probes", calls)
			}
		})
	}
}

func TestProviderProtocolDetectionRejectsAmbiguousAndInvalidEvidence(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		contentType  string
		body         string
		splitSchemas bool
	}{
		{name: "both protocol candidates confirmed", contentType: "application/json", splitSchemas: true},
		{name: "unknown schema", contentType: "application/json", body: `{"data":[{"id":"model","type":"other"}]}`},
		{name: "missing data array", contentType: "application/json", body: `{"object":"list"}`},
		{name: "non json media type", contentType: "text/plain", body: `{"object":"list","data":[{"id":"model","object":"model"}]}`},
		{name: "unavailable endpoint", status: http.StatusServiceUnavailable, contentType: "application/json", body: `{"object":"list","data":[{"id":"model","object":"model"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/models" {
					t.Errorf("unexpected probe path %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", test.contentType)
				body := test.body
				if test.splitSchemas {
					body = `{"object":"list","data":[{"id":"model","object":"model"}]}`
					if r.Header.Get("Authorization") == "" {
						body = `{"data":[{"id":"model","type":"model"}],"has_more":false}`
					}
				}
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			protocol, err := detectProviderProtocol(context.Background(), server.URL+"/v1", "provider-secret", "", server.Client())
			if protocol != "" || err == nil || !strings.Contains(err.Error(), "set --protocol") {
				t.Fatalf("protocol=%q err=%v", protocol, err)
			}
			if calls != 2 {
				t.Fatalf("probe calls=%d, want both credential shapes", calls)
			}
		})
	}
}

func TestProviderProtocolProbeDoesNotFollowRedirects(t *testing.T) {
	redirectCalls := 0
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectCalls++
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			t.Errorf("provider credentials reached redirect target: Authorization=%q x-api-key=%q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model","object":"model"}]}`))
	}))
	defer redirectTarget.Close()
	probeCalls := 0
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalls++
		w.Header().Set("Location", redirectTarget.URL+"/v1/models")
		w.WriteHeader(http.StatusFound)
	}))
	defer probe.Close()

	if protocol, err := detectProviderProtocol(context.Background(), probe.URL+"/v1", "provider-secret", "", probe.Client()); err == nil || protocol != "" {
		t.Fatalf("redirect selected a protocol %q (err=%v)", protocol, err)
	}
	if probeCalls != 2 || redirectCalls != 0 {
		t.Fatalf("probe calls=%d redirect calls=%d", probeCalls, redirectCalls)
	}
}

func TestInspectProviderEndpointDetectsProtocolAndReturnsModels(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/models" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != defaultAnthropicAPIVersion {
			t.Errorf("Anthropic auth headers = %q / %q", r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"z-model"},{"type":"model","id":"a-model"}],"has_more":false}`))
	}))
	defer server.Close()

	info, err := InspectProviderEndpoint(context.Background(), server.URL+"/v1", "provider-secret", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if info.Protocol != "anthropic" || !info.ModelsKnown || strings.Join(info.Models, ",") != "a-model,z-model" {
		t.Fatalf("endpoint info = %+v", info)
	}
	if calls != 2 {
		t.Fatalf("probe calls=%d, want two authentication attempts", calls)
	}
}

func TestDiscoverProviderModelsCanUseAnExplicitProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("x-api-key") != "" {
			t.Errorf("auth headers = %q / %q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"custom-model"}]}`))
	}))
	defer server.Close()

	models, known := DiscoverProviderModels(server.URL+"/v1", "provider-secret", "", "openai", server.Client())
	if !known || strings.Join(models, ",") != "custom-model" {
		t.Fatalf("models=%v known=%v", models, known)
	}
}

func TestResolveNamedProviderAutomaticallyDetectsProtocol(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != defaultAnthropicAPIVersion {
			t.Fatal("automatic Anthropic probe did not use the configured key and default version")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"model","display_name":"Model"}],"has_more":false}`))
	}))
	defer server.Close()

	providers, err := resolveProviders(Config{
		Providers: map[string]Provider{
			"auto-provider": {Protocol: "auto", BaseURL: server.URL + "/v1", APIKey: "provider-secret"},
		},
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if providers["auto-provider"].Protocol != "anthropic" || providers["auto-provider"].APIVersion != defaultAnthropicAPIVersion {
		t.Fatalf("resolved provider = %+v", providers["auto-provider"])
	}
	if calls != 2 {
		t.Fatalf("probe calls = %d, want OpenAI and Anthropic auth attempts", calls)
	}
}

func TestResolveNamedProviderProtocolOverrideSkipsDetection(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	providers, err := resolveProviders(Config{
		Providers: map[string]Provider{
			"forced-openai": {Protocol: "openai", BaseURL: server.URL + "/v1", APIKey: "provider-secret"},
		},
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if providers["forced-openai"].Protocol != "openai" || calls != 0 {
		t.Fatalf("providers=%+v probe calls=%d", providers, calls)
	}
}

func TestResolveNamedProvidersAcceptMultipleProfilesForOneProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model","object":"model"}]}`))
	}))
	defer server.Close()

	providers, err := resolveProviders(Config{
		Providers: map[string]Provider{
			"first":  {Protocol: "auto", BaseURL: server.URL + "/v1", APIKey: "key-1"},
			"second": {Protocol: "auto", BaseURL: server.URL + "/v1", APIKey: "key-2"},
		},
	}, server.Client())
	if err != nil || len(providers) != 2 {
		t.Fatalf("providers=%v error=%v", providers, err)
	}
}

func TestNewHandlerAutoDetectsProviderProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "Bearer provider-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != defaultAnthropicAPIVersion {
			t.Fatal("missing Anthropic discovery authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"custom-model","display_name":"Custom Model"}],"has_more":false}`))
	}))
	defer server.Close()

	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), server.URL+"/v1")
	c.Protocol = ""
	c.APIVersion = ""
	h, closeGateway, err := NewHandler(c, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	resolved := h.(*handler).config
	if resolved.Protocol != "anthropic" || resolved.APIVersion != defaultAnthropicAPIVersion {
		t.Fatalf("resolved provider=%q version=%q", resolved.Protocol, resolved.APIVersion)
	}
}

func TestNewHandlerKeepsResolvedProvidersAvailableWhenOneAutoProbeFails(t *testing.T) {
	failedProbeCalls := 0
	unresolved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request to unresolved provider: %s %s", r.Method, r.URL.Path)
		}
		failedProbeCalls++
		_, _ = io.WriteString(w, `{"notice":"probe-body-sensitive-marker","data":[{"id":"unresolved-model"}]}`)
	}))
	defer unresolved.Close()
	resolvedCalls := 0
	resolved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"ready-model","object":"model"}]}`)
			return
		}
		resolvedCalls++
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer resolved.Close()

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
		"ready":      {Protocol: "openai", BaseURL: resolved.URL + "/v1", APIKey: "ready-key", UpstreamID: "ready", SupportedModels: []string{"ready-model"}, UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "ready"}},
		"unresolved": {Protocol: "auto", BaseURL: unresolved.URL + "/v1", APIKey: "unresolved-key", UpstreamID: "unresolved", SupportedModels: []string{"unresolved-model"}, UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "unresolved"}},
	}
	var logs bytes.Buffer
	h, closeGateway, err := NewHandlerWithLogger(c, nil, observability.JSONLogger(&logs))
	if err != nil {
		t.Fatalf("one unresolved provider prevented gateway startup: %v", err)
	}
	defer closeGateway()
	if !strings.Contains(logs.String(), `"event":"provider_unavailable"`) || !strings.Contains(logs.String(), `"provider_ref":"unresolved"`) || !strings.Contains(logs.String(), "tidemux provider update REF --protocol") {
		t.Fatalf("missing structured provider recovery event: %s", logs.String())
	}
	if strings.Contains(logs.String(), "probe-body-sensitive-marker") || strings.Contains(logs.String(), "unresolved-key") {
		t.Fatalf("provider probe event exposed sensitive data: %s", logs.String())
	}

	models := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	models.Header.Set("Authorization", "Bearer local-secret")
	modelsOut := httptest.NewRecorder()
	h.ServeHTTP(modelsOut, models)
	if modelsOut.Code != http.StatusOK || !strings.Contains(modelsOut.Body.String(), "ready/ready-model") || strings.Contains(modelsOut.Body.String(), "unresolved/unresolved-model") {
		t.Fatalf("models status=%d body=%s", modelsOut.Code, modelsOut.Body.String())
	}

	ready := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBodyFor("openai", "ready", "ready-model")))
	ready.Header.Set("Authorization", "Bearer local-secret")
	readyOut := httptest.NewRecorder()
	h.ServeHTTP(readyOut, ready)
	if readyOut.Code != http.StatusOK || resolvedCalls != 1 {
		t.Fatalf("resolved provider status=%d calls=%d body=%s", readyOut.Code, resolvedCalls, readyOut.Body.String())
	}

	blocked := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBodyFor("openai", "unresolved", "unresolved-model")))
	blocked.Header.Set("Authorization", "Bearer local-secret")
	blockedOut := httptest.NewRecorder()
	h.ServeHTTP(blockedOut, blocked)
	if blockedOut.Code != http.StatusServiceUnavailable || !strings.Contains(blockedOut.Body.String(), "provider_unavailable") {
		t.Fatalf("unresolved provider status=%d body=%s", blockedOut.Code, blockedOut.Body.String())
	}
	if failedProbeCalls != 2 {
		t.Fatalf("failed protocol probe calls=%d, want two auth shapes", failedProbeCalls)
	}
}

func TestNewHandlerRejectsWhenAllNamedProvidersFailAutoProbe(t *testing.T) {
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"not":"a recognized model catalog"}`)
	}))
	defer probe.Close()
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
		"unresolved": {Protocol: "auto", BaseURL: probe.URL + "/v1", APIKey: "provider-key", UpstreamID: "unresolved", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "unresolved"}},
	}
	if _, _, err := NewHandler(c, nil); err == nil || !strings.Contains(err.Error(), "no provider protocol could be resolved for unresolved") || !strings.Contains(err.Error(), "tidemux provider update REF --protocol") {
		t.Fatalf("handler error=%v, want actionable all-providers-failed error", err)
	}
}

func TestDiscoverProviderModelsUsesEndpointProtocolAndAuth(t *testing.T) {
	for _, test := range []struct {
		protocol string
		body     string
		wantAuth string
		want     string
	}{
		{protocol: "openai", body: `{"object":"list","data":[{"id":"z-model"},{"id":"a-model"},{"id":"a-model"}]}`, wantAuth: "Bearer endpoint-secret", want: "a-model,z-model"},
		{protocol: "anthropic", body: `{"data":[{"id":"z-model","type":"model"},{"id":"a-model","type":"model"}],"has_more":false}`, wantAuth: "x-api-key:endpoint-secret", want: "a-model,z-model"},
	} {
		t.Run(test.protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("path=%s", r.URL.Path)
				}
				if test.protocol == "openai" {
					if r.Header.Get("Authorization") != test.wantAuth || r.Header.Get("x-api-key") != "" {
						t.Errorf("OpenAI headers = %q / %q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
					}
				} else if r.Header.Get("x-api-key") != "endpoint-secret" || r.Header.Get("anthropic-version") != "2024-01-01" || r.Header.Get("Authorization") != "" {
					t.Errorf("Anthropic headers = %q / %q / %q", r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.Header.Get("Authorization"))
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			endpoint := Provider{APIKey: "endpoint-secret", APIVersion: "2024-01-01"}
			models, known := discoverProviderModels(server.URL+"/v1", endpoint, test.protocol, server.Client())
			if !known || strings.Join(models, ",") != test.want {
				t.Fatalf("models=%v known=%v", models, known)
			}
		})
	}
}

func TestDiscoverProviderModelsDoesNotTrustPartialPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"first-page-only"}],"has_more":true}`))
	}))
	defer server.Close()

	models, known := discoverProviderModels(server.URL+"/v1", Provider{APIKey: "endpoint-secret"}, "openai", server.Client())
	if known || len(models) != 0 {
		t.Fatalf("partial model page was treated as authoritative: models=%v known=%v", models, known)
	}
}
