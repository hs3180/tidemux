package adapter

import (
	"context"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestQueuedCancellationIsAuditedWithoutSending(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	g, _ := limiter.NewConcurrencyGate(1)
	g.Acquire(context.Background())
	defer g.Release()
	c := Client{Protocol: "openai", Upstream: "test", Ledger: l, Gate: g}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err = c.Call(ctx, []byte(`{}`), "m"); err == nil {
		t.Fatal("expected cancellation")
	}
	rows, err := l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].Status != "canceled" || len(rows[0].Events) != 1 || rows[0].InputTokens != nil {
		t.Fatalf("rows %+v err %v", rows, err)
	}
}
func TestSuccessfulQueueAndAuditFailure(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	g, _ := limiter.NewConcurrencyGate(1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()
	c := Client{Protocol: "openai", BaseURL: up.URL, APIKey: "test", Upstream: "test", Ledger: l, Gate: g}
	g.Acquire(context.Background())
	done := make(chan error, 1)
	go func() { _, _, e := c.Call(context.Background(), []byte(`{}`), "m"); done <- e }()
	time.Sleep(20 * time.Millisecond)
	g.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows, _ := l.Recent(context.Background(), 10)
	if len(rows) != 1 || rows[0].Status != "ok" || len(rows[0].Events) != 1 {
		t.Fatalf("queue %+v", rows)
	}
	l.Close()
	_, _, err = c.Call(context.Background(), []byte(`{}`), "m")
	if err == nil || err.Error() != "audit_failed_do_not_retry_blindly" {
		t.Fatalf("audit failure %v", err)
	}
}

func TestMissingUsageUsesLocalContentEstimateAndSessionCache(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()
	in, out, hit := 1.0, 2.0, .1
	c := Client{
		Protocol:    "openai",
		BaseURL:     up.URL,
		Upstream:    "test",
		Prices:      map[string]Price{"m": {Currency: "USD", Source: "test", Version: "1", InputCacheMiss: &in, InputCacheHit: &hit, Output: &out}},
		PromptCache: NewPromptCache(),
		Ledger:      l,
	}
	g, _ := limiter.NewConcurrencyGate(1)
	c.Gate = g
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	for i := 0; i < 2; i++ {
		if _, _, err = c.CallWithOptions(context.Background(), body, "m", nil, CallOptions{SessionID: "session"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	var first, second *ledger.Audit
	for i := range rows {
		row := &rows[i]
		if row.CacheReadTokens != nil && *row.CacheReadTokens == 0 {
			first = row
		} else {
			second = row
		}
	}
	if first == nil || first.EstimatedCost == nil || first.CostSource != "local_estimated_cache_prefix" {
		t.Fatalf("first estimate=%+v", first)
	}
	if second == nil || second.EstimatedCost == nil || second.CostSource != "local_estimated_cache_prefix" || second.CacheReadTokens == nil || *second.CacheReadTokens == 0 {
		t.Fatalf("second estimate=%+v", second)
	}
}

func TestInterruptedStreamEstimatesReceivedOutput(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n")
	}))
	defer up.Close()
	in, out := 1.0, 2.0
	g, _ := limiter.NewConcurrencyGate(1)
	c := Client{
		Protocol:    "openai",
		BaseURL:     up.URL,
		Upstream:    "test",
		Prices:      map[string]Price{"m": {Currency: "USD", Source: "test", Version: "1", InputCacheMiss: &in, InputCacheHit: &in, Output: &out}},
		PromptCache: NewPromptCache(),
		Ledger:      l,
		Gate:        g,
	}
	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	_, _, err = c.CallWithOptions(context.Background(), body, "m", func(string, []byte) error { return nil }, CallOptions{SessionID: "session"})
	if err == nil || err.Error() != "incomplete_upstream_stream" {
		t.Fatalf("err=%v", err)
	}
	rows, err := l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].EstimatedCost == nil || rows[0].CostSource != "local_estimated_cache_prefix" || rows[0].InputTokens == nil || rows[0].OutputTokens == nil || *rows[0].OutputTokens == 0 {
		t.Fatalf("partial stream audit=%+v", rows[0])
	}
}
