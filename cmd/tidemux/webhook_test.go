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

func (k *webhookTestKeychain) Lookup(_ context.Context, reference gateway.KeychainReference) (string, error) {
	value, ok := k.values[reference.Service+"/"+reference.Account]
	if !ok {
		return "", errors.New("simulated missing Keychain item")
	}
	return value, nil
}

func (k *webhookTestKeychain) Delete(_ context.Context, reference gateway.KeychainReference) error {
	if k.failDelete {
		return errors.New("simulated Keychain deletion failure")
	}
	delete(k.values, reference.Service+"/"+reference.Account)
	return nil
}

func TestResolveReportWebhookLoadsValidatedEndpointFromKeychain(t *testing.T) {
	reference := gateway.KeychainReference{Service: "com.tidemux.report-webhook", Account: "test"}
	endpoint := "https://hooks.example.invalid/bot/private-token"
	config := gateway.Config{ReportWebhook: gateway.ReportWebhookConfig{Provider: "lark", Keychain: reference}}
	lookup := &webhookTestKeychain{values: map[string]string{reference.Service + "/" + reference.Account: endpoint}}

	resolved, err := resolveReportWebhook(config, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ReportWebhookURL != endpoint {
		t.Fatalf("resolved endpoint = %q, want Keychain value", resolved.ReportWebhookURL)
	}
	if config.ReportWebhookURL != "" {
		t.Fatal("resolving endpoint mutated the original config")
	}
}

func TestResolveReportWebhookRejectsMissingOrInvalidKeychainEndpoint(t *testing.T) {
	reference := gateway.KeychainReference{Service: "com.tidemux.report-webhook", Account: "test"}
	config := gateway.Config{ReportWebhook: gateway.ReportWebhookConfig{Provider: "lark", Keychain: reference}}

	if _, err := resolveReportWebhook(config, &webhookTestKeychain{values: map[string]string{}}); err == nil || !strings.Contains(err.Error(), "Keychain item unavailable") {
		t.Fatalf("missing Keychain item error = %v", err)
	}
	lookup := &webhookTestKeychain{values: map[string]string{reference.Service + "/" + reference.Account: "http://example.com/private-token"}}
	if _, err := resolveReportWebhook(config, lookup); err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("invalid endpoint error = %v", err)
	}
	if _, err := resolveReportWebhook(gateway.Config{}, lookup); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured webhook error = %v", err)
	}
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

func TestProviderReferenceScanIncludesMultiKeyGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	config := `{"providers":{
		"single":{"upstream_keychain":{"service":"provider","account":"single"}},
		"group":{"upstream_keychains":[
			{"service":"provider","account":"first"},
			{"service":"provider","account":"second"}
		]}
	}}`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	used, err := providerUsesKeychainReference(path, gateway.KeychainReference{Service: "provider", Account: "second"})
	if err != nil || !used {
		t.Fatalf("multi-key provider reference used=%t err=%v", used, err)
	}
	used, err = providerUsesKeychainReference(path, gateway.KeychainReference{Service: "provider", Account: "missing"})
	if err != nil || used {
		t.Fatalf("missing provider reference used=%t err=%v", used, err)
	}

	duplicate := `{"providers":{"group":{"upstream_keychains":[],"upstream_keychains":[]}}}`
	if err := os.WriteFile(path, []byte(duplicate), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := providerUsesKeychainReference(path, gateway.KeychainReference{Service: "provider", Account: "second"}); err == nil {
		t.Fatal("duplicate provider key references should prevent cleanup")
	}
}

func TestDeleteStaleWebhookCredentialPreservesMultiKeyProviderReference(t *testing.T) {
	path := writeWebhookScheduleTestConfig(t)
	current, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := current.ReportWebhook.Keychain
	if err := replaceReportWebhookConfig(path, gateway.ReportWebhookConfig{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"protocol", "base_url", "model", "upstream_id", "upstream_keychain"} {
		delete(raw, field)
	}
	provider := map[string]json.RawMessage{
		"protocol": json.RawMessage(`"openai"`),
		"base_url": json.RawMessage(`"https://provider.example/v1"`),
		"model":    json.RawMessage(`"model"`),
		"upstream_keychains": mustWebhookJSON([]gateway.KeychainReference{
			previous,
		}),
	}
	raw["providers"] = mustWebhookJSON(map[string]map[string]json.RawMessage{"primary": provider})
	raw["default_providers"] = mustWebhookJSON(map[string]string{"openai": "primary"})
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	store := &webhookTestKeychain{values: map[string]string{previous.Service + "/" + previous.Account: "still-used-secret"}}
	err = deleteStaleReportWebhookEndpoint(path, previous, store)
	if _, exists := store.values[previous.Service+"/"+previous.Account]; !exists {
		t.Fatal("a Keychain item referenced by a multi-key provider was deleted")
	}
	if err != nil {
		if _, configErr := gateway.LoadConfig(path); configErr == nil {
			t.Fatalf("valid multi-key config should be safely handled, got %v", err)
		}
	}
}

func mustWebhookJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
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
				if provider == "lark" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
					return
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

func TestLarkWebhookReportsProviderRejectionWithoutLeakingEndpoint(t *testing.T) {
	const token = "test-webhook-secret-token-123456"
	var endpoint string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":19021,"msg":"token ` + token + ` is not allowed"}`))
	}))
	defer server.Close()
	endpoint = server.URL + "/hook/" + token

	err := postReportWebhook(endpoint, "lark", "TideMux test report")
	if err == nil || !strings.Contains(err.Error(), "code 19021") || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %v, want provider code and safe message", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), endpoint) {
		t.Fatalf("error exposed webhook credential: %v", err)
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
	if err := deliverReportWithLocale("webhook", report, htmlPath, server.URL, "generic", reportLocaleSimplifiedChinese); err != nil {
		t.Fatal(err)
	}
	message := payload["text"]
	if !strings.Contains(message, "📊 TideMux 每日用量") || !strings.Contains(message, "日期：2026-09-24") || !strings.Contains(message, "请求：3 次（失败 0 次）") {
		t.Fatalf("webhook did not contain the readable report summary: %q", message)
	}
	if strings.Contains(message, "TideMux 2026-09-24 usage report") {
		t.Fatalf("webhook contains a duplicate report title: %q", message)
	}
	if strings.Contains(message, privateHTML) || strings.Contains(message, htmlPath) || strings.Contains(strings.ToLower(message), "<html") {
		t.Fatalf("webhook included local HTML report data: %q", message)
	}
}

func TestReportTextIsReadableAndOmitsUnavailableChecks(t *testing.T) {
	cost := 1.234567
	mismatch := int64(0)
	report := ledger.DailyReport{
		Day:                 "2026-09-24",
		Timezone:            "Asia/Shanghai",
		RequestCount:        12345,
		FailureCount:        2,
		InputTokens:         1234567,
		OutputTokens:        234567,
		EstimatedCost:       &cost,
		Currency:            "USD",
		UnknownCostRequests: 3,
		TokenizerMismatches: &mismatch,
	}

	want := "📊 TideMux 每日用量\n日期：2026-09-24 · 时区：Asia/Shanghai\n\n请求：12,345 次（失败 2 次）\nToken：输入 1,234,567 · 输出 234,567\n本地估算费用：1.234567 USD\n未知费用请求：3 次\n\n数据校验\nToken 统计差异：0"
	if got := reportTextForLocale(report, reportLocaleSimplifiedChinese); got != want {
		t.Fatalf("report text =\n%s\nwant =\n%s", got, want)
	}
	if strings.Contains(reportTextForLocale(ledger.DailyReport{Day: "2026-09-24"}, reportLocaleSimplifiedChinese), "数据校验") {
		t.Fatal("unavailable optional checks should be omitted rather than shown as unknown")
	}
}

func TestReportTextSupportsEnglishAndTraditionalChinese(t *testing.T) {
	report := ledger.DailyReport{Day: "2026-09-24", Timezone: "Asia/Taipei", RequestCount: 12, FailureCount: 1, InputTokens: 1000, OutputTokens: 500}
	english := reportTextForLocale(report, reportLocaleEnglish)
	if !strings.Contains(english, "📊 TideMux Daily Usage") || !strings.Contains(english, "Date: 2026-09-24 · Timezone: Asia/Taipei") || !strings.Contains(english, "Requests: 12 total · Failures: 1") {
		t.Fatalf("English report = %q", english)
	}
	traditional := reportTextForLocale(report, reportLocaleTraditionalChinese)
	if !strings.Contains(traditional, "📊 TideMux 每日用量") || !strings.Contains(traditional, "日期：2026-09-24 · 時區：Asia/Taipei") || !strings.Contains(traditional, "請求：12 次（失敗 1 次）") {
		t.Fatalf("Traditional Chinese report = %q", traditional)
	}
	if got := reportMessagesFor("fr").title; got != reportCatalog[reportLocaleEnglish].title {
		t.Fatalf("unsupported locale fell back to %q, want English", got)
	}
}

func TestReportLocaleDetection(t *testing.T) {
	t.Run("uses the first supported Apple preferred language", func(t *testing.T) {
		output := "(\n    \"fr-FR\",\n    \"zh-Hant-TW\",\n    \"en-US\"\n)"
		got, ok := reportLocaleFromAppleLanguages(output)
		if !ok || got != reportLocaleTraditionalChinese {
			t.Fatalf("locale = %q, ok=%t", got, ok)
		}
	})

	t.Run("parses locale environment values", func(t *testing.T) {
		environment := map[string]string{"LANG": "zh_CN.UTF-8"}
		got, ok := reportLocaleFromEnvironment(func(key string) string { return environment[key] })
		if !ok || got != reportLocaleSimplifiedChinese {
			t.Fatalf("locale = %q, ok=%t", got, ok)
		}
	})

	t.Run("respects explicit locale override and English fallback", func(t *testing.T) {
		environment := map[string]string{"LC_ALL": "fr_FR.UTF-8", "LANG": "zh_CN.UTF-8"}
		got, ok := reportLocaleFromEnvironment(func(key string) string { return environment[key] })
		if !ok || got != reportLocaleEnglish {
			t.Fatalf("locale = %q, ok=%t", got, ok)
		}
	})

	t.Run("defaults to English when no system locale is available", func(t *testing.T) {
		got, ok := reportLocaleFromEnvironment(func(string) string { return "" })
		if ok {
			t.Fatalf("empty environment unexpectedly selected %q", got)
		}
		if fallback := reportMessagesFor("").title; fallback != reportCatalog[reportLocaleEnglish].title {
			t.Fatalf("default title = %q, want English", fallback)
		}
	})

	t.Run("selects simplified or traditional Chinese by script and region", func(t *testing.T) {
		for value, want := range map[string]reportLocale{
			"zh-Hans-CN": reportLocaleSimplifiedChinese,
			"zh_CN":      reportLocaleSimplifiedChinese,
			"zh-Hant-TW": reportLocaleTraditionalChinese,
			"zh_HK":      reportLocaleTraditionalChinese,
			"en_US":      reportLocaleEnglish,
		} {
			got, ok := reportLocaleForTag(value)
			if !ok || got != want {
				t.Errorf("locale for %q = %q, ok=%t; want %q", value, got, ok, want)
			}
		}
	})
}

func TestReportCountTextGroupsThousands(t *testing.T) {
	for value, want := range map[int64]string{
		0:                    "0",
		999:                  "999",
		1000:                 "1,000",
		123456789:            "123,456,789",
		-1234567:             "-1,234,567",
		-9223372036854775808: "-9,223,372,036,854,775,808",
	} {
		if got := reportCountText(value); got != want {
			t.Errorf("reportCountText(%d) = %q, want %q", value, got, want)
		}
	}
}
