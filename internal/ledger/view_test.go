package ledger

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSummarizeDerivesWindowMetrics(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	if _, err := l.AppendRequest(context.Background(), Request{
		TimestampMS: 100, Model: "deepseek-chat", Status: "ok",
		PromptTokens: 40, CompletionTokens: 20, TotalTokens: 60,
		CacheHitTokens: int64Ptr(10), CacheMissTokens: int64Ptr(30),
		PriceInPerMTok: 2, PriceOutPerMTok: 8, PriceHitPerMTok: 1,
		PriceMissPerMTok: 2, EstimatedCostCNY: 0.00023,
		FirstTokenLatencyMS: int64Ptr(100), TotalLatencyMS: int64Ptr(500),
	}); err != nil {
		t.Fatal(err)
	}
	errorID, err := l.AppendRequest(context.Background(), Request{
		TimestampMS: 150, Model: "deepseek-chat", Status: "error",
		PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10,
		PriceInPerMTok: 2, PriceOutPerMTok: 8, PriceHitPerMTok: 1,
		PriceMissPerMTok: 2, EstimatedCostCNY: 0.00005,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.AppendEvent(context.Background(), Event{TimestampMS: 110, Type: "queue_wait", RequestID: &errorID, Reason: "并发上限"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AppendEvent(context.Background(), Event{TimestampMS: 120, Type: "rate_limit_429", RequestID: &errorID, Reason: "上游限流"}); err != nil {
		t.Fatal(err)
	}

	s, err := l.Summarize(context.Background(), 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if s.RequestCount != 2 || s.SuccessfulRequestCount != 1 || s.ErrorCount != 1 {
		t.Fatalf("unexpected request counts: %+v", s)
	}
	if s.TotalTokens != 70 || s.SpentCNY != 0.00023 {
		t.Fatalf("unexpected usage totals: %+v", s)
	}
	if s.CacheHitTokens != 10 || s.CacheMissTokens != 30 || s.CacheHitRatio != 0.25 {
		t.Fatalf("unexpected cache summary: %+v", s)
	}
	if s.AvgFirstTokenMS != 100 || s.AvgTotalLatencyMS != 500 || s.QueueWaitCount != 1 || s.RateLimitCount != 1 {
		t.Fatalf("unexpected latency/events: %+v", s)
	}

	filtered, err := l.Summarize(context.Background(), 200, 300)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.RequestCount != 0 || filtered.SpentCNY != 0 {
		t.Fatalf("unexpected empty window summary: %+v", filtered)
	}
}

func int64Ptr(v int64) *int64 { return &v }
