package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderProtocolDetectionIgnoresURLNamesAndUsesWireBehavior(t *testing.T) {
	tests := []struct {
		name       string
		basePath   string
		wireFormat string
	}{
		{name: "anthropic path implements OpenAI", basePath: "/anthropic/v1", wireFormat: "openai"},
		{name: "openai path implements Anthropic", basePath: "/openai/v1", wireFormat: "anthropic"},
		{name: "DeepSeek Anthropic path", basePath: "/deepseek/anthropic", wireFormat: "anthropic"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != strings.TrimRight(test.basePath, "/")+"/models" || r.Method != http.MethodGet {
					t.Errorf("unexpected probe request %s %s", r.Method, r.URL.Path)
				}
				openAIAuth := r.Header.Get("Authorization") == "Bearer provider-secret" && r.Header.Get("x-api-key") == ""
				anthropicAuth := r.Header.Get("x-api-key") == "provider-secret" && r.Header.Get("Authorization") == ""
				if !openAIAuth && !anthropicAuth {
					t.Errorf("unexpected probe credentials: Authorization present=%t x-api-key present=%t", r.Header.Get("Authorization") != "", r.Header.Get("x-api-key") != "")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if test.wireFormat == "openai" && !openAIAuth || test.wireFormat == "anthropic" && !anthropicAuth {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if test.wireFormat == "openai" {
					_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"model","object":"model"}]}`)
					return
				}
				_, _ = io.WriteString(w, `{"data":[{"id":"model","type":"model"}],"has_more":false}`)
			}))
			defer server.Close()

			protocol, err := detectProviderProtocol(context.Background(), server.URL+test.basePath, "provider-secret", "", server.Client())
			if err != nil || protocol != test.wireFormat {
				t.Fatalf("detected protocol=%q err=%v, want %q", protocol, err, test.wireFormat)
			}
			if calls != 2 {
				t.Fatalf("probe calls=%d, want one request per auth/protocol shape", calls)
			}
		})
	}
}
