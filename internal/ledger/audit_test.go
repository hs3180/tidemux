package ledger

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAuditAtomicityAndUnknownValues(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a := Audit{ID: "r1", TimestampMS: 1, Protocol: "openai", Upstream: "test", Model: "m", Status: "error", Events: []string{"queue_wait"}}
	if _, err = l.db.Exec(`CREATE TRIGGER reject_event BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'test'); END`); err != nil {
		t.Fatal(err)
	}
	if l.AppendAudit(a) == nil {
		t.Fatal("expected event failure")
	}
	rows, _ := l.Recent(context.Background(), 10)
	if len(rows) != 0 {
		t.Fatal("partial request committed")
	}
	l.db.Exec(`DROP TRIGGER reject_event`)
	if err = l.AppendAudit(a); err != nil {
		t.Fatal(err)
	}
	if l.AppendAudit(a) == nil {
		t.Fatal("duplicate ID accepted")
	}
	rows, err = l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].InputTokens != nil || rows[0].EstimatedCost != nil {
		t.Fatalf("unknown values lost %+v %v", rows, err)
	}
}
