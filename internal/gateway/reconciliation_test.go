package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

const statementCSV = "period_start,period_end,currency,amount,request_id,model\n1,1000,USD,2,request,m\n"

func TestGatewayAutomaticallyImportsStatementsOnStartupAndRestart(t *testing.T) {
	dir := t.TempDir()
	statements := filepath.Join(dir, "statements")
	if err := os.Mkdir(statements, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statements, "bill.csv"), []byte(statementCSV), 0600); err != nil {
		t.Fatal(err)
	}
	c := testConfig(filepath.Join(dir, "ledger.db"), "http://127.0.0.1:1")
	var lastAttempt int64
	for i := 0; i < 2; i++ {
		time.Sleep(2 * time.Millisecond)
		_, closeGateway, err := NewHandler(c, nil)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer closeGateway()
			l, err := ledger.OpenReadOnly(c.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			waitForSync(t, l, func(s ledger.StatementSync) bool { return s.LastSuccessMS != nil && s.LastAttemptMS > lastAttempt })
			status, err := l.StatementSyncStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			lastAttempt = status.LastAttemptMS
			var count int
			if err := l.QueryRow(context.Background(), "SELECT COUNT(*) FROM supplier_statement_lines").Scan(&count); err != nil || count != 1 {
				t.Fatalf("startup %d: count=%d error=%v", i, count, err)
			}
		}()
	}
}

func TestAutomaticStatementSyncRetriesAndStops(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	statements := filepath.Join(dir, "statements")
	if err := os.Mkdir(statements, 0700); err != nil {
		t.Fatal(err)
	}
	bill := filepath.Join(statements, "bill.csv")
	if err := os.WriteFile(bill, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	stop := runStatementSync(l, statements, 10*time.Millisecond)
	defer stop()
	waitForSync(t, l, func(s ledger.StatementSync) bool { return s.Failures > 0 })
	// The external exporter publishes complete files using an atomic rename.
	if err := os.WriteFile(bill+".tmp", []byte(statementCSV), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bill+".tmp", bill); err != nil {
		t.Fatal(err)
	}
	waitForSync(t, l, func(s ledger.StatementSync) bool { return s.LastSuccessMS != nil && s.Failures == 0 })
	stop()
	status, err := l.StatementSyncStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statements, "later.csv"), []byte("period_start,period_end,currency,amount\n1000,2000,USD,3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Waiting longer than the poll interval proves the stopped worker cannot ingest.
	time.Sleep(30 * time.Millisecond)
	var count int
	if err := l.QueryRow(context.Background(), "SELECT COUNT(*) FROM supplier_statement_lines").Scan(&count); err != nil || count != 1 {
		t.Fatalf("after shutdown: count=%d error=%v", count, err)
	}
	after, err := l.StatementSyncStatus(context.Background())
	if err != nil || after.LastAttemptMS != status.LastAttemptMS {
		t.Fatalf("sync ran after stop: before=%+v after=%+v error=%v", status, after, err)
	}
}

func TestStatementScanRejectsChangedFilesAndSkipsNonStatements(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	statements := filepath.Join(dir, "statements")
	syncStatements(context.Background(), l, statements)
	for name, content := range map[string]string{"bill.csv": statementCSV, "ignored.tmp": "bad", ".partial.csv": "bad"} {
		if err := os.WriteFile(filepath.Join(statements, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	syncStatements(context.Background(), l, statements)
	s, err := l.StatementSyncStatus(context.Background())
	if err != nil || s.FilesImported != 1 || s.LinesImported != 1 || s.Failures != 0 {
		t.Fatalf("status=%+v error=%v", s, err)
	}
	if err := os.WriteFile(filepath.Join(statements, "bill.csv"), []byte(statementCSV+"1000,2000,USD,3,,m\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(statements, "bill.csv"), filepath.Join(statements, "link.csv")); err != nil {
		t.Fatal(err)
	}
	syncStatements(context.Background(), l, statements)
	s, err = l.StatementSyncStatus(context.Background())
	if err != nil || s.Failures != 2 || s.LinesImported != 0 {
		t.Fatalf("changed/symlink status=%+v error=%v", s, err)
	}
	var count int
	if err := l.QueryRow(context.Background(), "SELECT COUNT(*) FROM supplier_statement_lines").Scan(&count); err != nil || count != 1 {
		t.Fatalf("changed bill was duplicated: count=%d error=%v", count, err)
	}
}

func TestStatementSyncUnavailableDirectoryDoesNotBlockGateway(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	c := testConfig(filepath.Join(dir, "ledger.db"), "http://127.0.0.1:1")
	c.Reconciliation.StatementDir = blocked
	_, closeGateway, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	l, err := ledger.OpenReadOnly(c.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	waitForSync(t, l, func(s ledger.StatementSync) bool { return s.Failures > 0 && s.LastSuccessMS == nil })
}

func TestSlowStatementImportLeavesTimeForRequestAuditing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Make one import deterministically wait for its context deadline, regardless
	// of machine speed. The recursive query uses constant memory and is interrupted
	// by SQLite when the automatic import's context expires.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TRIGGER slow_statement BEFORE INSERT ON supplier_statement_lines BEGIN
		WITH RECURSIVE busy(n) AS (SELECT 0 UNION ALL SELECT n FROM busy)
		SELECT sum(n) FROM busy;
	END`)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("prepare slow import: %v; close: %v", err, closeErr)
	}
	if err := os.WriteFile(filepath.Join(dir, "bill.csv"), []byte(statementCSV), 0600); err != nil {
		t.Fatal(err)
	}
	stop := runStatementSync(l, dir, time.Hour)
	defer stop()
	// A canceled SELECT proves the importer holds the shared connection before
	// the terminal audit starts; a sleep alone would not establish contention.
	blocked := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		var ready int
		err := l.QueryRow(ctx, "SELECT 1").Scan(&ready)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			blocked = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if !blocked {
		t.Fatal("automatic import did not hold the shared ledger connection")
	}
	// AppendAudit's own five-second deadline must succeed while import is active.
	if err := l.AppendAudit(ledger.Audit{ID: "during-import", TimestampMS: 200, Protocol: "openai", Upstream: "test", Model: "m", Status: "ok"}); err != nil {
		t.Fatalf("terminal request audit failed during automatic import: %v", err)
	}
	waitForSync(t, l, func(s ledger.StatementSync) bool { return s.Failures > 0 })
	stop()
	var statements, requests int
	if err := l.QueryRow(context.Background(), "SELECT COUNT(*) FROM supplier_statement_lines").Scan(&statements); err != nil {
		t.Fatal(err)
	}
	if err := l.QueryRow(context.Background(), "SELECT COUNT(*) FROM request_audit WHERE id='during-import'").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if statements != 0 || requests != 1 {
		t.Fatalf("slow import left statements=%d or lost terminal audit: requests=%d", statements, requests)
	}
}

func TestReconciliationConfigValidation(t *testing.T) {
	for _, c := range []ReconciliationConfig{{}, {StatementDir: t.TempDir(), PollIntervalSeconds: 1}, {PollIntervalSeconds: 86400}} {
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []ReconciliationConfig{{StatementDir: "relative"}, {StatementDir: " "}, {PollIntervalSeconds: -1}, {PollIntervalSeconds: 86401}} {
		if c.Validate() == nil {
			t.Fatalf("accepted invalid config: %+v", c)
		}
	}
}

func waitForSync(t *testing.T, l *ledger.Ledger, ready func(ledger.StatementSync) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		s, err := l.StatementSyncStatus(context.Background())
		if err == nil && ready(s) {
			return
		}
		last = fmt.Sprintf("%+v error=%v", s, err)
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("automatic sync did not reach expected state: %s", last)
}
