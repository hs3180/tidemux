package ledger

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAppendRequestAndEvent(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	hit := int64(12)
	shaping := "queued"
	id, err := l.AppendRequest(context.Background(), Request{
		TimestampMS: 1757300000000, Model: "deepseek-chat", Status: "queued_ok",
		PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28,
		CacheHitTokens: &hit, PriceInPerMTok: 2, PriceOutPerMTok: 8,
		PriceHitPerMTok: 1, PriceMissPerMTok: 2, EstimatedCostCNY: 0.00004,
		Shaping: &shaping,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("request ID = %d, want 1", id)
	}
	if _, err := l.AppendEvent(context.Background(), Event{TimestampMS: 1757300000001, Type: "queue_wait", RequestID: &id, Reason: "并发上限，已排队"}); err != nil {
		t.Fatal(err)
	}

	var requests, events int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM ledger_requests`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM ledger_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || events != 1 {
		t.Fatalf("rows = requests:%d events:%d", requests, events)
	}
}

func TestAppendRequestRejectsInconsistentUsage(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	_, err = l.AppendRequest(context.Background(), Request{TimestampMS: 1, Model: "deepseek-chat", Status: "ok", PromptTokens: 1, CompletionTokens: 1, TotalTokens: 3})
	if err == nil {
		t.Fatal("AppendRequest accepted inconsistent token totals")
	}

	var count int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM ledger_requests`).Scan(&count); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unexpected rows: %d", count)
	}
}
