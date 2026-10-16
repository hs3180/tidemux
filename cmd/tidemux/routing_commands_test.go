package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestRoutingCommandConfiguresSharedModelStrategy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := providerBudgetTestConfig(path)
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

	if err := routingCommand([]string{"set", "--shared-model-strategy", "random", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EffectiveRouting().SharedModelStrategy != "random" {
		t.Fatalf("routing config=%+v", loaded.Routing)
	}
	if err := routingCommand([]string{"set", "--shared-model-strategy=off", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err = gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EffectiveRouting() != (gateway.RoutingConfig{}) || loaded.Routing != nil {
		t.Fatalf("shared-model routing defaults were not restored: %+v", loaded.Routing)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"routing"`) {
		t.Fatalf("disabled routing field was not omitted: %s", data)
	}
}

func TestRoutingCommandRejectsRemovedBillingFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := providerBudgetTestConfig(path)
	config.Routing = &gateway.RoutingConfig{SharedModelStrategy: "price_priority"}
	if err := writeCommandConfig(path, config, nil); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(dir, "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"false", "true"} {
		args := []string{"set", "--shared-model-strategy=random", "--billing-exhaustion-failover=" + value, "--config", path}
		if err := routingCommand(args, output, output); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("removed billing flag %s was not rejected as unknown: %v", value, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected billing flag changed the config: err=%v", err)
	}
}

func TestRoutingThresholdCommandPreservesStrategyAndRestoresDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := providerBudgetTestConfig(path)
	if err := writeCommandConfig(path, config, nil); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(dir, "output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	for _, value := range []string{"0", "120", "default"} {
		if err := routingCommand([]string{"set", "--provider-switch-threshold", value, "--config", path}, out, out); err != nil {
			t.Fatal(err)
		}
		if err := routingCommand([]string{"set", "--shared-model-strategy", "random", "--config", path}, out, out); err != nil {
			t.Fatal(err)
		}
		loaded, err := gateway.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		settings := loaded.EffectiveRouting()
		want := int64(60)
		if value == "0" {
			want = 0
		}
		if value == "120" {
			want = 120
		}
		if settings.EffectiveProviderSwitchThreshold() != want || settings.SharedModelStrategy != "random" {
			t.Fatal("unrelated routing policy was overwritten")
		}
		if value == "default" && settings.ProviderSwitchThreshold != nil {
			t.Fatal("default did not remove the override")
		}
	}
	before, _ := os.ReadFile(path)
	for _, value := range []string{"-1", "86401", "1.5", "private-text"} {
		if routingCommand([]string{"set", "--provider-switch-threshold", value, "--config", path}, out, out) == nil {
			t.Fatal("accepted invalid threshold")
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("invalid command changed configuration")
		}
	}
	if err := routingCommand([]string{"show", "--config", path}, out, out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(data), "Provider switch threshold: 60 seconds") {
		t.Fatal("effective default is absent from show")
	}
}
