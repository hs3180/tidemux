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
	if err := routingCommand([]string{"show", "--config", path}, output, output); err != nil {
		t.Fatal(err)
	}
	show, err := os.ReadFile(output.Name())
	if err != nil || !strings.Contains(string(show), "disabled (legacy config value ignored)") {
		t.Fatalf("legacy routing value was not shown as ignored: output=%q err=%v", show, err)
	}

	steps := []struct {
		name string
		args []string
		want gateway.RoutingConfig
	}{
		{"enable shared clears legacy", []string{"--shared-model-strategy=random"}, gateway.RoutingConfig{SharedModelStrategy: "random"}},
		{"disable shared", []string{"--shared-model-strategy=off"}, gateway.RoutingConfig{}},
		{"set both", []string{"--shared-model-strategy=random", "--billing-exhaustion-failover=false"}, gateway.RoutingConfig{SharedModelStrategy: "random"}},
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
	if err := routingCommand([]string{"set", "--billing-exhaustion-failover=true", "--config", path}, output, output); err == nil {
		t.Fatal("enabling gateway billing failover was accepted")
	}
}
