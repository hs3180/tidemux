package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestInstanceAutoChainCommandsValidateAndPreserveConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := providerBudgetTestConfig(path)
	for ref, provider := range c.Providers {
		provider.SupportedModels = []string{"model-one", "model-two", "vendor/model"}
		c.Providers[ref] = provider
	}
	if _, exists := c.Providers["p2"]; !exists {
		provider := c.Providers["p1"]
		provider.UpstreamID = "p2"
		c.Providers["p2"] = provider
	}
	if err := writeCommandConfig(path, c, nil); err != nil {
		t.Fatal(err)
	}
	before, err := gateway.LoadConfig(path)
	if err != nil {
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
	if err := run([]string{"auto-chain", "set", "--config", path, "--entries", "p2/model-two,p1/vendor/model,p1/model-one"}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []gateway.AutoChainEntry{{Provider: "p2", Model: "model-two"}, {Provider: "p1", Model: "vendor/model"}, {Provider: "p1", Model: "model-one"}}
	if !reflect.DeepEqual(loaded.AutoChain, want) || !reflect.DeepEqual(loaded.Providers, before.Providers) {
		t.Fatalf("order or unrelated provider settings changed: %+v", loaded)
	}
	if err := autoChainCommand([]string{"show", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	output, _ := os.ReadFile(stdout.Name())
	if !strings.Contains(string(output), "1\tp2/model-two\n2\tp1/vendor/model\n3\tp1/model-one") {
		t.Fatalf("configured chain not inspectable: %s", output)
	}
	saved, _ := os.ReadFile(path)
	for _, entries := range []string{"missing/model-one", "p1/model-missing", "p1/auto", "p1/model-one,p1/model-one", "p1", ""} {
		if err := autoChainCommand([]string{"set", "--entries", entries, "--config", path}, stdout, stderr); err == nil {
			t.Fatalf("invalid entries accepted: %q", entries)
		}
		data, _ := os.ReadFile(path)
		if !bytes.Equal(data, saved) {
			t.Fatal("failed validation changed the saved chain")
		}
	}
	if err := autoChainCommand([]string{"set", "p1", "model-one", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, _ = gateway.LoadConfig(path)
	if len(loaded.AutoChain) != 1 || loaded.AutoChain[0].Provider != "p1" {
		t.Fatal("set did not replace the single instance chain")
	}
	if err := autoChainCommand([]string{"clear", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"auto_chain"`) {
		t.Fatal("clear did not omit the optional field for v0.2.2 rollback")
	}
	if err := providerCommand([]string{"auto-chain", "p1", "--config", path}, stdout, stderr); err == nil {
		t.Fatal("obsolete provider-local chain command still accepted")
	}
}
