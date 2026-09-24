package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
)

type webhookTestKeychain struct {
	values     map[string]string
	failDelete bool
}

func (k *webhookTestKeychain) Delete(_ context.Context, reference gateway.KeychainReference) error {
	if k.failDelete {
		return errors.New("simulated Keychain deletion failure")
	}
	delete(k.values, reference.Service+"/"+reference.Account)
	return nil
}

func TestDeleteStaleReportWebhookEndpoint(t *testing.T) {
	t.Run("deletes a replaced endpoint after the config is updated", func(t *testing.T) {
		path := writeWebhookScheduleTestConfig(t)
		current, err := gateway.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		oldRef := current.ReportWebhook.Keychain
		newRef := gateway.KeychainReference{Service: "com.tidemux.report-webhook", Account: "new"}
		store := &webhookTestKeychain{values: map[string]string{
			oldRef.Service + "/" + oldRef.Account: "old-endpoint-secret",
			newRef.Service + "/" + newRef.Account: "new-endpoint-secret",
		}}
		if err := replaceReportWebhookConfig(path, gateway.ReportWebhookConfig{Provider: "discord", Keychain: newRef}); err != nil {
			t.Fatal(err)
		}
		if err := deleteStaleReportWebhookEndpoint(path, oldRef, store); err != nil {
			t.Fatal(err)
		}
		if _, exists := store.values[oldRef.Service+"/"+oldRef.Account]; exists {
			t.Fatal("replaced webhook endpoint remains in Keychain")
		}
		if _, exists := store.values[newRef.Service+"/"+newRef.Account]; !exists {
			t.Fatal("current webhook endpoint was deleted")
		}
	})

	t.Run("preserves an endpoint reference still used by another credential", func(t *testing.T) {
		path := writeWebhookScheduleTestConfig(t)
		current, err := gateway.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		oldRef := current.ReportWebhook.Keychain
		current.UpstreamKeychain = oldRef
		data, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := replaceReportWebhookConfig(path, gateway.ReportWebhookConfig{}); err != nil {
			t.Fatal(err)
		}
		store := &webhookTestKeychain{values: map[string]string{oldRef.Service + "/" + oldRef.Account: "shared-secret"}}
		if err := deleteStaleReportWebhookEndpoint(path, oldRef, store); err != nil {
			t.Fatal(err)
		}
		if _, exists := store.values[oldRef.Service+"/"+oldRef.Account]; !exists {
			t.Fatal("Keychain item still referenced by upstream config was deleted")
		}
	})

	t.Run("reports Keychain deletion failure without disclosing the endpoint", func(t *testing.T) {
		path := writeWebhookScheduleTestConfig(t)
		current, err := gateway.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		oldRef := current.ReportWebhook.Keychain
		if err := replaceReportWebhookConfig(path, gateway.ReportWebhookConfig{}); err != nil {
			t.Fatal(err)
		}
		store := &webhookTestKeychain{values: map[string]string{oldRef.Service + "/" + oldRef.Account: "private-endpoint"}, failDelete: true}
		err = deleteStaleReportWebhookEndpoint(path, oldRef, store)
		if err == nil || !strings.Contains(err.Error(), "could not be deleted from Keychain") {
			t.Fatalf("expected explicit cleanup error, got %v", err)
		}
		if strings.Contains(err.Error(), "private-endpoint") {
			t.Fatal("cleanup error exposed the webhook endpoint")
		}
		if _, exists := store.values[oldRef.Service+"/"+oldRef.Account]; !exists {
			t.Fatal("failed Keychain deletion unexpectedly removed the item")
		}
	})
}

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

func TestDeliverReportWebhookSendsSummaryWithoutHTMLExport(t *testing.T) {
	htmlPath := filepath.Join(t.TempDir(), "private-report.html")
	const privateHTML = "<html><body>full report must stay local</body></html>"
	if err := os.WriteFile(htmlPath, []byte(privateHTML), 0o600); err != nil {
		t.Fatal(err)
	}

	var payload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	report := ledger.DailyReport{Day: "2026-09-24", RequestCount: 3, InputTokens: 12, OutputTokens: 7}
	if err := deliverReport("webhook", report, htmlPath, server.URL, "generic"); err != nil {
		t.Fatal(err)
	}
	message := payload["text"]
	if !strings.Contains(message, "TideMux 2026-09-24 usage report") || !strings.Contains(message, "Requests: 3") {
		t.Fatalf("webhook did not contain the readable report summary: %q", message)
	}
	if strings.Contains(message, privateHTML) || strings.Contains(message, htmlPath) || strings.Contains(strings.ToLower(message), "<html") {
		t.Fatalf("webhook included local HTML report data: %q", message)
	}
}
