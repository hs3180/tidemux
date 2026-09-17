package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

func TestReportExportWritesPrivateSelfContainedHTML(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.db")
	store, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	input, output := int64(12), int64(7)
	cost := 0.75
	if err := store.AppendAudit(ledger.Audit{
		ID:          "html-export",
		TimestampMS: time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC).UnixMilli(),
		Protocol:    "openai",
		Upstream:    "test",
		Model:       "model",
		Status:      "ok",
		InputTokens: &input, OutputTokens: &output,
		EstimatedCost: &cost, Currency: "USD",
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "config.json")
	config := map[string]any{
		"protocol": "openai", "base_url": "https://example.com/v1", "model": "model", "upstream_id": "test",
		"listen_addr": "127.0.0.1:8787", "max_in_flight": 1, "ledger_path": ledgerPath,
		"upstream_keychain":     map[string]string{"service": "test.provider", "account": "default"},
		"access_token_keychain": map[string]string{"service": "test.gateway", "account": "default"},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(dir, "report.html")
	stdout, err := runReportTest(t, "report", "export", "--config", configPath, "--output", outputPath, "--days", "3", "--date", "2026-09-14", "--timezone", "UTC")
	if err != nil {
		t.Fatalf("export failed: %v; output=%s", err, stdout)
	}
	var result reportExportResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		t.Fatalf("invalid export result %q: %v", stdout, err)
	}
	if result.Path != outputPath || result.Days != 3 || result.StartDay != "2026-09-12" || result.EndDay != "2026-09-14" || result.Timezone != "UTC" {
		t.Fatalf("unexpected export result: %+v", result)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("HTML report permissions = %o, want 600", info.Mode().Perm())
	}
	html, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(html)
	for _, want := range []string{"<!doctype html>", "Daily report", "Requests per day", "Estimated cost per day", "2026-09-12", "2026-09-14", "<svg"} {
		if !strings.Contains(content, want) {
			t.Fatalf("HTML report missing %q", want)
		}
	}
	if strings.Contains(content, "<script src=") || strings.Contains(content, "https://") {
		t.Fatal("HTML report has an external dependency")
	}
	if strings.Contains(content, "html-export") {
		t.Fatal("HTML report exposed a request ID")
	}
}

func runReportTest(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		stdout.Close()
		t.Fatal(err)
	}
	runErr := run(args, stdout, stderr)
	if err := stdout.Close(); err != nil {
		stderr.Close()
		t.Fatal(err)
	}
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	return data, runErr
}
