package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
