package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestRoutingCommandConfiguresExplicitRoutingPolicies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := providerBudgetTestConfig(path)
	provider := config.Providers["p1"]
	provider.SupportedModels = []string{"model-one", "model-two"}
	config.Providers["p1"] = provider
	if err := writeCommandConfig(path, config, nil); err != nil {
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

	if err := routingCommand([]string{"set", "--shared-model-strategy", "price_priority", "--billing-exhaustion-failover=true", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EffectiveRouting().SharedModelStrategy != "price_priority" || !loaded.EffectiveRouting().BillingExhaustionFailover {
		t.Fatalf("routing config=%+v", loaded.Routing)
	}
	if err := routingCommand([]string{"set", "--shared-model-strategy=off", "--billing-exhaustion-failover=false", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err = gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EffectiveRouting() != (gateway.RoutingConfig{}) || loaded.Routing != nil {
		t.Fatalf("routing defaults were not restored: %+v", loaded.Routing)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"routing"`) {
		t.Fatalf("disabled routing field was not omitted: %s", data)
	}
}

func TestProviderAutoChainCommandPreservesOrderAndValidatesScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := providerBudgetTestConfig(path)
	provider := config.Providers["p1"]
	provider.SupportedModels = []string{"model-one", "model-two"}
	config.Providers["p1"] = provider
	if err := writeCommandConfig(path, config, nil); err != nil {
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

	if err := providerAutoChainCommand([]string{"p1", "--config", path, "--models", "model-two,model-one"}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(loaded.Providers["p1"].AutoModelChain, ","); got != "model-two,model-one" {
		t.Fatalf("auto model chain=%q", got)
	}
	if err := providerAutoChainCommand([]string{"p1", "--models=model-missing", "--config", path}, stdout, stderr); err == nil {
		t.Fatal("out-of-scope model was accepted")
	}
	if err := providerAutoChainCommand([]string{"p1", "--clear", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err = gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers["p1"].AutoModelChain) != 0 {
		t.Fatalf("auto model chain was not cleared: %v", loaded.Providers["p1"].AutoModelChain)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"auto_model_chain"`) {
		t.Fatalf("cleared auto model chain field was not omitted: %s", data)
	}
}
