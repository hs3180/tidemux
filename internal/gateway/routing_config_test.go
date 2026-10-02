package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRoutingConfigurationIsOptInAndAutoChainIsValidated(t *testing.T) {
	config := namedProviderConfig(testConfig("l.db", "https://example.com/v1"), "main")
	provider := config.Providers["main"]
	provider.SupportedModels = []string{"model-one", "model-two"}
	config.Providers["main"] = provider
	if err := config.Validate(); err != nil {
		t.Fatalf("default routing config rejected: %v", err)
	}
	if config.EffectiveRouting() != (RoutingConfig{}) || len(config.AutoChain) != 0 {
		t.Fatalf("routing should be disabled by default: routing=%+v chain=%v", config.Routing, config.AutoChain)
	}

	config.AutoChain = []AutoChainEntry{{Provider: "main", Model: "model-two"}, {Provider: "main", Model: "model-one"}}
	config.Providers["main"] = provider
	config.Routing = &RoutingConfig{SharedModelStrategy: "price_priority", BillingExhaustionFailover: true}
	if err := config.Validate(); err != nil {
		t.Fatalf("valid routing config rejected: %v", err)
	}
	for name, chain := range map[string][]AutoChainEntry{
		"duplicate":        {{Provider: "main", Model: "model-one"}, {Provider: "main", Model: "model-one"}},
		"auto entry":       {{Provider: "main", Model: "auto"}},
		"out of scope":     {{Provider: "main", Model: "other-model"}},
		"missing provider": {{Provider: "missing", Model: "model-one"}},
		"empty model":      {{Provider: "main", Model: ""}},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := config
			invalid.Providers = map[string]Provider{"main": provider}
			invalid.AutoChain = chain
			if err := invalid.Validate(); err == nil {
				t.Fatal("invalid auto model chain accepted")
			}
		})
	}
	config.Routing.SharedModelStrategy = "cheapest_unverified"
	if err := config.Validate(); err == nil {
		t.Fatal("unknown shared-model strategy accepted")
	}
}

func TestV022ConfigWithoutRoutingFieldsLoadsWithSafeDefaults(t *testing.T) {
	config := namedProviderConfig(testConfig("l.db", "https://example.com/v1"), "main")
	provider := config.Providers["main"]
	provider.SupportedModels = []string{"model-one"}
	config.Providers["main"] = provider
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "routing")
	delete(raw, "auto_chain")
	var providers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw["providers"], &providers); err != nil {
		t.Fatal(err)
	}
	delete(providers["main"], "auto_model_chain")
	raw["providers"], err = json.Marshal(providers)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("0.2.2-style config rejected: %v", err)
	}
	if loaded.EffectiveRouting() != (RoutingConfig{}) || len(loaded.AutoChain) != 0 {
		t.Fatalf("old config did not load with disabled defaults: routing=%+v chain=%v", loaded.Routing, loaded.AutoChain)
	}
}
