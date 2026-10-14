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

func TestRoutingCommandHidesAndClearsLegacyBillingSetting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := providerBudgetTestConfig(path)
	config.Routing = &gateway.RoutingConfig{BillingExhaustionFailover: true}
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
	if err := output.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := routingCommand([]string{"show", "--config", path}, output, output); err != nil {
		t.Fatal(err)
	}
	show, err := os.ReadFile(output.Name())
	if err != nil || string(show) != "Shared model strategy: off\n" {
		t.Fatalf("unexpected routing output: output=%q err=%v", show, err)
	}

	steps := []struct {
		name string
		args []string
		want gateway.RoutingConfig
	}{
		{"enable shared clears legacy", []string{"--shared-model-strategy=random"}, gateway.RoutingConfig{SharedModelStrategy: "random"}},
		{"change shared", []string{"--shared-model-strategy=price_priority"}, gateway.RoutingConfig{SharedModelStrategy: "price_priority"}},
		{"restore defaults", []string{"--shared-model-strategy=off"}, gateway.RoutingConfig{}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			args := append([]string{"set", "--config", path}, step.args...)
			if err := routingCommand(args, output, output); err != nil {
				t.Fatal(err)
			}
			loaded, err := gateway.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.EffectiveRouting(); got != step.want {
				t.Fatalf("routing settings=%+v, want %+v", got, step.want)
			}
			if step.want == (gateway.RoutingConfig{}) && loaded.Routing != nil {
				t.Fatalf("disabled routing settings were not omitted: %+v", loaded.Routing)
			}
		})
	}
}
