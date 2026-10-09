package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

func fixture(t *testing.T) (*ledger.Ledger, *Exporter, string, *Signer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	signer, err := OpenSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{Enabled: true}, path, signer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return l, e, path, signer
}

func audit(id, currency string) ledger.Audit {
	return ledger.Audit{ID: id, TimestampMS: 100, Protocol: "openai", Upstream: "mock", ProviderRef: "test", Model: "model", Status: "ok", Currency: currency, Events: []string{"complete"}}
}

func records(t *testing.T, directory string) []Record {
	t.Helper()
	logs, err := listLogs(directory)
	if err != nil {
		t.Fatal(err)
	}
	var result []Record
	for _, log := range logs {
		data, err := os.ReadFile(log.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var row Record
			if json.Unmarshal([]byte(line), &row) != nil {
				t.Fatalf("invalid complete row: %q", line)
			}
			result = append(result, row)
		}
	}
	return result
}

func syncAll(t *testing.T, e *Exporter) {
	t.Helper()
	var previous Status
	for i := 0; i < 100; i++ {
		s, err := e.Sync(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		if s.AuditRowID == previous.AuditRowID && s.StatementID == previous.StatementID {
			return
		}
		previous = s
	}
	t.Fatal("export did not converge")
}

func TestCommitBoundaryAtomicEventsAndSameTimestamp(t *testing.T) {
	l, e, path, _ := fixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER reject_usage_event BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'test'); END`); err != nil {
		t.Fatal(err)
	}
	if l.AppendAudit(audit("rejected", "")) == nil {
		t.Fatal("expected aborted audit+event")
	}
	syncAll(t, e)
	if len(records(t, e.config.Directory)) != 0 {
		t.Fatal("exported uncommitted audit")
	}
	if _, err = db.Exec(`DROP TRIGGER reject_usage_event`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"same-time-1", "same-time-2"} {
		if err := l.AppendAudit(audit(id, "")); err != nil {
			t.Fatal(err)
		}
	}
	syncAll(t, e)
	rows := records(t, e.config.Directory)
	if len(rows) != 2 || rows[0].Message.Usage != nil || rows[0].TideMux.InputTokens != nil || rows[0].TideMux.EstimatedCost != nil || rows[0].TideMux.SessionGroup != nil || rows[0].TideMux.Currency != nil {
		t.Fatalf("rows=%+v", rows)
	}
	var count int
	if err := l.QueryRow(context.Background(), `SELECT COUNT(*) FROM audit_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("events=%d %v", count, err)
	}
}

func TestCrashReplayHalfTailAndSupplierRevision(t *testing.T) {
	l, e, _, signer := fixture(t)
	a := audit("known", "EUR")
	n := int64(3)
	cost := 1.5
	a.InputTokens = &n
	a.EstimatedCost = &cost
	a.UsageSource = "provider"
	a.SessionGroup = signer.SessionGroup("openai", "caller-secret", "raw-session-secret")
	if err := l.AppendAudit(a); err != nil {
		t.Fatal(err)
	}
	e.afterAppend = func() error { return errors.New("simulated_crash") }
	if _, err := e.Sync(context.Background(), false); err == nil {
		t.Fatal("crash hook did not fire")
	}
	logPath := filepath.Join(logDirectory(e.config.Directory), a.SessionGroup, "usage-00000000000000000001.jsonl")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"half":`)
	f.Close()
	e.afterAppend = nil
	syncAll(t, e)
	rows := records(t, e.config.Directory)
	if len(rows) != 2 || rows[0].RequestID != rows[1].RequestID || rows[0].Message.ID != rows[1].Message.ID || rows[0].TideMux.Revision != 0 {
		t.Fatalf("replay=%+v", rows)
	}
	if _, err := l.ImportStatementCSV(context.Background(), strings.NewReader("period_start,period_end,currency,amount,request_id,model\n50,150,EUR,2,known,model\n50,150,USD,99,known,model\n50,150,EUR,10,,model\n"), 200); err != nil {
		t.Fatal(err)
	}
	syncAll(t, e)
	rows = records(t, e.config.Directory)
	last := rows[len(rows)-1].TideMux
	if last.Revision == 0 || last.SupplierAmount == nil || *last.SupplierAmount != 2 || last.SupplierStatementLines != 1 || *last.EstimatedCost != 1.5 || last.CostSource != "estimated_from_provider_usage" {
		t.Fatalf("revision=%+v", last)
	}
	if _, err := l.ImportStatementCSV(context.Background(), strings.NewReader("period_start,period_end,currency,amount,request_id,model\n50,150,EUR,0.5,known,model\n"), 300); err != nil {
		t.Fatal(err)
	}
	syncAll(t, e)
	rows = records(t, e.config.Directory)
	updated := rows[len(rows)-1].TideMux
	if updated.Revision <= last.Revision || *updated.SupplierAmount != 2.5 || updated.SupplierStatementLines != 2 {
		t.Fatalf("update=%+v", updated)
	}
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "caller-secret") || strings.Contains(string(data), "raw-session-secret") {
		t.Fatal("raw identity leaked")
	}
}

func TestOutputFailureCannotRecoverPendingBudgetOrRollbackAudit(t *testing.T) {
	l, e, _, _ := fixture(t)
	ctx := context.Background()
	now := time.Now()
	p := ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 10, WeeklyLimit: 10, Mode: "hard", AlertThreshold: .8}
	if _, err := l.CheckBudget(ctx, "pending", "provider", p, false, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config.Directory, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(ctx, false); err == nil {
		t.Fatal("expected output error")
	}
	if err := l.AppendAudit(audit("still-commits", "")); err != nil {
		t.Fatal(err)
	}
	var pending, events int
	if err := l.QueryRow(ctx, `SELECT COUNT(*) FROM budget_charges WHERE request_id='pending' AND state='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := l.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE request_id='still-commits'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || events != 1 {
		t.Fatalf("pending=%d events=%d", pending, events)
	}
}

func TestIdentityPersistentNamespacesPrivateOutputAndRotation(t *testing.T) {
	l, e, path, s := fixture(t)
	s2, err := OpenSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	group := s.SessionGroup("anthropic", "caller", "session")
	if group != s2.SessionGroup("anthropic", "caller", "session") || group == s.SessionGroup("openai", "caller", "session") || group == s.SessionGroup("anthropic", "other", "session") || s.SessionGroup("anthropic", "caller", "") != "" {
		t.Fatal("identity namespace/stability")
	}
	e.config.MaxBytes = 4096
	e.config.MaxFiles = 2
	for i := 0; i < 35; i++ {
		a := audit(strings.Repeat("x", i+1), "USD")
		a.SessionGroup = group
		if err := l.AppendAudit(a); err != nil {
			t.Fatal(err)
		}
	}
	syncAll(t, e)
	logs, err := listLogs(e.config.Directory)
	if err != nil || len(logs) != 2 {
		t.Fatalf("retention files=%d error=%v", len(logs), err)
	}
	if err := filepath.Walk(e.config.Directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("output not private: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+".usage-key", 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSigner(path); err == nil {
		t.Fatal("public key accepted")
	}
}

func TestDedicatedDirectorySymlinkAndVacuumAnchor(t *testing.T) {
	l, e, path, _ := fixture(t)
	if err := l.AppendAudit(audit("removed", "")); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendAudit(audit("anchor", "")); err != nil {
		t.Fatal(err)
	}
	syncAll(t, e)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM audit_events WHERE request_id='removed'; DELETE FROM request_audit WHERE id='removed'; VACUUM`); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendAudit(audit("after-vacuum", "")); err != nil {
		t.Fatal(err)
	}
	syncAll(t, e)
	rows := records(t, e.config.Directory)
	found := false
	for _, r := range rows {
		if r.TideMux.RequestID == "after-vacuum" {
			found = true
		}
	}
	if !found {
		t.Fatal("rowid reset skipped new committed request")
	}
	if err := os.WriteFile(filepath.Join(e.config.Directory, "client-owned.jsonl"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(context.Background(), false); err == nil || err.Error() != "usage_directory_not_dedicated" {
		t.Fatalf("err=%v", err)
	}
	os.Remove(filepath.Join(e.config.Directory, "client-owned.jsonl"))
	os.Remove(filepath.Join(e.config.Directory, "checkpoint.json"))
	if err := os.Symlink(path, filepath.Join(e.config.Directory, "checkpoint.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(context.Background(), false); err == nil {
		t.Fatal("symlink output accepted")
	}
}

func TestSessionDirectoriesRotationAndGlobalRetention(t *testing.T) {
	l, e, _, signer := fixture(t)
	groups := []string{signer.SessionGroup("openai", "caller", "a"), signer.SessionGroup("openai", "caller", "b")}
	e.config.MaxBytes = 4096
	e.config.MaxFiles = 64
	for i := 0; i < 20; i++ {
		a := audit(fmt.Sprintf("request-%d", i), "USD")
		a.SessionGroup = groups[i%2]
		if err := l.AppendAudit(a); err != nil {
			t.Fatal(err)
		}
	}
	syncAll(t, e)
	logs, err := listLogs(e.config.Directory)
	if err != nil || len(logs) <= 2 || len(logs) >= 20 {
		t.Fatalf("rotation must reuse session files: logs=%d error=%v", len(logs), err)
	}
	for _, log := range logs {
		data, err := os.ReadFile(log.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var row Record
			if err := json.Unmarshal([]byte(line), &row); err != nil || row.SessionID != log.group {
				t.Fatalf("session split by file rotation: %s %v", log.path, err)
			}
		}
	}
	e.config.MaxFiles = 2
	syncAll(t, e)
	logs, err = listLogs(e.config.Directory)
	if err != nil || len(logs) != 2 {
		t.Fatalf("global retention: logs=%d error=%v", len(logs), err)
	}
	for i := 0; i < 8; i++ {
		a := audit(fmt.Sprintf("other-%d", i), "")
		a.SessionGroup = signer.SessionGroup("openai", "caller", fmt.Sprintf("session-%d", i))
		if err := l.AppendAudit(a); err != nil {
			t.Fatal(err)
		}
	}
	syncAll(t, e)
	entries, err := os.ReadDir(logDirectory(e.config.Directory))
	if err != nil || len(entries) != 2 {
		t.Fatalf("empty session directories retained: groups=%d error=%v", len(entries), err)
	}
}

func TestNestedOutputRejectsClientDirectoriesAndSymlinks(t *testing.T) {
	for _, scenario := range []string{"client-project", "session-symlink", "log-symlink", "public-log"} {
		t.Run(scenario, func(t *testing.T) {
			l, e, path, signer := fixture(t)
			a := audit("request", "")
			a.SessionGroup = signer.SessionGroup("openai", "caller", "session")
			if err := l.AppendAudit(a); err != nil {
				t.Fatal(err)
			}
			syncAll(t, e)
			logs, err := listLogs(e.config.Directory)
			if err != nil || len(logs) != 1 {
				t.Fatal(err)
			}
			switch scenario {
			case "client-project":
				err = os.Mkdir(filepath.Join(e.config.Directory, "projects", "client"), 0700)
			case "session-symlink":
				err = os.Symlink(path, filepath.Join(logDirectory(e.config.Directory), strings.Repeat("a", 64)))
			case "log-symlink":
				err = os.Remove(logs[0].path)
				if err == nil {
					err = os.Symlink(path, logs[0].path)
				}
			case "public-log":
				err = os.Chmod(logs[0].path, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Sync(context.Background(), false); err == nil {
				t.Fatal("accepted unsafe nested output")
			}
		})
	}
}
