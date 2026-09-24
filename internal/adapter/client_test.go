package adapter

import (
	"context"
	"errors"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newCandidateTestClient(t *testing.T, upstream *http.Client) (*Client, *ledger.Ledger) {
	t.Helper()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := limiter.NewConcurrencyGate(1)
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	return &Client{Protocol: "openai", BaseURL: "http://upstream.invalid/v1", APIKey: "unused", Upstream: "test", HTTP: upstream, Ledger: l, Gate: g}, l
}

func TestKeyCandidatesFailOverWithOneAuditRecord(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer key-a" {
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"private rate limit detail"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chat1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL = upstream.URL + "/v1"
	var failedIndex int = -1
	var failure *CallError
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	response, id, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, func(index int, callErr *CallError) {
		failedIndex, failure = index, callErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response) == 0 || id == "" || len(calls) != 2 || failedIndex != 0 || failure == nil || !failure.Retryable || failure.UpstreamStatus != 429 || failure.Cooldown != 9*time.Second {
		t.Fatalf("response=%s id=%q calls=%v failedIndex=%d failure=%+v", response, id, calls, failedIndex, failure)
	}
	if calls[0] != "Bearer key-a" || calls[1] != "Bearer key-b" {
		t.Fatalf("credential order = %v", calls)
	}
	rows, err := l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audits=%d err=%v", len(rows), err)
	}
	if rows[0].ID != id || rows[0].Status != "ok" || !containsEvent(rows[0].Events, "rate_limit_429") || !containsEvent(rows[0].Events, "key_failover") {
		t.Fatalf("audit = %+v", rows[0])
	}
	if rows[0].InputTokens == nil || *rows[0].InputTokens != 2 || rows[0].OutputTokens == nil || *rows[0].OutputTokens != 1 {
		t.Fatalf("audit usage should come from the successful attempt only: %+v", rows[0])
	}
	if strings.Contains(strings.Join(rows[0].Events, ","), "key-a") || strings.Contains(strings.Join(rows[0].Events, ","), "key-b") {
		t.Fatalf("audit exposed key material: %+v", rows[0].Events)
	}
}

func TestAnthropicKeyCandidatesFailOverWithXAPIKey(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("x-api-key"))
		if r.Header.Get("x-api-key") == "key-a" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.Protocol = "anthropic"
	client.APIVersion = "2023-06-01"
	client.BaseURL = upstream.URL + "/v1"
	body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	_, _, err := client.CallFromKeyCandidates("anthropic", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, nil)
	if err != nil || len(calls) != 2 || calls[0] != "key-a" || calls[1] != "key-b" {
		t.Fatalf("err=%v key attempts=%v", err, calls)
	}
}

func TestPreWriteTransportFailureCanFailOver(t *testing.T) {
	attempts := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("dial failed")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chat1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))}, nil
	})
	client, l := newCandidateTestClient(t, &http.Client{Transport: transport})
	defer l.Close()
	var failure *CallError
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, func(_ int, callErr *CallError) { failure = callErr })
	if err != nil || attempts != 2 || failure == nil || !failure.Retryable || failure.Cooldown != 5*time.Second {
		t.Fatalf("err=%v attempts=%d failure=%+v", err, attempts, failure)
	}
}

func TestTransportFailureAfterHeadersDoesNotFailOver(t *testing.T) {
	attempts := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteHeaders != nil {
			trace.WroteHeaders()
		}
		return nil, errors.New("connection lost after request write")
	})
	client, l := newCandidateTestClient(t, &http.Client{Transport: transport})
	defer l.Close()
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, nil)
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Retryable || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestFailoverGuardRejectsRetryAfterOutputOrCancellation(t *testing.T) {
	callErr := &CallError{Retryable: true}
	if canFailover(callErr, true, false, context.Background()) != true {
		t.Fatal("eligible pre-output retry was rejected")
	}
	if canFailover(callErr, true, true, context.Background()) {
		t.Fatal("retry allowed after downstream output began")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if canFailover(callErr, true, false, ctx) {
		t.Fatal("retry allowed after request cancellation")
	}
}

func TestIncompleteStreamAfterOutputDoesNotTryAnotherKey(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL = upstream.URL + "/v1"
	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	frames := 0
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", func(string, []byte) error {
		frames++
		return nil
	}, CallOptions{}, []string{"key-a", "key-b"}, nil)
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Code != "incomplete_upstream_stream" || frames != 1 || calls != 1 {
		t.Fatalf("err=%v frames=%d upstream calls=%d", err, frames, calls)
	}
}

func containsEvent(events []string, target string) bool {
	for _, event := range events {
		if event == target {
			return true
		}
	}
	return false
}

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
