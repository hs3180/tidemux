package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

// The first connection remains open: this is a live reservation, not a restart.
func TestReportListPreservesLiveBudgetReservation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	config := providerBudgetTestConfig(configPath)
	if err := writeCommandConfig(configPath, config, nil); err != nil {
		t.Fatal(err)
	}
	store, err := ledger.OpenForGateway(config.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	policy := ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 10, WeeklyLimit: 80, AlertThreshold: .8, Mode: "hard"}
	if _, err := store.CheckBudget(context.Background(), "live-request", "p1", policy, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"list"}, {"generate", "--date", "2026-10-02"},
		{"export", "--days", "1", "--output", filepath.Join(t.TempDir(), "report.html")},
		{"deliver", "--id", "999"}, {"retry", "--id", "999"},
		{"deliveries", "--id", "999"}, {"notify", "--channel", "webhook"},
	} {
		runReportWithKeychainTest(t, nil, append(args, "--config", configPath)...)
		var state string
		if err := store.QueryRow(context.Background(), "SELECT state FROM budget_charges WHERE request_id='live-request'").Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "pending" {
			t.Fatalf("report %s changed live state to %s", args[0], state)
		}
	}
	var state string
	if err := store.QueryRow(context.Background(), "SELECT state FROM budget_charges WHERE request_id='live-request'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		_, followupErr := store.CheckBudget(context.Background(), "follow-up", "p1", policy, false, time.Now())
		t.Fatalf("report list changed live reservation to %q; follow-up admission: %v", state, followupErr)
	}
}
