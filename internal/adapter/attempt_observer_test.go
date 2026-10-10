package adapter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type diagnosticTimeoutError struct{}

func (diagnosticTimeoutError) Error() string   { return "private network detail" }
func (diagnosticTimeoutError) Timeout() bool   { return true }
func (diagnosticTimeoutError) Temporary() bool { return true }

type diagnosticTimeoutBody struct{}

func (diagnosticTimeoutBody) Read([]byte) (int, error) { return 0, diagnosticTimeoutError{} }
func (diagnosticTimeoutBody) Close() error             { return nil }

func TestHTTPAttemptObserverCapturesBodyTimeoutAndExcludesPreDo(t *testing.T) {
	for _, stream := range []bool{false, true} {
		client := &Client{Protocol: "openai", BaseURL: "http://upstream.invalid/v1", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: diagnosticTimeoutBody{}}, nil
		})}}
		var results []HTTPAttemptResult
		observer := func() func(HTTPAttemptResult) {
			return func(result HTTPAttemptResult) { results = append(results, result) }
		}
		var sink StreamSink
		if stream {
			sink = func(string, []byte) error { return nil }
		}
		_, _, err := client.doAttempt(context.Background(), "openai", []byte(`{}`), "m", sink, CallOptions{}, Limits{}.Effective(), "id", "secret", &bytes.Buffer{}, new(bool), observer)
		if err == nil || len(results) != 1 || results[0] != (HTTPAttemptResult{Outcome: "timeout", FailureClass: "timeout"}) {
			t.Fatalf("stream=%v result=%v err=%v", stream, results, err)
		}
		// A malformed URL fails before Do and must not invoke the observer.
		results = nil
		client.BaseURL = "://invalid"
		client.doAttempt(context.Background(), "openai", nil, "m", nil, CallOptions{}, Limits{}.Effective(), "id", "secret", &bytes.Buffer{}, new(bool), observer)
		if len(results) != 0 {
			t.Fatal("pre-Do failure was counted")
		}
		// Already canceled work never reaches the HTTP transport or metrics.
		client.BaseURL = "http://upstream.invalid/v1"
		client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Error("canceled request reached transport")
			return nil, r.Context().Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client.doAttempt(ctx, "openai", nil, "m", nil, CallOptions{}, Limits{}.Effective(), "id", "secret", &bytes.Buffer{}, new(bool), observer)
		if len(results) != 0 {
			t.Fatal("pre-canceled request was counted")
		}
	}
}

func TestHTTPAttemptObserverExcludesSkippedCandidatesAndPreservesAudit(t *testing.T) {
	posts := 0
	client, l := newCandidateTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		posts++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"r","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))}, nil
	})})
	defer l.Close()
	var starts []int
	var results []HTTPAttemptResult
	_, _, err := client.CallFromKeyCandidates("openai", context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`), "m", nil, CallOptions{}, []string{"cooled-secret", "eligible-secret"}, KeyCandidateCallbacks{
		Ready: func(index int) (bool, time.Duration) { return index == 1, 0 },
		Attempt: func(index int) func(HTTPAttemptResult) {
			starts = append(starts, index)
			return func(result HTTPAttemptResult) { results = append(results, result) }
		},
	})
	if err != nil || posts != 1 || len(starts) != 1 || starts[0] != 1 || len(results) != 1 || results[0].Outcome != "success" {
		t.Fatalf("attempts=%v results=%v posts=%d err=%v", starts, results, posts, err)
	}
	rows, err := l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].Status != "ok" || *rows[0].InputTokens != 2 || *rows[0].OutputTokens != 1 {
		t.Fatal("observer changed authoritative audit or usage")
	}
}
