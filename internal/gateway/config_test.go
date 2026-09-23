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
		"openai":    {BaseURL: "https://example.com/openai/v1", Model: "openai-model", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"}},
		"anthropic": {BaseURL: "https://example.com/anthropic/v1", Model: "anthropic-model", UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"}},
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
	if resolved.Providers["openai"].APIKey != "openai-private" || resolved.Providers["anthropic"].APIKey != "anthropic-private" {
		t.Fatalf("provider credentials were not resolved: %#v", resolved.Providers)
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
		"openai": c.Providers["openai"],
		"anthropic": {
			BaseURL:          "https://example.com/anthropic/v1",
			Model:            "anthropic-model",
			UpstreamKeychain: c.Providers["openai"].UpstreamKeychain,
		},
	}
	sharedResolved, err := shared.ResolveCredentials(context.Background(), keyedTestSecrets{
		"test.provider/openai": "shared-private",
		"test.gateway/default": "gateway-private",
	})
	if err != nil || sharedResolved.Providers["anthropic"].APIKey != "shared-private" {
		t.Fatalf("shared endpoint credential resolution failed: err=%v", err)
	}
}

func TestIndependentProviderExampleValidates(t *testing.T) {
	config, err := LoadConfig(filepath.Join("..", "..", "examples", "dual-providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Providers["openai"].Model == config.Providers["anthropic"].Model {
		t.Fatal("example should show independent provider model IDs")
	}
}

func TestBudgetRequiresConfiguredModelPricing(t *testing.T) {
	c := testConfig("l.db", "https://example.com/prefix/v1")
	c.Budget = ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .8, Mode: "hard"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "budget requires pricing") {
		t.Fatalf("missing pricing error=%v", err)
	}
	c.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	if err := c.Validate(); err != nil {
		t.Fatalf("priced budget rejected: %v", err)
	}
}

func TestProviderBudgetUsesEachProvidersModelAndPrices(t *testing.T) {
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
	c.Budget = ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .8, Mode: "hard"}
	c.Providers = map[string]Provider{
		"openai": {
			BaseURL: "https://openai.example/v1", Model: "openai-model",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "openai"},
			Prices:           map[string]adapter.Price{"openai-model": testPrice()},
		},
		"anthropic": {
			BaseURL: "https://anthropic.example/v1", Model: "anthropic-model",
			UpstreamKeychain: KeychainReference{Service: "test.provider", Account: "anthropic"},
			Prices:           map[string]adapter.Price{"anthropic-model": testPrice()},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("independent provider pricing rejected: %v", err)
	}
	openAI := c.Providers["openai"]
	openAI.Prices = nil
	c.Providers["openai"] = openAI
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "each configured provider model") {
		t.Fatalf("missing one provider's price accepted: %v", err)
	}
}

func TestLegacyBudgetConfigReportsConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"budget":{"daily_limit":1,"monthly_limit":2,"timezone":"UTC","reserve_amount":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "legacy budget fields conflict") {
		t.Fatalf("error=%v", err)
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
}
