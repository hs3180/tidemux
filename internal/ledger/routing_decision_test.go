package ledger

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hs3180/tidemux/internal/diagnostics"
)

func TestLocalRoutingDecisionCommitsWithRejectionAndPreservesLegacyShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	remaining := int64(30000)
	d := Diagnostic{ID: "0123456789abcdef0123456789abcdef", TimestampMS: 1, Protocol: "openai", Method: "POST", Endpoint: "chat_completions", Status: 429, ErrorCode: "provider_keys_cooling_down", RoutingDecision: &diagnostics.RoutingDecision{FromProvider: "a", ToProvider: "a", Model: "one", Action: "retain", Reason: "within_threshold", ThresholdMS: 60000, RemainingMS: &remaining}}
	if _, err = l.db.Exec(`CREATE TRIGGER fail_decision BEFORE INSERT ON local_routing_decisions BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if l.AppendDiagnostic(d) == nil {
		t.Fatal("injected decision failure succeeded")
	}
	rows, err := l.RecentDiagnostics(context.Background(), 10)
	if err != nil || len(rows) != 0 {
		t.Fatal("partial diagnostic committed")
	}
	l.db.Exec(`DROP TRIGGER fail_decision`)
	if err = l.AppendDiagnostic(d); err != nil {
		t.Fatal(err)
	}
	rows, err = l.RecentDiagnostics(context.Background(), 10)
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].RoutingDecision, d.RoutingDecision) {
		t.Fatalf("decision did not round-trip: %+v %v", rows, err)
	}
	// An older binary's seven-value INSERT remains valid on the new database.
	if _, err = l.db.Exec(`INSERT INTO local_diagnostics VALUES ('abcdef0123456789abcdef0123456789',2,'openai','POST','chat_completions',429,'rate_limited')`); err != nil {
		t.Fatal("historical diagnostic writer became incompatible")
	}
	// The read-only API also supports a historical database with no optional
	// details table and must not create that table while reading it.
	if _, err = l.db.Exec(`DROP TABLE local_routing_decisions`); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenAuditReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rows, err = ro.RecentDiagnostics(context.Background(), 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("old diagnostics unreadable: %+v %v", rows, err)
	}
}
