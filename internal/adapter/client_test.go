package adapter

import (
	"context"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
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
