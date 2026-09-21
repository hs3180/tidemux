package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReportWebhookPayloadsArePlainText(t *testing.T) {
	for _, provider := range []string{"generic", "telegram", "discord", "lark"} {
		t.Run(provider, func(t *testing.T) {
			var payload map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("request = %s %s", r.Method, r.Header.Get("Content-Type"))
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			if err := postReportWebhook(server.URL, provider, "TideMux 2026-09-21 usage report\nRequests: 1"); err != nil {
				t.Fatal(err)
			}
			var message string
			switch provider {
			case "discord":
				message, _ = payload["content"].(string)
			case "lark":
				content, _ := payload["content"].(map[string]any)
				message, _ = content["text"].(string)
			default:
				message, _ = payload["text"].(string)
			}
			if message == "" || strings.Contains(message, "<html") || !strings.Contains(message, "Requests: 1") {
				t.Fatalf("payload=%#v", payload)
			}
		})
	}
}

func TestReportWebhookRejectsUnsafeOrOversizedEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/hook", "not a URL", ""} {
		if err := validateReportWebhookEndpoint(endpoint); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
	if err := postReportWebhook("http://127.0.0.1:8787/hook", "generic", strings.Repeat("x", maxReportWebhookBytes+1)); err == nil {
		t.Fatal("accepted oversized webhook message")
	}
}
