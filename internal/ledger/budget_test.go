package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestBudgetPolicyUsesRollingWindowJSONFields(t *testing.T) {
	var p BudgetPolicy
	data := `{"currency":"USD","five_hour_limit":5,"weekly_limit":80,"alert_threshold":0.8,"mode":"hard"}`
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Currency != "USD" || p.FiveHourLimit != 5 || p.WeeklyLimit != 80 || p.AlertThreshold != .8 || p.Mode != "hard" {
		t.Fatalf("decoded policy: %+v", p)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != data {
		t.Fatalf("encoded policy: %s", encoded)
	}
}

func TestBudgetHardLimitUsesSettledCharges(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := BudgetPolicy{Currency: "USD", FiveHourLimit: 2, WeeklyLimit: 3, AlertThreshold: .8, Mode: "hard"}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if _, err := l.CheckBudget(context.Background(), "one", "provider-a", p, false, now); err != nil {
		t.Fatal(err)
	}
	in, out := int64(1), int64(1)
	cost := 2.0
	if err := l.AppendAudit(Audit{ID: "audit", TimestampMS: now.UnixMilli(), Protocol: "openai", Upstream: "u", Model: "m", Status: "ok", InputTokens: &in, OutputTokens: &out, EstimatedCost: &cost, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordBudgetCharge(context.Background(), "one", "audit", "provider-a", "USD", now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.CheckBudget(context.Background(), "two", "provider-a", p, false, now); err == nil || err.Error() != "budget_hard_limit" {
		t.Fatalf("err=%v", err)
	}
}

func TestUnknownUsageBlocksFutureBudgetRequests(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := BudgetPolicy{Currency: "USD", FiveHourLimit: 10, WeeklyLimit: 10, AlertThreshold: .8, Mode: "hard"}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if err := l.RecordBudgetCharge(context.Background(), "one", "missing-audit", "provider-a", "USD", now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.CheckBudget(context.Background(), "two", "provider-a", p, false, now); err == nil || err.Error() != "budget_usage_unknown" {
		t.Fatalf("err=%v", err)
	}
}

func TestBudgetSoftRequiresConfirmation(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := BudgetPolicy{Currency: "USD", FiveHourLimit: 10, WeeklyLimit: 10, AlertThreshold: .1, Mode: "soft"}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	in, out := int64(1), int64(1)
	cost := 2.0
	if err := l.AppendAudit(Audit{ID: "audit", TimestampMS: now.UnixMilli(), Protocol: "openai", Upstream: "u", Model: "m", Status: "ok", InputTokens: &in, OutputTokens: &out, EstimatedCost: &cost, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordBudgetCharge(context.Background(), "one", "audit", "provider-a", "USD", now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.CheckBudget(context.Background(), "two", "provider-a", p, false, now); err == nil || err.Error() != "budget_confirmation_required" {
		t.Fatalf("err=%v", err)
	}
	if decision, err := l.CheckBudget(context.Background(), "two", "provider-a", p, true, now); err != nil || !decision.Warning {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestPendingBudgetChargeBecomesUnknownAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := BudgetPolicy{Currency: "USD", FiveHourLimit: 10, WeeklyLimit: 10, AlertThreshold: .8, Mode: "hard"}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if _, err := l.CheckBudget(context.Background(), "in-flight", "provider-a", p, false, now); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.CheckBudget(context.Background(), "next", "provider-a", p, false, now); err == nil || err.Error() != "budget_usage_unknown" {
		t.Fatalf("err=%v", err)
	}
}

func TestProviderBudgetsDoNotCountOtherProviders(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .8, Mode: "hard"}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	in, out := int64(1), int64(1)
	cost := 2.0
	if err := l.AppendAudit(Audit{ID: "audit-a", TimestampMS: now.UnixMilli(), Protocol: "openai", Upstream: "provider-a", Model: "m", Status: "ok", InputTokens: &in, OutputTokens: &out, EstimatedCost: &cost, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordBudgetCharge(context.Background(), "request-a", "audit-a", "provider-a", "USD", now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.CheckBudget(context.Background(), "request-a2", "provider-a", p, false, now); err == nil || err.Error() != "budget_hard_limit" {
		t.Fatalf("provider-a budget did not include its charge: %v", err)
	}
	if _, err := l.CheckBudget(context.Background(), "request-b", "provider-b", p, false, now); err != nil {
		t.Fatalf("provider-a charge leaked into provider-b budget: %v", err)
	}
}

func TestLegacyUnscopedBudgetChargesMigrateConservatively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE budget_charges (
 request_id TEXT PRIMARY KEY, audit_id TEXT, charged_at_ms INTEGER NOT NULL,
 currency TEXT NOT NULL, charged_amount REAL NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','settled','unknown')));
 INSERT INTO budget_charges VALUES ('legacy','audit',1790000000000,'USD',2,'settled');`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 3, AlertThreshold: .8, Mode: "hard"}
	now := time.UnixMilli(1790000000000).UTC()
	if _, err := l.CheckBudget(context.Background(), "provider-a-next", "provider-a", p, false, now); err == nil || err.Error() != "budget_hard_limit" {
		t.Fatalf("legacy unscoped charge not conservatively counted for provider-a: %v", err)
	}
	if _, err := l.CheckBudget(context.Background(), "provider-b-next", "provider-b", p, false, now); err == nil || err.Error() != "budget_hard_limit" {
		t.Fatalf("legacy unscoped charge not conservatively counted for provider-b: %v", err)
	}
}
