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
