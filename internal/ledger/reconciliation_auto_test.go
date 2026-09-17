package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func auditForReconciliation(id, currency string, ts int64, cost *float64) Audit {
	in, out := int64(10), int64(3)
	return Audit{ID: id, TimestampMS: ts, Protocol: "openai", Upstream: "supplier", Model: "m", Status: "ok", InputTokens: &in, OutputTokens: &out, EstimatedCost: cost, Currency: currency}
}
func openReconciliationTest(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

const statementHeader = "period_start,period_end,currency,amount,request_id,model\n"

func TestReconciliationUpdatesOnSourceWritesWithoutQuery(t *testing.T) {
	l, _ := openReconciliationTest(t)
	ctx := context.Background()
	// Supplier statement and tokenizer data can arrive before the request audit.
	if _, err := l.ImportStatementCSV(ctx, strings.NewReader(statementHeader+"50,150,USD,3,late,m\n"), 200); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := l.QueryRow(ctx, `SELECT status FROM reconciliation_statements`).Scan(&status); err != nil || status != "request_not_found" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if err := l.AppendTokenComparison(ctx, TokenComparison{RequestID: "late", Tokenizer: "test", TokenizerVersion: "v1", EstimatedInput: 9, EstimatedOutput: 3, MeasuredAtMS: 150}); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendBalanceSnapshot(ctx, BalanceSnapshot{ObservedAtMS: 50, Currency: "USD", Total: 10}); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendBalanceSnapshot(ctx, BalanceSnapshot{ObservedAtMS: 150, Currency: "USD", Total: 7}); err != nil {
		t.Fatal(err)
	}
	cost := 2.5
	if err := l.AppendAudit(auditForReconciliation("late", "USD", 100, &cost)); err != nil {
		t.Fatal(err)
	}
	if err := l.QueryRow(ctx, `SELECT status FROM reconciliation_statements`).Scan(&status); err != nil || status != "matched" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	var lines, compared, mismatch int
	var amount, intervalCost float64
	if err := l.QueryRow(ctx, `SELECT statement_lines,supplier_amount,tokenizer_compared,tokenizer_mismatch FROM reconciliation_requests WHERE request_id='late'`).Scan(&lines, &amount, &compared, &mismatch); err != nil {
		t.Fatal(err)
	}
	if lines != 1 || amount != 3 || compared != 1 || mismatch != 1 {
		t.Fatalf("materialized request: %d %g %d %d", lines, amount, compared, mismatch)
	}
	if err := l.QueryRow(ctx, `SELECT local_estimated FROM reconciliation_balance_intervals WHERE from_ms=50`).Scan(&intervalCost); err != nil || intervalCost != 2.5 {
		t.Fatalf("interval cost=%g err=%v", intervalCost, err)
	}
	report, err := l.ReconciliationStats(ctx, 50, 150)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || report[0].Coverage != "complete" || report[0].Difference == nil || *report[0].Difference != 0.5 {
		t.Fatalf("report=%+v", report)
	}
	// A later supplier line and tokenizer measurement update an existing audit too.
	if err := l.AppendAudit(auditForReconciliation("early", "EUR", 100, &cost)); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendTokenComparison(ctx, TokenComparison{RequestID: "early", Tokenizer: "test", TokenizerVersion: "v1", EstimatedInput: 10, EstimatedOutput: 3, MeasuredAtMS: 150}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ImportStatementCSV(ctx, strings.NewReader(statementHeader+"50,150,EUR,2.5,early,m\n"), 200); err != nil {
		t.Fatal(err)
	}
	if err := l.QueryRow(ctx, `SELECT statement_lines,tokenizer_compared,tokenizer_mismatch FROM reconciliation_requests WHERE request_id='early'`).Scan(&lines, &compared, &mismatch); err != nil || lines != 1 || compared != 1 || mismatch != 0 {
		t.Fatalf("materialized existing request: %d %d %d err=%v", lines, compared, mismatch, err)
	}
}

func TestReconciliationBackfillsExistingLedger(t *testing.T) {
	l, path := openReconciliationTest(t)
	ctx := context.Background()
	_, err := l.db.Exec(`DROP TRIGGER reconciliation_audit_insert; DROP TRIGGER reconciliation_token_insert; DROP TRIGGER reconciliation_statement_insert; DROP TRIGGER reconciliation_balance_insert; DELETE FROM reconciliation_schema;`)
	if err != nil {
		t.Fatal(err)
	}
	cost := 2.5
	if err = l.AppendAudit(auditForReconciliation("historical", "USD", 100, &cost)); err != nil {
		t.Fatal(err)
	}
	if _, err = l.ImportStatementCSV(ctx, strings.NewReader(statementHeader+"50,150,USD,3,historical,m\n"), 200); err != nil {
		t.Fatal(err)
	}
	if err = l.AppendTokenComparison(ctx, TokenComparison{RequestID: "historical", Tokenizer: "test", TokenizerVersion: "v1", EstimatedInput: 9, EstimatedOutput: 3, MeasuredAtMS: 150}); err != nil {
		t.Fatal(err)
	}
	if err = l.AppendBalanceSnapshot(ctx, BalanceSnapshot{ObservedAtMS: 50, Currency: "USD", Total: 10}); err != nil {
		t.Fatal(err)
	}
	if err = l.AppendBalanceSnapshot(ctx, BalanceSnapshot{ObservedAtMS: 150, Currency: "USD", Total: 7}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = l.QueryRow(ctx, `SELECT COUNT(*) FROM reconciliation_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("pre-backfill rows=%d err=%v", n, err)
	}
	l.Close()
	l, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = l.QueryRow(ctx, `SELECT COUNT(*) FROM reconciliation_requests WHERE statement_lines=1 AND tokenizer_mismatch=1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backfilled rows=%d err=%v", n, err)
	}
	if err = l.QueryRow(ctx, `SELECT COUNT(*) FROM reconciliation_statements WHERE status='matched'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backfilled statements=%d err=%v", n, err)
	}
	var estimate float64
	if err = l.QueryRow(ctx, `SELECT local_estimated FROM reconciliation_balance_intervals WHERE from_ms=50`).Scan(&estimate); err != nil || estimate != cost {
		t.Fatalf("backfilled balance estimate=%g err=%v", estimate, err)
	}
}

func TestStatementImportPersistsDedupAndImmutableSources(t *testing.T) {
	l, path := openReconciliationTest(t)
	ctx := context.Background()
	data := statementHeader + "50,150,USD,3,same,m\n50,150,USD,3,same,m\n"
	n, err := l.ImportStatementFile(ctx, "original.csv", strings.NewReader(data), 200)
	if err != nil || n != 2 {
		t.Fatalf("first import=%d err=%v", n, err)
	}
	l.Close()
	l, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	n, err = l.ImportStatementFile(ctx, "renamed.csv", strings.NewReader(data), 300)
	if err != nil || n != 0 {
		t.Fatalf("renamed duplicate=%d err=%v", n, err)
	}
	n, err = l.ImportStatementCSV(ctx, strings.NewReader(data), 400)
	if err != nil || n != 0 {
		t.Fatalf("anonymous duplicate=%d err=%v", n, err)
	}
	if _, err = l.ImportStatementFile(ctx, "renamed.csv", strings.NewReader(data+"50,150,USD,4,new,m\n"), 500); err == nil {
		t.Fatal("accepted changed previously imported source")
	}
	var count int
	if err = l.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_statement_lines`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
	if err = l.QueryRow(ctx, `SELECT COUNT(*) FROM statement_imports`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("fingerprints=%d err=%v", count, err)
	}
}

func TestStatementBatchInvalidRowsAreAtomicAndInputBounded(t *testing.T) {
	l, _ := openReconciliationTest(t)
	ctx := context.Background()
	bad := statementHeader + "50,150,USD,3,ok,m\n50,150,USD,-1,bad,m\n"
	if _, err := l.ImportStatementFile(ctx, "fixable.csv", strings.NewReader(bad), 200); err == nil {
		t.Fatal("accepted invalid batch")
	}
	for _, table := range []string{"supplier_statement_lines", "reconciliation_statements", "statement_imports", "statement_import_sources"} {
		var n int
		if err := l.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s rows=%d err=%v", table, n, err)
		}
	}
	good := statementHeader + "50,150,USD,3,ok,m\n"
	if n, err := l.ImportStatementFile(ctx, "fixable.csv", strings.NewReader(good), 300); err != nil || n != 1 {
		t.Fatalf("corrected import=%d err=%v", n, err)
	}
	oversized := io.MultiReader(strings.NewReader(statementHeader), io.LimitReader(zeroReader{}, MaxStatementBytes))
	if _, err := l.ImportStatementCSV(ctx, oversized, 400); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized error=%v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestReconciliationScopesCurrenciesAndPeriods(t *testing.T) {
	l, _ := openReconciliationTest(t)
	ctx := context.Background()
	cost := 2.0
	for _, a := range []Audit{auditForReconciliation("usd", "USD", 100, &cost), auditForReconciliation("eur", "EUR", 100, &cost)} {
		if err := l.AppendAudit(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.AppendTokenComparison(ctx, TokenComparison{RequestID: "usd", Tokenizer: "t", TokenizerVersion: "v", EstimatedInput: 9, EstimatedOutput: 3, MeasuredAtMS: 101}); err != nil {
		t.Fatal(err)
	}
	data := statementHeader + "50,150,USD,3,usd,m\n50,150,EUR,4,usd,m\n50,150,EUR,5,eur,other-model\n200,300,EUR,6,eur,m\n"
	if _, err := l.ImportStatementCSV(ctx, strings.NewReader(data), 400); err != nil {
		t.Fatal(err)
	}
	for _, s := range []BalanceSnapshot{{ObservedAtMS: 40, Currency: "USD", Total: 12}, {ObservedAtMS: 50, Currency: "USD", Total: 10}, {ObservedAtMS: 150, Currency: "USD", Total: 7}, {ObservedAtMS: 160, Currency: "USD", Total: 5}, {ObservedAtMS: 100, Currency: "EUR", Total: 10}} {
		if err := l.AppendBalanceSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := l.Reconcile(ctx, 50, 150)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	eur, usd := rows[0], rows[1]
	if eur.TokenizerComparisons != 0 || usd.TokenizerComparisons != 1 || usd.TokenizerMismatches != 1 {
		t.Fatalf("currency tokenizer rows=%+v", rows)
	}
	if eur.MatchedLines != 0 || eur.UnmatchedLines != 2 || eur.MissingStatementRequests != 1 || eur.BalanceNetChange != nil {
		t.Fatalf("eur=%+v", eur)
	}
	if usd.BalanceNetChange == nil || *usd.BalanceNetChange != -3 || usd.UnattributedBalanceDelta == nil || *usd.UnattributedBalanceDelta != -1 {
		t.Fatalf("usd=%+v", usd)
	}
	rows, err = l.Reconcile(ctx, 90, 110)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.SupplierStatement != 0 || row.StatementLines != 0 || row.PartialStatementLines == 0 || row.Coverage != "partial" || row.Difference != nil || row.BalanceNetChange != nil {
			t.Fatalf("partial period=%+v", row)
		}
	}
}

func TestNoStatementAndUnknownCostNeverLookReconciled(t *testing.T) {
	l, _ := openReconciliationTest(t)
	ctx := context.Background()
	cost := 2.0
	if err := l.AppendAudit(auditForReconciliation("pending", "USD", 100, &cost)); err != nil {
		t.Fatal(err)
	}
	rows, err := l.Reconcile(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Coverage != "no_statement" || rows[0].Difference != nil || rows[0].MissingStatementRequests != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	if err := l.AppendAudit(auditForReconciliation("unknown", "EUR", 100, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ImportStatementCSV(ctx, strings.NewReader(statementHeader+"50,150,EUR,3,unknown,m\n"), 200); err != nil {
		t.Fatal(err)
	}
	rows, err = l.Reconcile(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].UnknownCostRequests != 1 || rows[0].Coverage != "partial" || rows[0].Difference != nil || rows[0].MatchedDifference != nil {
		t.Fatalf("unknown=%+v", rows[0])
	}
}

func TestOpenReadOnlyAndBillingExport(t *testing.T) {
	l, path := openReconciliationTest(t)
	ctx := context.Background()
	cost := 2.5
	if err := l.AppendAudit(auditForReconciliation("known", "USD", 100, &cost)); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendAudit(auditForReconciliation("unknown", "USD", 101, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ImportStatementCSV(ctx, strings.NewReader(statementHeader+"50,150,USD,3,known,m\n50,150,USD,1,,m\n"), 200); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordStatementSync(ctx, 200, 1, 2, 0); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordStatementSync(ctx, 300, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	l.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err = ro.AppendAudit(auditForReconciliation("forbidden", "USD", 110, &cost)); err == nil {
		t.Fatal("read-only audit write succeeded")
	}
	if _, err = ro.db.Exec(`CREATE TABLE forbidden(id INTEGER)`); err == nil {
		t.Fatal("read-only schema write succeeded")
	}
	sync, err := ro.StatementSyncStatus(ctx)
	if err != nil || sync.LastAttemptMS != 300 || sync.LastSuccessMS == nil || *sync.LastSuccessMS != 200 || sync.Failures != 1 {
		t.Fatalf("sync=%+v err=%v", sync, err)
	}
	var out bytes.Buffer
	if err = ro.ExportBillingCSV(ctx, &out, 0, 0); err != nil {
		t.Fatal(err)
	}
	csvRows, err := csv.NewReader(&out).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(csvRows) != 5 {
		t.Fatalf("CSV=%v", csvRows)
	}
	if csvRows[1][7] != "2.5" || csvRows[1][8] != "" || csvRows[2][7] != "" || csvRows[2][9] != "unknown_cost_missing_statement" || csvRows[3][8] != "3" || csvRows[4][9] != "missing_request_id" {
		t.Fatalf("CSV=%v", csvRows)
	}
	if _, err = ro.Reconcile(ctx, 0, 0); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only query changed database")
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err = OpenReadOnly(missing); err == nil {
		t.Fatal("read-only open accepted missing ledger")
	}
	if _, err = os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only open created missing ledger")
	}
}

func TestReadOnlyDoesNotMigrateLegacyLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE request_audit(id TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenReadOnly(path); err == nil {
		t.Fatal("read-only open accepted uninitialized reconciliation")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only open migrated legacy ledger")
	}
}

func TestCanceledStatementImportIsAtomic(t *testing.T) {
	l, _ := openReconciliationTest(t)
	data := statementHeader + strings.Repeat("50,150,USD,3,pending,m\n", 100000)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.ImportStatementFile(ctx, "canceled.csv", strings.NewReader(data), 200); err == nil {
		t.Fatal("large import ignored cancellation")
	}
	for _, table := range []string{"supplier_statement_lines", "reconciliation_statements", "statement_imports", "statement_import_sources"} {
		var count int
		if err := l.QueryRow(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows=%d err=%v", table, count, err)
		}
	}
	cost := 1.0
	if err := l.AppendAudit(auditForReconciliation("after-cancel", "USD", 100, &cost)); err != nil {
		t.Fatalf("audit after canceled import: %v", err)
	}
}

func BenchmarkStatementImport1000Rows(b *testing.B) {
	data := statementHeader + strings.Repeat("50,150,USD,3,pending,m\n", 1000)
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		l, err := Open(filepath.Join(b.TempDir(), "ledger.db"))
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if count, err := l.ImportStatementCSV(context.Background(), strings.NewReader(data), 200); err != nil || count != 1000 {
			b.Fatalf("count=%d err=%v", count, err)
		}
		b.StopTimer()
		l.Close()
	}
}
