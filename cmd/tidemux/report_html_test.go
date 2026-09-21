package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

	defaultPath := defaultReportPath(ledgerPath)
	stdout, err = runReportTest(t, "report", "export", "--config", configPath, "--days", "3", "--date", "2026-09-14", "--timezone", "UTC")
	if err != nil {
		t.Fatalf("default export failed: %v; output=%s", err, stdout)
	}
	if err := json.Unmarshal(stdout, &result); err != nil {
		t.Fatalf("invalid default export result %q: %v", stdout, err)
	}
	if result.Path != defaultPath {
		t.Fatalf("default export path = %q, want %q", result.Path, defaultPath)
	}
	if info, err := os.Stat(filepath.Dir(defaultPath)); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("report directory permissions = %o, want 700", info.Mode().Perm())
	}

	fakeBin := filepath.Join(dir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	openCapture := filepath.Join(dir, "open-target")
	if err := os.WriteFile(filepath.Join(fakeBin, "open"), []byte("#!/bin/sh\nprintf '%s' \"$1\" > \"$TIDEMUX_OPEN_CAPTURE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "terminal-notifier"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TIDEMUX_NOTIFY_CAPTURE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	notifyCapture := filepath.Join(dir, "notify-args")
	t.Setenv("TIDEMUX_OPEN_CAPTURE", openCapture)
	t.Setenv("TIDEMUX_NOTIFY_CAPTURE", notifyCapture)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := runReportTest(t, "report", "open", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	opened, err := os.ReadFile(openCapture)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != defaultPath {
		t.Fatalf("report open target = %q, want %q", opened, defaultPath)
	}

	generated, err := runReportTest(t, "report", "generate", "--config", configPath, "--date", "2026-09-14", "--timezone", "UTC")
	if err != nil {
		t.Fatalf("generate failed: %v; output=%s", err, generated)
	}
	var daily ledger.DailyReport
	if err := json.Unmarshal(generated, &daily); err != nil {
		t.Fatalf("invalid generated report %q: %v", generated, err)
	}
	delivered, err := runReportTest(t, "report", "deliver", "--config", configPath, "--id", strconv.FormatInt(daily.ID, 10), "--channel", "macos", "--days", "3")
	if err != nil {
		t.Fatalf("deliver failed: %v; output=%s", err, delivered)
	}
	if !strings.Contains(string(delivered), `"status":"sent"`) {
		t.Fatalf("unexpected delivery result: %s", delivered)
	}
	notificationArgs, err := os.ReadFile(notifyCapture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(notificationArgs)), "\n")
	if !containsReportNotificationArgs(args, defaultPath) {
		t.Fatalf("notification args do not open %q: %v", defaultPath, args)
	}
}

func containsReportNotificationArgs(args []string, reportPath string) bool {
	wantURL := (&url.URL{Scheme: "file", Path: reportPath}).String()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-open" && args[i+1] == wantURL {
			return true
		}
	}
	return false
}

func TestNotifyMacOSOpensNotificationSettingsAfterPermissionFailure(t *testing.T) {
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "terminal-notifier"), []byte("#!/bin/sh\nprintf '%s\\n' 'Notifications are not allowed for this application' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	openCapture := filepath.Join(dir, "open-target")
	if err := os.WriteFile(filepath.Join(fakeBin, "open"), []byte("#!/bin/sh\nprintf '%s' \"$1\" > \"$TIDEMUX_SETTINGS_CAPTURE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TIDEMUX_SETTINGS_CAPTURE", openCapture)
	t.Setenv("PATH", fakeBin)
	if err := notifyMacOS("TideMux", "Report ready", filepath.Join(dir, "report.html")); err == nil || !strings.Contains(err.Error(), "notification settings opened") {
		t.Fatalf("notifyMacOS error = %v, want permission recovery", err)
	}
	target, err := os.ReadFile(openCapture)
	if err != nil {
		t.Fatal(err)
	}
	if string(target) != macOSNotificationSettingsURL {
		t.Fatalf("settings target = %q, want %q", target, macOSNotificationSettingsURL)
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
