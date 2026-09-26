package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
)

type testSecrets map[string]string

func (s testSecrets) Lookup(_ context.Context, r KeychainReference) (string, error) {
	return s[r.Service], nil
}

type keyedTestSecrets map[string]string

func (s keyedTestSecrets) Lookup(_ context.Context, r KeychainReference) (string, error) {
	return s[r.Service+"/"+r.Account], nil
}

func TestConfigCredentialsAndValidation(t *testing.T) {
	c := testConfig("l.db", "https://example.com/prefix/v1")
	resolved, err := c.ResolveCredentials(context.Background(), testSecrets{"test.provider": "provider-private", "test.gateway": "local-private"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(resolved)
	if strings.Contains(string(data), "private") {
		t.Fatal("serialized secret")
	}
	for _, url := range []string{"http://example.com/v1", "https://user:pass@example.com/v1", "https://example.com/v1?key=x", "file:///tmp/x"} {
		bad := c
		bad.BaseURL = url
		if bad.Validate() == nil {
			t.Errorf("accepted %s", url)
		}
	}
	for _, value := range []int{-1, 4097} {
		bad := c
		bad.MaxActiveSessions = value
		if bad.Validate() == nil {
			t.Fatalf("accepted max_active_sessions=%d", value)
		}
	}
	for _, value := range []int{-1, 86401} {
		bad := c
		bad.ActiveSessionIdleTimeoutSeconds = value
		if bad.Validate() == nil {
			t.Fatalf("accepted active_session_idle_timeout_seconds=%d", value)
		}
	}
	bad := c
	bad.ListenAddr = "0.0.0.0:8787"
	if err := bad.Validate(); err != nil {
		t.Fatalf("non-loopback validation error=%v", err)
	}
	bad.ListenAddr = "192.168.1.10:8787"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "0.0.0.0") {
		t.Fatalf("specific LAN listener validation error=%v", err)
	}
	badProtocol := c
	badProtocol.Protocol = "both"
	if err := badProtocol.Validate(); err == nil {
		t.Fatal("both protocol accepted as a client mode")
	}
	for _, protocol := range []string{"", "auto", "OPENAI", "anthropic"} {
		auto := c
		auto.Protocol = protocol
		if err := auto.Validate(); err != nil {
			t.Fatalf("protocol %q rejected: %v", protocol, err)
		}
	}
	for _, body := range []string{`{"deepseek_api_key":"secret"}`, `{} {}`, `{"protocol":"openai","protocol":"anthropic"}`} {
		p := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(p, []byte(body), 0600)
		if _, err := LoadConfig(p); err == nil {
			t.Fatal("bad config accepted")
		}
	}
}

func TestIndependentProvidersResolveSeparateKeychainCredentials(t *testing.T) {
	c := testConfig("l.db", "https://example.com/v1")
	c.Protocol = ""
	c.APIVersion = ""
	c.UpstreamKeychain = KeychainReference{}
	c.APIKey = ""
	c.Model = ""
	c.UpstreamID = ""
	c.ModelCapabilities = ModelCapabilities{}
	c.Prices = nil
	c.BaseURL = ""
	c.Providers = map[string]Provider{
		"openai-main":    {Protocol: "openai", BaseURL: "https://openai.example/v1", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"}},
		"anthropic-main": {Protocol: "anthropic", BaseURL: "https://anthropic.example/v1", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	secrets := keyedTestSecrets{
		"test.provider/openai":    "openai-private",
		"test.provider/anthropic": "anthropic-private",
		"test.gateway/default":    "gateway-private",
	}
	resolved, err := c.ResolveCredentials(context.Background(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Providers["openai-main"].APIKey != "openai-private" || resolved.Providers["anthropic-main"].APIKey != "anthropic-private" {
		t.Fatalf("provider credentials were not resolved: %#v", resolved.Providers)
	}
	if keys := resolved.Providers["openai-main"].ResolvedAPIKeys(); len(keys) != 1 || keys[0] != "openai-private" {
		t.Fatalf("legacy single-key provider did not resolve as a one-key group: %#v", keys)
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private") {
		t.Fatalf("resolved credential serialized: %s", data)
	}

	shared := c
	shared.Providers = map[string]Provider{
		"openai-main": c.Providers["openai-main"],
		"anthropic-main": {
			Protocol:         "anthropic",
			BaseURL:          "https://anthropic.example/v1",
			UpstreamKeychain: c.Providers["openai-main"].UpstreamKeychain,
		},
	}
	sharedResolved, err := shared.ResolveCredentials(context.Background(), keyedTestSecrets{
		"test.provider/openai": "shared-private",
		"test.gateway/default": "gateway-private",
	})
	if err != nil || sharedResolved.Providers["anthropic-main"].APIKey != "shared-private" {
		t.Fatalf("shared endpoint credential resolution failed: err=%v", err)
	}
}

func TestSupportedModelsAreOptionalAllowlist(t *testing.T) {
	c := testConfig("l.db", "https://example.com/v1")
	c.BaseURL = ""
	c.Protocol = ""
	c.APIVersion = ""
	c.APIKey = ""
	c.Model = ""
	c.UpstreamID = ""
	c.UpstreamKeychain = KeychainReference{}
	c.ModelCapabilities = ModelCapabilities{}
	c.Prices = nil
	c.Providers = map[string]Provider{
		"main": {
			Protocol: "openai", BaseURL: "https://example.com/v1",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "main"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("omitted allowlist should mean all models: %v", err)
	}

	provider := c.Providers["main"]
	provider.SupportedModels = []string{"model-a", "model-b"}
	c.Providers["main"] = provider
	if err := c.Validate(); err != nil {
		t.Fatalf("valid supported_models rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		models []string
	}{
		{name: "duplicate model", models: []string{"model-a", "model-a"}},
		{name: "empty model", models: []string{"model-a", " "}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := c
			badProvider := provider
			badProvider.SupportedModels = test.models
			bad.Providers = map[string]Provider{"main": badProvider}
			if err := bad.Validate(); err == nil {
				t.Fatal("invalid supported_models accepted")
			}
		})
	}
}

func TestNamedProviderProtocolMayBeAutomaticallyDetected(t *testing.T) {
	c := testConfig("l.db", "https://legacy.example/v1")
	c.BaseURL = ""
	c.Protocol = ""
	c.APIVersion = ""
	c.UpstreamKeychain = KeychainReference{}
	c.APIKey = ""
	c.Model = ""
	c.UpstreamID = ""
	c.ModelCapabilities = ModelCapabilities{}
	c.Prices = nil
	c.Providers = map[string]Provider{
		"auto-provider": {
			Protocol:         "auto",
			BaseURL:          "https://provider.example/v1",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "auto"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("automatic provider protocol was rejected: %v", err)
	}

	bad := c
	provider := bad.Providers["auto-provider"]
	provider.Protocol = "both"
	bad.Providers["auto-provider"] = provider
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "protocol must be auto, openai or anthropic") {
		t.Fatalf("invalid protocol error=%v", err)
	}
}

func TestNamedProviderConfigRoundTrips(t *testing.T) {
	config := Config{
		ListenAddr: "127.0.0.1:4000",
		Providers: map[string]Provider{
			"openai": {
				Protocol:         "openai",
				BaseURL:          "https://openai.example/v1",
				UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"},
			},
			"anthropic": {
				Protocol:         "anthropic",
				BaseURL:          "https://anthropic.example/v1",
				UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"},
			},
		},
		AccessTokenKeychain: KeychainReference{Service: "test.gateway", Account: "default"},
		MaxInFlight:         1,
		LedgerPath:          filepath.Join(t.TempDir(), "ledger.db"),
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers) != 2 {
		t.Fatalf("provider profiles did not round-trip: %+v", loaded.Providers)
	}
	if strings.Contains(string(data), "default_providers") || strings.Contains(string(data), "default_model") {
		t.Fatalf("configuration serialized an implicit route or model: %s", data)
	}
}

func TestNamedProvidersAllowSameProtocolWithExplicitRequestRouting(t *testing.T) {
	c := testConfig("l.db", "https://legacy.example/v1")
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
		"openai-primary":   {Protocol: "openai", BaseURL: "https://primary.example/v1", UpstreamKeychain: KeychainReference{Service: "test", Account: "primary"}},
		"openai-secondary": {Protocol: "openai", BaseURL: "https://secondary.example/v1", UpstreamKeychain: KeychainReference{Service: "test", Account: "secondary"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("same-protocol providers rejected: %v", err)
	}
}

func TestBudgetPricingIsCheckedForEachRequestedModel(t *testing.T) {
	c := namedProviderConfig(testConfig("l.db", "https://example.com/prefix/v1"), "openai-main")
	provider := c.Providers["openai-main"]
	provider.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .8, Mode: "hard"}
	c.Providers["openai-main"] = provider
	if err := c.Validate(); err != nil {
		t.Fatalf("provider budget should not require a default model price: %v", err)
	}
	provider.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	c.Providers["openai-main"] = provider
	if err := c.Validate(); err != nil {
		t.Fatalf("priced budget rejected: %v", err)
	}
}

func TestProviderBudgetUsesEachProvidersModelAndPrices(t *testing.T) {
	c := testConfig("l.db", "https://legacy.example/v1")
	c.Protocol, c.APIVersion, c.UpstreamKeychain, c.APIKey = "", "", KeychainReference{}, ""
	c.Model, c.UpstreamID, c.ModelCapabilities, c.Prices, c.BaseURL = "", "", ModelCapabilities{}, nil, ""
	c.Providers = map[string]Provider{
		"openai-main": {
			Protocol:         "openai",
			BaseURL:          "https://openai.example/v1",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"},
			Prices:           map[string]adapter.Price{"openai-model": testPrice()},
			Budget:           &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .8, Mode: "hard"},
		},
		"anthropic-main": {
			Protocol:         "anthropic",
			BaseURL:          "https://anthropic.example/v1",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("independent provider pricing rejected: %v", err)
	}
	openAI := c.Providers["openai-main"]
	openAI.Prices = nil
	c.Providers["openai-main"] = openAI
	if err := c.Validate(); err != nil {
		t.Fatalf("missing default-model pricing should not invalidate config: %v", err)
	}
}

func TestNamedProviderBudgetAcceptsMatchingBuiltInPrice(t *testing.T) {
	c := testConfig("l.db", "https://legacy.example/v1")
	c.Protocol, c.APIVersion, c.UpstreamKeychain, c.APIKey = "", "", KeychainReference{}, ""
	c.Model, c.UpstreamID, c.BaseURL = "", "", ""
	c.ModelCapabilities, c.Prices = ModelCapabilities{}, nil
	c.Providers = map[string]Provider{"deepseek": {
		Protocol: "openai", BaseURL: "https://api.deepseek.com/v1",
		UpstreamID: "deepseek", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "deepseek"},
		Budget: &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .8, Mode: "hard"},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("built-in price should satisfy budget pricing: %v", err)
	}
}

func TestLegacyBudgetConfigReportsConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"budget":{"daily_limit":1,"monthly_limit":2,"timezone":"UTC","reserve_amount":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "legacy budget fields cannot be migrated automatically") {
		t.Fatalf("error=%v", err)
	}
}

func TestGlobalBudgetMustBeAssignedToAProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	policy := ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 5, WeeklyLimit: 80, AlertThreshold: .8, Mode: "hard"}
	c := namedProviderConfig(testConfig(path, "https://example.com/v1"), "test")
	c.Providers["test"] = Provider{
		Protocol: "openai", BaseURL: "https://example.com/v1",
		UpstreamID: "test", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "default"},
		Prices: map[string]adapter.Price{"custom-model": testPrice()},
	}
	c.LegacyBudget = &policy
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "tidemux provider budget REF") {
		t.Fatalf("global budget loaded without reassignment: %v", err)
	}
	loaded, moved, replaced, err := LoadConfigForProviderBudgetMigration(path)
	if err != nil {
		t.Fatal(err)
	}
	if moved != policy || replaced || loaded.LegacyBudget != nil {
		t.Fatalf("migration result config=%+v budget=%+v replaced=%t", loaded, moved, replaced)
	}
}

func TestReportScheduleValidation(t *testing.T) {
	for _, value := range []string{"00:00", "09:05", "23:59"} {
		normalized, err := NormalizeReportScheduleTime(value)
		if err != nil || normalized != value {
			t.Fatalf("time %q normalized=%q err=%v", value, normalized, err)
		}
	}
	for _, value := range []string{"9:05", "24:00", "12:60", "noon"} {
		if _, err := NormalizeReportScheduleTime(value); err == nil {
			t.Fatalf("accepted invalid time %q", value)
		}
	}
	for _, schedule := range []ReportSchedule{
		{Time: "09:00", Channel: "invalid"},
		{Time: "09:00", Channel: "smtp"},
		{Time: "", Channel: "macos"},
	} {
		if err := schedule.Validate(); err == nil {
			t.Fatalf("accepted invalid schedule %+v", schedule)
		}
	}
	if got := (ReportSchedule{Time: "09:00"}).EffectiveChannel(); got != "macos" {
		t.Fatalf("default channel=%q", got)
	}
	if got := (ReportSchedule{Time: "09:00", Channel: "webhook"}).EffectiveChannel(); got != "webhook" {
		t.Fatalf("webhook channel=%q", got)
	}
	webhook := testConfig("l.db", "https://example.com/v1")
	webhook.ReportWebhook = ReportWebhookConfig{Provider: "discord", Keychain: KeychainReference{Service: "webhook", Account: "default"}}
	webhook.ReportSchedule = ReportSchedule{Time: "09:00", Channel: "webhook"}
	if err := webhook.Validate(); err != nil {
		t.Fatalf("valid webhook config rejected: %v", err)
	}
	without := webhook
	without.ReportWebhook = ReportWebhookConfig{}
	if err := without.Validate(); err == nil || !strings.Contains(err.Error(), "requires report_webhook") {
		t.Fatalf("missing webhook config error=%v", err)
	}
	resolved, err := webhook.ResolveCredentials(context.Background(), testSecrets{"test.provider": "provider-private", "test.gateway": "local-private"})
	if err != nil {
		t.Fatalf("gateway credentials should not depend on the optional report webhook: %v", err)
	}
	if resolved.ReportWebhookURL != "" {
		t.Fatal("gateway credential resolution loaded the report webhook endpoint")
	}
}
