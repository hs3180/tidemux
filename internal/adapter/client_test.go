package adapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
	"github.com/hs3180/tidemux/internal/observability"
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

func TestRateLimitReturnsToClientAfterOneUpstreamAttempt(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("Authorization"))
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"private rate limit detail"}}`)
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL = upstream.URL + "/v1"
	var failedIndex int = -1
	var failure *CallError
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	_, id, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{
		Failed: func(index int, callErr *CallError) {
			failedIndex, failure = index, callErr
		},
	})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Status != http.StatusTooManyRequests || callErr.RetryAfterValue != "9" {
		t.Fatalf("call error=%+v", err)
	}
	if id == "" || len(calls) != 1 || failedIndex != 0 || failure == nil || failure.UpstreamStatus != 429 || failure.Cooldown != 9*time.Second {
		t.Fatalf("id=%q calls=%v failedIndex=%d failure=%+v", id, calls, failedIndex, failure)
	}
	if calls[0] != "Bearer key-a" {
		t.Fatalf("request used unexpected credential: %v", calls)
	}
	rows, err := l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audits=%d err=%v", len(rows), err)
	}
	if rows[0].ID != id || rows[0].Status != "error" || !containsEvent(rows[0].Events, "rate_limit") || containsEvent(rows[0].Events, "rate_limit_retry") || containsEvent(rows[0].Events, "key_failover") {
		t.Fatalf("audit = %+v", rows[0])
	}
	if strings.Contains(strings.Join(rows[0].Events, ","), "key-a") || strings.Contains(strings.Join(rows[0].Events, ","), "key-b") {
		t.Fatalf("audit exposed key material: %+v", rows[0].Events)
	}
}

func TestMappedRateLimitCategoryReturnsWithoutRetry(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("Authorization"))
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"provider_throttled"}}`)
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL = upstream.URL + "/v1"
	client.ErrorCodeMappings = []ProviderErrorMapping{{UpstreamCode: "provider_throttled", HTTPStatus: http.StatusForbidden, Category: ProviderErrorRateLimited}}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	failedCallbacks := 0
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{
		Failed: func(index int, callErr *CallError) {
			failedCallbacks++
			if index != 0 || callErr.Category != ProviderErrorRateLimited || callErr.UpstreamStatus != http.StatusForbidden {
				t.Fatalf("rate-limit callback index=%d error=%+v", index, callErr)
			}
		},
	})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Category != ProviderErrorRateLimited || len(calls) != 1 || calls[0] != "Bearer key-a" || failedCallbacks != 1 {
		t.Fatalf("err=%v calls=%v failed callbacks=%d", err, calls, failedCallbacks)
	}
}

func TestAnthropicUpstreamFailureReturnsWithoutKeyFailover(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("x-api-key"))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.Protocol = "anthropic"
	client.APIVersion = "2023-06-01"
	client.BaseURL = upstream.URL + "/v1"
	body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	_, _, err := client.CallFromKeyCandidates("anthropic", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{})
	if err == nil || len(calls) != 1 || calls[0] != "key-a" {
		t.Fatalf("err=%v key attempts=%v", err, calls)
	}
}

func TestStreamingRateLimitReturnsBeforeFirstFrameWithoutRetry(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("Authorization"))
		if r.Header.Get(SessionIDHeader) != "session-123" {
			t.Errorf("session ID not preserved: %q", r.Header.Get(SessionIDHeader))
		}
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL = upstream.URL + "/v1"
	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	var frameIDs []string
	var frames [][]byte
	response, id, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", func(frameID string, frame []byte) error {
		frameIDs = append(frameIDs, frameID)
		frames = append(frames, append([]byte(nil), frame...))
		return nil
	}, CallOptions{SessionID: "session-123"}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{})
	var callErr *CallError
	if !errors.As(err, &callErr) || len(calls) != 1 || calls[0] != "Bearer key-a" {
		t.Fatalf("err=%v calls=%v", err, calls)
	}
	if id == "" || len(frameIDs) != 0 || len(response) != 0 {
		t.Fatalf("requestID=%q frameIDs=%v response=%s frames=%q", id, frameIDs, response, frames)
	}
	rows, err := l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Status != "error" || !containsEvent(rows[0].Events, "rate_limit") {
		t.Fatalf("audits=%+v err=%v", rows, err)
	}
}

func TestPreWriteTransportFailureReturnsWithoutFailover(t *testing.T) {
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
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{
		Failed: func(_ int, callErr *CallError) {
			failure = callErr
		},
	})
	if err == nil || attempts != 1 || failure == nil || !failure.UpstreamNotAttempted || failure.Cooldown != 5*time.Second {
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
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{})
	var callErr *CallError
	if !errors.As(err, &callErr) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
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
	}, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Code != "incomplete_upstream_stream" || frames != 1 || calls != 1 {
		t.Fatalf("err=%v frames=%d upstream calls=%d", err, frames, calls)
	}
}

func TestStreamCancellationAfterFirstFrameDoesNotTryAnotherKey(t *testing.T) {
	attempts := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL = upstream.URL + "/v1"
	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := make(chan []byte, 1)
	type result struct {
		id  string
		err error
	}
	completed := make(chan result, 1)
	go func() {
		_, id, err := client.CallFromKeyCandidates("openai", ctx, body, "m", func(_ string, frame []byte) error {
			frames <- append([]byte(nil), frame...)
			return nil
		}, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{})
		completed <- result{id: id, err: err}
	}()

	select {
	case frame := <-frames:
		if !strings.Contains(string(frame), "partial") {
			t.Fatalf("first stream frame = %q", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the first stream frame")
	}
	select {
	case authorization := <-attempts:
		if authorization != "Bearer key-a" {
			t.Fatalf("first attempt used unexpected credential: %q", authorization)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream was not called")
	}
	cancel()

	var resultValue result
	select {
	case resultValue = <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for canceled stream to finish")
	}
	var callErr *CallError
	if !errors.As(resultValue.err, &callErr) || callErr.Code != "request_canceled" || resultValue.id == "" {
		t.Fatalf("request ID=%q error=%v", resultValue.id, resultValue.err)
	}
	select {
	case authorization := <-attempts:
		t.Fatalf("canceled stream unexpectedly tried another key: %q", authorization)
	default:
	}
	rows, err := l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].ID != resultValue.id || rows[0].Status != "canceled" || rows[0].ErrorCode != "request_canceled" {
		t.Fatalf("audits=%+v err=%v", rows, err)
	}
}

func TestRateLimitDoesNotRetryOrWaitAndPreservesRetryAfter(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL = upstream.URL + "/v1"
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Code != "upstream_rate_limited" || callErr.Category != ProviderErrorRateLimited || callErr.RetryAfterValue != "3600" || !callErr.RetryProvided {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 {
		t.Fatalf("attempts=%d", calls)
	}
}

func TestRateLimitWithoutRetryAfterDoesNotInventRetryHeaderValue(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL = upstream.URL + "/v1"
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Code != "upstream_rate_limited" || callErr.RetryProvided || callErr.RetryAfterValue != "" || callErr.Cooldown != time.Second {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 {
		t.Fatalf("attempts=%d", calls)
	}
}

func TestMappedRateLimitStreamErrorAfterOutputIsNotRetried(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"code\":\"throttled\",\"status\":429,\"message\":\"SECRET\"}}\n\n")
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL = upstream.URL + "/v1"
	client.ErrorCodeMappings = []ProviderErrorMapping{{UpstreamCode: "throttled", HTTPStatus: 429, Category: ProviderErrorRateLimited}}
	frames := 0
	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", func(_ string, _ []byte) error {
		frames++
		return nil
	}, CallOptions{}, []string{"key-a", "key-b"}, KeyCandidateCallbacks{})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Category != ProviderErrorRateLimited {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 || frames != 1 || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("attempts=%d frames=%d err=%v", calls, frames, err)
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

func TestAuditFailureAfterReadinessPreventsDispatch(t *testing.T) {
	posts := 0
	c, l := newCandidateTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		posts++
		return nil, errors.New("unexpected dispatch")
	})})
	defer l.Close()
	completed := false
	_, id, err := c.CallFromKeyCandidates("openai", context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"fixture"}]}`), "m", nil, CallOptions{}, []string{"key"}, KeyCandidateCallbacks{
		Ready: func(int) (bool, time.Duration) {
			// Simulate another request discovering a storage failure while this
			// admitted call was waiting for its dispatch slot/readiness check.
			l.Close()
			if l.AppendAudit(ledger.Audit{ID: "other", TimestampMS: 1, Protocol: "openai", Upstream: "test", Model: "m", Status: "ok"}) == nil {
				t.Fatal("expected a real storage failure")
			}
			return true, 0
		},
		Completed: func(int, error) { completed = true },
	})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Code != "audit_unavailable" || !callErr.UpstreamNotAttempted || id != "" || posts != 0 || completed {
		t.Fatalf("storage failure bypassed the final dispatch guard: id=%s err=%+v posts=%d completed=%t", id, err, posts, completed)
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
	var logs bytes.Buffer
	c := Client{Protocol: "openai", BaseURL: up.URL, APIKey: "test", Upstream: "test", Ledger: l, Gate: g, Logger: observability.JSONLogger(&logs)}
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
	c.APIKey = "private-key-marker"
	_, requestID, err := c.Call(context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"private-request-marker"}]}`), "m")
	if err == nil || err.Error() != "audit_failed_do_not_retry_blindly" {
		t.Fatalf("audit failure %v", err)
	}
	if !strings.Contains(logs.String(), `"event":"request_audit_write_failure"`) || !strings.Contains(logs.String(), `"requestId":"`+requestID+`"`) || !strings.Contains(logs.String(), `"failure_code":"audit_write_failed"`) {
		t.Fatalf("missing safe audit-write diagnostic: %s", logs.String())
	}
	if strings.Contains(logs.String(), "private-key-marker") || strings.Contains(logs.String(), "private-request-marker") || strings.Contains(logs.String(), "database is closed") {
		t.Fatalf("audit-write diagnostic exposed credential or request content: %s", logs.String())
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
	t.Cleanup(c.PromptCache.Close)
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
	t.Cleanup(c.PromptCache.Close)
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
