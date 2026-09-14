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
	r, e := l.GenerateDailyReport(context.Background(), at, "UTC", BudgetPolicy{Currency: "USD", Timezone: "UTC", DailyLimit: 1, MonthlyLimit: 2, AlertThreshold: .8, Mode: "hard", ReserveAmount: .1})
	if e != nil || r.RequestCount != 1 || r.EstimatedCost == nil || *r.EstimatedCost != .5 || r.ID < 1 {
		t.Fatalf("r=%+v e=%v", r, e)
	}
	rows, e := l.ListDailyReports(context.Background(), 1)
	if e != nil || len(rows) != 1 || rows[0].InputTokens != 3 {
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
