package adapter

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hs3180/tidemux/internal/ledger"
)

func TestAppendLedgerMapsUsageCostAndEvent(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	hit, miss := int64(10), int64(30)
	id, err := AppendLedger(context.Background(), l, Record{
		TimestampMS: 1, Model: "deepseek-chat", Status: "queued_ok",
		Usage:          Usage{PromptTokens: 40, CompletionTokens: 20, CacheHitTokens: &hit, CacheMissTokens: &miss},
		PriceInPerMTok: 2, PriceOutPerMTok: 8, PriceHitPerMTok: 1, PriceMissPerMTok: 2,
		Events: []ledger.Event{{TimestampMS: 2, Type: "queue_wait", Reason: "并发上限，已排队"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var total, cost, reqID int64
	var eventReason string
	if err := l.QueryRow(context.Background(), `SELECT total_tokens, CAST(est_cost_cny * 1000000 AS INTEGER) FROM ledger_requests WHERE id = ?`, id).Scan(&total, &cost); err != nil {
		t.Fatal(err)
	}
	if err := l.QueryRow(context.Background(), `SELECT req_id, reason FROM ledger_events WHERE id = 1`).Scan(&reqID, &eventReason); err != nil {
		t.Fatal(err)
	}
	if total != 60 || cost != 230 || reqID != id || eventReason != "并发上限，已排队" {
		t.Fatalf("unexpected ledger mapping: total=%d cost=%d reqID=%d reason=%q", total, cost, reqID, eventReason)
	}
}
