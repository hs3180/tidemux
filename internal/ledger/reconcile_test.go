package ledger

import (
	"context"
	"strings"
	"testing"
)

func TestReconcileKeepsEstimatesStatementsAndUnknownsDistinct(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	in, out := int64(10), int64(3)
	cost := 2.5
	if err := l.AppendAudit(Audit{ID: "known", TimestampMS: 100, Protocol: "openai", Upstream: "deepseek", Model: "m", Status: "ok", InputTokens: &in, OutputTokens: &out, EstimatedCost: &cost, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendAudit(Audit{ID: "unknown", TimestampMS: 101, Protocol: "openai", Upstream: "deepseek", Model: "m", Status: "ok", Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendTokenComparison(context.Background(), TokenComparison{RequestID: "known", Tokenizer: "deepseek", TokenizerVersion: "v1", EstimatedInput: 9, EstimatedOutput: 3, MeasuredAtMS: 102}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ImportStatementCSV(context.Background(), strings.NewReader("period_start,period_end,currency,amount,request_id,model\n1970-01-01T00:00:00Z,1970-01-01T00:01:00Z,USD,3.0,known,m\n1970-01-01T00:00:00Z,1970-01-01T00:01:00Z,USD,1.0,,m\n"), 103); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendBalanceSnapshot(context.Background(), BalanceSnapshot{ObservedAtMS: 90, Currency: "USD", Total: 10}); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendBalanceSnapshot(context.Background(), BalanceSnapshot{ObservedAtMS: 110, Currency: "USD", Total: 6}); err != nil {
		t.Fatal(err)
	}
	r, err := l.Reconcile(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 1 {
		t.Fatalf("rows=%+v", r)
	}
	x := r[0]
	if x.LocalEstimated != 2.5 || x.SupplierStatement != 4 || x.Difference != nil || x.Coverage != "partial" || x.MatchedLines != 1 || x.UnmatchedLines != 1 || x.UnknownCostRequests != 1 || x.TokenizerComparisons != 1 || x.TokenizerMismatches != 1 {
		t.Fatalf("report=%+v", x)
	}
	if x.BalanceNetChange == nil || *x.BalanceNetChange != -4 || x.UnattributedBalanceDelta != nil {
		t.Fatalf("balance=%+v", x)
	}
}

func TestStatementImportRejectsAmbiguousOrInvalidRows(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, data := range []string{"period_start,period_end,currency,amount\n1,2,USD,-1\n", "period_start,period_end,currency\n1,2,USD\n", "period_start,period_end,currency,amount\n2,1,USD,1\n"} {
		if _, err := l.ImportStatementCSV(context.Background(), strings.NewReader(data), 1); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}
