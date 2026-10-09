package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
)

func providerBudgetTestPrice() adapter.Price {
	input, output := 1.0, 1.0
	return adapter.Price{Currency: "USD", Source: "test", Version: "1", InputCacheHit: &input, InputCacheMiss: &input, Output: &output}
}

func providerBudgetTestConfig(path string) gateway.Config {
	provider := func(name, protocol, model string) gateway.Provider {
		return gateway.Provider{
			Protocol: protocol, BaseURL: "https://" + name + ".example/v1", UpstreamID: name,
			UpstreamKeychain: gateway.KeychainReference{Service: "test.provider", Account: name},
			Prices:           map[string]adapter.Price{model: providerBudgetTestPrice()},
		}
	}
	return gateway.Config{
		ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: filepath.Join(filepath.Dir(path), "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "test.gateway", Account: "local"},
		Providers: map[string]gateway.Provider{
			"p1": provider("p1", "openai", "model-one"),
			"p2": provider("p2", "anthropic", "model-two"),
		},
	}
}

func TestProviderBudgetCommandScopesSettingsToSelectedProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeCommandConfig(path, providerBudgetTestConfig(path), nil); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.CreateTemp(dir, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(dir, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	args := []string{"p1", "--budget-5h", "5", "--budget-weekly", "80", "--config", path}
	if err := providerBudgetCommand(args, os.Stdin, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 5, WeeklyLimit: 80, AlertThreshold: .8, Mode: "hard"}
	if loaded.Providers["p1"].Budget == nil || *loaded.Providers["p1"].Budget != want {
		t.Fatalf("p1 budget=%+v want %+v", loaded.Providers["p1"].Budget, want)
	}
	if loaded.Providers["p2"].Budget != nil {
		t.Fatalf("p2 budget changed: %+v", loaded.Providers["p2"].Budget)
	}
	if err := providerBudgetCommand([]string{"p1", "--budget-5h", "0", "--budget-weekly", "0", "--config", path}, os.Stdin, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err = gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Providers["p1"].Budget != nil {
		t.Fatalf("zero limits did not disable p1 budget: %+v", loaded.Providers["p1"].Budget)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw["budget"]; exists {
		t.Fatal("budget was written at gateway scope")
	}
	var providers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw["providers"], &providers); err != nil {
		t.Fatal(err)
	}
	if _, exists := providers["p1"]["budget"]; exists {
		t.Fatal("disabled provider budget was not omitted")
	}
}

func TestProviderBudgetRejectsGlobalPolicyWithoutChangingConfig(t *testing.T) {
	for name, budget := range map[string]string{
		"empty":   "{}",
		"null":    "null",
		"rolling": `{"currency":"USD","five_hour_limit":5,"weekly_limit":80,"alert_threshold":0.8,"mode":"hard"}`,
		"legacy":  `{"daily_limit":1,"monthly_limit":2,"timezone":"UTC","reserve_amount":1}`,
	} {
		for _, disable := range []bool{false, true} {
			mode := "set"
			if disable {
				mode = "disable"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "config.json")
				data, err := json.Marshal(struct {
					gateway.Config
					Budget json.RawMessage `json:"budget"`
				}{Config: providerBudgetTestConfig(path), Budget: json.RawMessage(budget)})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				stdout, err := os.CreateTemp(dir, "stdout")
				if err != nil {
					t.Fatal(err)
				}
				defer stdout.Close()
				stderr, err := os.CreateTemp(dir, "stderr")
				if err != nil {
					t.Fatal(err)
				}
				defer stderr.Close()
				args := []string{"p1", "--budget-5h", "5", "--config", path}
				if disable {
					args = []string{"p1", "--disable", "--config", path}
				}
				if err := providerBudgetCommand(args, os.Stdin, stdout, stderr); err == nil {
					t.Fatal("global budget field was accepted")
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(data) {
					t.Fatal("rejected budget command changed the configuration")
				}
			})
		}
	}
}

func TestProviderBudgetRequiresReferenceWhenSeveralProvidersExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeCommandConfig(path, providerBudgetTestConfig(path), nil); err != nil {
		t.Fatal(err)
	}
	stdout, _ := os.CreateTemp(dir, "stdout")
	defer stdout.Close()
	stderr, _ := os.CreateTemp(dir, "stderr")
	defer stderr.Close()
	err := providerBudgetCommand([]string{"--disable", "--config", path}, os.Stdin, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "specify a provider") {
		t.Fatalf("ambiguous budget target was accepted: %v", err)
	}
}
