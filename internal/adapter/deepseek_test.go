package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

func TestDeepSeekChatCallsProviderAndRecordsUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected provider request: path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "deepseek-chat" {
			t.Fatalf("unexpected request: %+v, err=%v", request, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"prompt_cache_hit_tokens":8,"prompt_cache_miss_tokens":4}}`))
	}))
	defer server.Close()

	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	client, err := NewDeepSeekClient(DeepSeekConfig{APIKey: "test-key", BaseURL: server.URL, Ledger: l, PriceInPerMTok: 2, PriceOutPerMTok: 8, PriceHitPerMTok: 1, PriceMissPerMTok: 2})
	if err != nil {
		t.Fatal(err)
	}
	choice, err := client.Chat(context.Background(), ChatRequest{Model: "deepseek-chat", Messages: []ChatMessage{{Role: "user", Content: "hello"}}})
	if err != nil || choice.Message.Content != "ok" {
		t.Fatalf("unexpected chat result: %+v, err=%v", choice, err)
	}
	var status string
	var prompt, completion, hit, miss int64
	if err := l.QueryRow(context.Background(), `SELECT status, prompt_tokens, completion_tokens, cache_hit_tokens, cache_miss_tokens FROM ledger_requests`).Scan(&status, &prompt, &completion, &hit, &miss); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || prompt != 12 || completion != 4 || hit != 8 || miss != 4 {
		t.Fatalf("unexpected ledger row: status=%q prompt=%d completion=%d hit=%d miss=%d", status, prompt, completion, hit, miss)
	}
}

func TestDeepSeekChatRecordsProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusTooManyRequests) }))
	defer server.Close()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	client, err := NewDeepSeekClient(DeepSeekConfig{APIKey: "test-key", BaseURL: server.URL, Ledger: l})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "deepseek-chat"}); err == nil {
		t.Fatal("expected provider error")
	}
	var status, code string
	if err := l.QueryRow(context.Background(), `SELECT status, err_code FROM ledger_requests`).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "error" || code != "429" {
		t.Fatalf("unexpected error ledger row: status=%q code=%q", status, code)
	}
}

func TestDeepSeekChatRecordsConcurrencyQueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	gate, err := limiter.NewConcurrencyGate(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	client, err := NewDeepSeekClient(DeepSeekConfig{APIKey: "test-key", BaseURL: server.URL, Ledger: l, Limiter: gate})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, callErr := client.Chat(context.Background(), ChatRequest{Model: "deepseek-chat"})
		result <- callErr
	}()
	time.Sleep(15 * time.Millisecond)
	gate.Release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	var status, eventType, reason string
	if err := l.QueryRow(context.Background(), `SELECT r.status, e.type, e.reason FROM ledger_requests r JOIN ledger_events e ON r.id = e.req_id`).Scan(&status, &eventType, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "queued_ok" || eventType != "queue_wait" || reason != "并发上限，已排队" {
		t.Fatalf("unexpected shaping record: %q %q %q", status, eventType, reason)
	}
}
