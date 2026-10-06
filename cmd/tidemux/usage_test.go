package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestUsageExplicitConfigurePreservesProviderFieldsAndDisableHistory(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	before := providerBudgetTestConfig(path)
	if err := writeCommandConfig(path, before, nil); err != nil {
		t.Fatal(err)
	}
	stdout, _ := os.CreateTemp(directory, "stdout")
	defer stdout.Close()
	stderr, _ := os.CreateTemp(directory, "stderr")
	defer stderr.Close()
	if err := usageCommand([]string{"configure", "--config", path}, stdout, stderr); err == nil {
		t.Fatal("implicit enable accepted")
	}
	if err := usageCommand([]string{"configure", "--enable", "--disable", "--config", path}, stdout, stderr); err == nil {
		t.Fatal("ambiguous intent accepted")
	}
	output := filepath.Join(directory, "dedicated")
	if err := usageCommand([]string{"configure", "--enable", "--directory", output, "--max-files", "3", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	after, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Providers, after.Providers) || after.UsageLog == nil || !after.UsageLog.Enabled || after.UsageLog.MaxFiles != 3 {
		t.Fatalf("config=%+v", after)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("configuration unexpectedly started exporter")
	}
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(output, "usage-00000000000000000001.jsonl")
	os.WriteFile(history, []byte("retained"), 0600)
	if err := usageCommand([]string{"configure", "--disable", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var raw map[string]any
	json.Unmarshal(data, &raw)
	if _, exists := raw["usage_log"]; exists {
		t.Fatal("disable kept config setting")
	}
	if data, _ := os.ReadFile(history); string(data) != "retained" {
		t.Fatal("disable destroyed history")
	}
	if err := usageCommand([]string{"export", "--config", path}, stdout, stderr); err == nil {
		t.Fatal("disabled export accepted")
	}
}
