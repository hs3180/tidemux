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

func TestRoutingCommandPreservesOtherSetting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeCommandConfig(path, providerBudgetTestConfig(path), nil); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(dir, "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	steps := []struct {
		name string
		args []string
		want gateway.RoutingConfig
	}{
		{"enable billing", []string{"--billing-exhaustion-failover=true"}, gateway.RoutingConfig{BillingExhaustionFailover: true}},
		{"enable shared", []string{"--shared-model-strategy=random"}, gateway.RoutingConfig{SharedModelStrategy: "random", BillingExhaustionFailover: true}},
		{"disable shared", []string{"--shared-model-strategy=off"}, gateway.RoutingConfig{BillingExhaustionFailover: true}},
		{"set both", []string{"--shared-model-strategy=random", "--billing-exhaustion-failover=false"}, gateway.RoutingConfig{SharedModelStrategy: "random"}},
		{"reenable billing", []string{"--billing-exhaustion-failover=true"}, gateway.RoutingConfig{SharedModelStrategy: "random", BillingExhaustionFailover: true}},
		{"disable billing", []string{"--billing-exhaustion-failover=false"}, gateway.RoutingConfig{SharedModelStrategy: "random"}},
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
