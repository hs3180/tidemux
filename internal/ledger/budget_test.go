package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestBudgetHardLimitReservesAtomicallyAndUnknownsStayConservative(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := BudgetPolicy{Currency: "USD", Timezone: "UTC", DailyLimit: 2, MonthlyLimit: 3, AlertThreshold: .8, Mode: "hard", ReserveAmount: 1}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if _, err := l.ReserveBudget(context.Background(), "one", p, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReserveBudget(context.Background(), "two", p, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReserveBudget(context.Background(), "three", p, false, now); err == nil || err.Error() != "budget_hard_limit" {
		t.Fatalf("err=%v", err)
	}
	if err := l.SettleBudget(context.Background(), "one", "missing-audit"); err != nil {
		t.Fatal(err)
	}
	var state string
	var charged float64
	if err := l.db.QueryRow(`SELECT state,charged_amount FROM budget_reservations WHERE request_id='one'`).Scan(&state, &charged); err != nil || state != "unknown" || charged != 1 {
		t.Fatalf("state=%s charged=%v err=%v", state, charged, err)
	}
}

func TestBudgetSoftRequiresConfirmationAndSettlesUsage(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := BudgetPolicy{Currency: "USD", Timezone: "UTC", DailyLimit: 10, MonthlyLimit: 10, AlertThreshold: .1, Mode: "soft", ReserveAmount: 1}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if _, err := l.ReserveBudget(context.Background(), "one", p, false, now); err == nil || err.Error() != "budget_confirmation_required" {
		t.Fatalf("err=%v", err)
	}
	if _, err := l.ReserveBudget(context.Background(), "one", p, true, now); err != nil {
		t.Fatal(err)
	}
	in, out := int64(1), int64(1)
	cost := .25
	if err := l.AppendAudit(Audit{ID: "audit", TimestampMS: now.UnixMilli(), Protocol: "openai", Upstream: "u", Model: "m", Status: "ok", InputTokens: &in, OutputTokens: &out, EstimatedCost: &cost, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleBudget(context.Background(), "one", "audit"); err != nil {
		t.Fatal(err)
	}
	var charged float64
	if err := l.db.QueryRow(`SELECT charged_amount FROM budget_reservations WHERE request_id='one'`).Scan(&charged); err != nil || charged != cost {
		t.Fatalf("charged=%v err=%v", charged, err)
	}
}
