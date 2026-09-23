package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectProviderProtocolFromModels(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		want       string
		wantBearer bool
	}{
		{name: "openai", body: `{"object":"list","data":[{"id":"model","object":"model"}]}`, want: "openai", wantBearer: true},
		{name: "anthropic", body: `{"data":[{"type":"model","id":"model","display_name":"Model"}]}`, want: "anthropic", wantBearer: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/models" {
					t.Fatalf("path=%s", r.URL.Path)
				}
				if test.wantBearer {
					if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("x-api-key") != "" {
						t.Fatal("OpenAI probe authentication was not used")
					}
				} else if calls == 1 {
					if r.Header.Get("Authorization") != "Bearer provider-secret" {
						t.Fatal("first probe should try the OpenAI authentication shape")
					}
					w.WriteHeader(http.StatusUnauthorized)
					return
				} else if r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != defaultAnthropicAPIVersion {
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
			if !test.wantBearer && calls != 2 {
				t.Fatalf("probe calls=%d, want 2", calls)
			}
		})
	}
}

func TestProviderProtocolHintAvoidsProbe(t *testing.T) {
	for _, test := range []struct {
		baseURL string
		want    string
	}{
		{baseURL: "https://api.openai.com/v1", want: "openai"},
		{baseURL: "https://api.anthropic.com/v1", want: "anthropic"},
		{baseURL: "https://api.deepseek.com", want: "openai"},
		{baseURL: "https://api.deepseek.com/anthropic/v1", want: "anthropic"},
	} {
		if got := providerProtocolHint(test.baseURL); got != test.want {
			t.Fatalf("hint(%q)=%q, want %q", test.baseURL, got, test.want)
		}
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
		_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"custom-model","display_name":"Custom Model"}]}`))
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
			endpoint := Provider{BaseURL: server.URL + "/v1", APIKey: "endpoint-secret", APIVersion: "2024-01-01"}
			models, known := discoverProviderModels(endpoint, test.protocol, server.Client())
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

	models, known := discoverProviderModels(Provider{BaseURL: server.URL + "/v1", APIKey: "endpoint-secret"}, "openai", server.Client())
	if known || len(models) != 0 {
		t.Fatalf("partial model page was treated as authoritative: models=%v known=%v", models, known)
	}
}
