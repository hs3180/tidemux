package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

func TestProviderBudgetResetCommandScopesProviderAndWindow(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	config := providerBudgetTestConfig(configPath)
	config.ListenAddr = "127.0.0.1:0"
	if err := writeCommandConfig(configPath, config, nil); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	store, err := ledger.Open(config.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"p1", "p2"} {
		err := store.RecordBudgetCharge(context.Background(), "unknown-"+provider, "missing-audit-"+provider, provider, "USD", now.Add(-time.Hour))
		if !errors.Is(err, ledger.ErrBudgetAuditMissing) {
			t.Fatalf("seed unknown usage for %s: got %v", provider, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	message := runBudgetResetForTest(t, configPath, "p1", "5h")
	if !strings.Contains(message, "Reset provider p1 budget window 5h") || !strings.Contains(message, "Restart the gateway") {
		t.Fatalf("unexpected reset guidance: %q", message)
	}

	store, err = ledger.Open(config.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	afterFiveHourReset := time.Now().UTC()
	fiveHour := ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 10, AlertThreshold: .8, Mode: "hard"}
	if _, err := store.CheckBudget(ctx, "p1-after-5h-reset", "p1", fiveHour, false, afterFiveHourReset); err != nil {
		t.Fatalf("selected provider's selected window stayed blocked: %v", err)
	}
	if _, err := store.CheckBudget(ctx, "p2-not-reset", "p2", fiveHour, false, afterFiveHourReset); err == nil || err.Error() != "budget_usage_unknown" {
		t.Fatalf("reset leaked to another provider: got %v", err)
	}
	weekly := ledger.BudgetPolicy{Currency: "USD", WeeklyLimit: 10, AlertThreshold: .8, Mode: "hard"}
	if _, err := store.CheckBudget(ctx, "p1-weekly-not-reset", "p1", weekly, false, afterFiveHourReset); err == nil || err.Error() != "budget_usage_unknown" {
		t.Fatalf("5h reset also cleared the 7d window: got %v", err)
	}

	message = runBudgetResetForTest(t, configPath, "p1", "7d")
	if !strings.Contains(message, "Reset provider p1 budget window 7d") {
		t.Fatalf("7d reset guidance missing: %q", message)
	}
	afterSevenDayReset := time.Now().UTC()
	if _, err := store.CheckBudget(ctx, "p1-after-7d-reset", "p1", weekly, false, afterSevenDayReset); err != nil {
		t.Fatalf("selected provider's 7d window stayed blocked: %v", err)
	}
	if _, err := store.CheckBudget(ctx, "p2-weekly-not-reset", "p2", weekly, false, afterSevenDayReset); err == nil || err.Error() != "budget_usage_unknown" {
		t.Fatalf("7d reset leaked to another provider: got %v", err)
	}
}

func TestProviderBudgetResetRefusesRunningGatewayWithoutResetting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	config := providerBudgetTestConfig(configPath)
	config.ListenAddr = listener.Addr().String()
	if err := writeCommandConfig(configPath, config, nil); err != nil {
		t.Fatal(err)
	}
	store, err := ledger.Open(config.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	err = store.RecordBudgetCharge(context.Background(), "unknown-p1", "missing-audit-p1", "p1", "USD", now.Add(-time.Hour))
	if !errors.Is(err, ledger.ErrBudgetAuditMissing) {
		t.Fatalf("seed unknown usage: got %v", err)
	}
	if err := store.Close(); err != nil {
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
	err = providerBudgetResetCommand([]string{"p1", "--window", "5h", "--config", configPath}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "gateway is still listening") {
		t.Fatalf("reset did not refuse a running gateway: %v", err)
	}

	store, err = ledger.Open(config.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	policy := ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 10, AlertThreshold: .8, Mode: "hard"}
	if _, err := store.CheckBudget(context.Background(), "p1-still-blocked", "p1", policy, false, now); err == nil || err.Error() != "budget_usage_unknown" {
		t.Fatalf("refused reset changed the provider budget: %v", err)
	}
}

func runBudgetResetForTest(t *testing.T, configPath, provider, window string) string {
	t.Helper()
	dir := filepath.Dir(configPath)
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
	if err := providerBudgetResetCommand([]string{provider, "--window", window, "--config", configPath}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
