package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBudgetCommandReplacesLegacyBudgetFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := map[string]any{
		"listen_addr":           "127.0.0.1:8787",
		"protocol":              "openai",
		"base_url":              "https://example.com/v1",
		"model":                 "custom-model",
		"upstream_id":           "test",
		"upstream_keychain":     map[string]string{"service": "test.provider", "account": "default"},
		"access_token_keychain": map[string]string{"service": "test.gateway", "account": "default"},
		"max_in_flight":         1,
		"ledger_path":           filepath.Join(dir, "ledger.db"),
		"prices":                map[string]any{"custom-model": map[string]any{"currency": "USD", "source": "test", "version": "1", "input_per_million": 1, "output_per_million": 1}},
		"budget":                map[string]any{"currency": "USD", "timezone": "UTC", "daily_limit": 5, "monthly_limit": 10, "reserve_amount": 1, "mode": "hard", "alert_threshold": .8},
	}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := budgetCommand([]string{"--config", path, "--budget-5h", "0.1", "--budget-weekly", "1"}, os.Stdin, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(updated, &raw); err != nil {
		t.Fatal(err)
	}
	budget, ok := raw["budget"].(map[string]any)
	if !ok || budget["five_hour_limit"] != 0.1 || budget["weekly_limit"] != float64(1) {
		t.Fatalf("budget=%v", raw["budget"])
	}
	if _, ok := budget["daily_limit"]; ok {
		t.Fatal("legacy daily_limit retained")
	}
}
