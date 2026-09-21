package adapter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoverModelsSupportsOpenAIAndAnthropic(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Fatalf("path=%s", r.URL.Path)
				}
				if protocol == "openai" && r.Header.Get("Authorization") != "Bearer secret" {
					t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
				}
				if protocol == "anthropic" && (r.Header.Get("x-api-key") != "secret" || r.Header.Get("anthropic-version") != "2023-06-01") {
					t.Fatalf("Anthropic headers=%q/%q", r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
				}
				io.WriteString(w, `{"data":[{"id":"model-a","display_name":"A","unexpected":"ignored"},{"id":"model-a"},{"id":"model-b"},{"id":""}]}`)
			}))
			defer server.Close()
			models, err := DiscoverModels(context.Background(), protocol, server.URL+"/v1", "secret", "2023-06-01", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 2 || models[0].ID != "model-a" || models[0].DisplayName != "A" || models[1].ID != "model-b" {
				t.Fatalf("models=%+v", models)
			}
		})
	}
}

func TestDiscoverModelsRejectsUnsafeAndInvalidResponses(t *testing.T) {
	for _, baseURL := range []string{"https://user:pass@example.com/v1", "http://example.com/v1", "https://example.com/v1?key=secret"} {
		if _, err := DiscoverModels(context.Background(), "openai", baseURL, "secret", "", nil); err == nil {
			t.Fatalf("accepted unsafe base URL %q", baseURL)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":"private detail"}`)
	}))
	defer server.Close()
	if _, err := DiscoverModels(context.Background(), "openai", server.URL, "secret", "", server.Client()); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("error=%v", err)
	}
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"`+strings.Repeat("x", maxModelDiscoveryBytes)+`"}]}`)
	}))
	defer large.Close()
	if _, err := DiscoverModels(context.Background(), "openai", large.URL, "secret", "", large.Client()); err == nil {
		t.Fatal("accepted oversized response")
	}
}
