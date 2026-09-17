package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDailyReportIsContentFreeAndPersistent(t *testing.T) {
	l, e := Open(filepath.Join(t.TempDir(), "l.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	in, out := int64(3), int64(2)
	cost := .5
	at := time.Date(2026, 9, 14, 7, 0, 0, 0, time.UTC)
	if e = l.AppendAudit(Audit{ID: "a", TimestampMS: at.UnixMilli(), Protocol: "openai", Upstream: "u", Model: "m", Status: "ok", InputTokens: &in, OutputTokens: &out, EstimatedCost: &cost, Currency: "USD"}); e != nil {
		t.Fatal(e)
	}
	r, e := l.GenerateDailyReport(context.Background(), at, "UTC", ReportBudget{})
	if e != nil || r.RequestCount != 1 || r.EstimatedCost == nil || *r.EstimatedCost != .5 || r.ID < 1 {
		t.Fatalf("r=%+v e=%v", r, e)
	}
	rows, e := l.ListDailyReports(context.Background(), 1)
	if e != nil || len(rows) != 1 || rows[0].InputTokens != 3 || rows[0].ID != r.ID {
		t.Fatalf("rows=%+v e=%v", rows, e)
	}
	if e = l.RecordDelivery(context.Background(), r.ID, "macos", "failed", "delivery_failed"); e != nil {
		t.Fatal(e)
	}
	d, e := l.ReportDeliveries(context.Background(), r.ID)
	if e != nil || len(d) != 1 || d[0].Attempts != 1 {
		t.Fatalf("d=%+v e=%v", d, e)
	}
}

func TestDailyReportSeriesIsChronologicalAndDoesNotPersist(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cost := 0.25
	at := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	for i, day := range []time.Time{at, at.AddDate(0, 0, 2)} {
		input, output := int64(i+1), int64(i+2)
		if err := l.AppendAudit(Audit{ID: string(rune('a' + i)), TimestampMS: day.UnixMilli(), Protocol: "openai", Upstream: "u", Model: "m", Status: "ok", InputTokens: &input, OutputTokens: &output, EstimatedCost: &cost, Currency: "USD"}); err != nil {
			t.Fatal(err)
		}
	}
	reports, err := l.DailyReportSeries(context.Background(), at.AddDate(0, 0, 2), "UTC", 3, ReportBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 || reports[0].Day != "2026-09-12" || reports[1].Day != "2026-09-13" || reports[2].Day != "2026-09-14" {
		t.Fatalf("unexpected days: %+v", reports)
	}
	if reports[0].RequestCount != 1 || reports[1].RequestCount != 0 || reports[2].RequestCount != 1 {
		t.Fatalf("unexpected request counts: %+v", reports)
	}
	if reports[0].ID != 0 || reports[1].ID != 0 || reports[2].ID != 0 {
		t.Fatalf("series unexpectedly persisted IDs: %+v", reports)
	}
	var persisted int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM daily_reports`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 0 {
		t.Fatalf("series persisted %d rows", persisted)
	}
}

func TestReportOptionalSourcesAndStableIdentity(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Model a ledger created before optional features were installed. Current
	// main also adds reconciliation triggers and views over these sources, so
	// remove those dependents before removing the optional tables.
	if _, err := l.db.Exec(`DROP TRIGGER IF EXISTS reconciliation_audit_insert;
 DROP TRIGGER IF EXISTS reconciliation_token_insert;
 DROP TRIGGER IF EXISTS reconciliation_statement_insert;
 DROP TRIGGER IF EXISTS reconciliation_balance_insert;
 DROP VIEW IF EXISTS reconciliation_request_source;
 DROP VIEW IF EXISTS reconciliation_statement_source;
 DROP VIEW IF EXISTS reconciliation_balance_source;
 DROP TABLE IF EXISTS token_comparisons;
 DROP TABLE IF EXISTS supplier_statement_lines;
 DROP TABLE IF EXISTS balance_snapshots;
 DROP TABLE IF EXISTS budget_reservations;`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Date(2026, 9, 14, 7, 0, 0, 0, time.UTC)
	cost := 0.5
	in, out := int64(3), int64(2)
	if err := l.AppendAudit(Audit{ID: "a", TimestampMS: at.UnixMilli(), Protocol: "openai", Upstream: "u", Model: "m", Status: "ok", InputTokens: &in, OutputTokens: &out, EstimatedCost: &cost, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	budget := ReportBudget{Currency: "USD", DailyLimit: 2}
	first, err := l.GenerateDailyReport(ctx, at, "UTC", budget)
	if err != nil {
		t.Fatal(err)
	}
	if first.TokenizerMismatches != nil || first.UnmatchedStatements != nil || first.BalanceNetChange != nil || first.BudgetRemaining != nil {
		t.Fatalf("missing extensions must remain unknown: %+v", first)
	}
	// A standalone report must not create or require another feature's schema.
	for _, table := range []string{"token_comparisons", "supplier_statement_lines", "balance_snapshots", "budget_reservations"} {
		exists, err := l.reportTableExists(ctx, table)
		if err != nil || exists {
			t.Fatalf("unexpected table %s: %v", table, err)
		}
	}
	// Fixtures model installed extension contracts without importing their code.
	_, err = l.db.Exec(`CREATE TABLE token_comparisons(request_id TEXT,estimated_input INTEGER,estimated_output INTEGER);
 INSERT INTO token_comparisons VALUES ('a',4,2);
 CREATE TABLE supplier_statement_lines(period_start_ms INTEGER,request_id TEXT);
 CREATE TABLE balance_snapshots(observed_at_ms INTEGER,currency TEXT,total REAL);
 CREATE TABLE budget_reservations(reserved_at_ms INTEGER,currency TEXT,charged_amount REAL);`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.db.Exec(`INSERT INTO supplier_statement_lines VALUES (?,NULL)`, at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = l.db.Exec(`INSERT INTO balance_snapshots VALUES (?,'USD',10),(?,'USD',9.5)`, at.UnixMilli(), at.Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = l.db.Exec(`INSERT INTO budget_reservations VALUES (?,'USD',0.5)`, at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// Create another report first so LastInsertId cannot masquerade as the old ID.
	if _, err = l.GenerateDailyReport(ctx, at.AddDate(0, 0, 1), "UTC", budget); err != nil {
		t.Fatal(err)
	}
	enriched, err := l.GenerateDailyReport(ctx, at, "UTC", budget)
	if err != nil {
		t.Fatal(err)
	}
	if enriched.ID != first.ID || enriched.TokenizerMismatches == nil || *enriched.TokenizerMismatches != 1 || enriched.UnmatchedStatements == nil || *enriched.UnmatchedStatements != 1 || enriched.BalanceNetChange == nil || *enriched.BalanceNetChange != -0.5 || enriched.BudgetRemaining == nil || *enriched.BudgetRemaining != 1.5 {
		t.Fatalf("unexpected report: %+v", enriched)
	}
	if _, err = l.db.Exec(`DROP TABLE token_comparisons; CREATE TABLE token_comparisons(broken TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err = l.GenerateDailyReport(ctx, at, "UTC", budget); err == nil {
		t.Fatal("installed but broken sources must not silently become zero")
	}
}
