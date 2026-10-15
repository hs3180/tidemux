package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUndispatchedRouteYieldsWithoutDuplicateAuditAndKeepsTiming(t *testing.T) {
	var attempts atomic.Int32
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))}, nil
	})
	client, l := newCandidateTestClient(t, &http.Client{Transport: transport})
	defer l.Close()
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"fixture"}]}`)
	started := time.Now().Add(-20 * time.Millisecond)
	_, id, err := client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil,
		CallOptions{RequestStartedAt: started, PriorQueueMS: 17}, []string{"cooled-key"}, KeyCandidateCallbacks{
			YieldUnavailable: true,
			Ready:            func(int) (bool, time.Duration) { return false, time.Minute },
		})
	var yielded *CallError
	if !errors.As(err, &yielded) || !yielded.UpstreamNotAttempted || id != "" || attempts.Load() != 0 || yielded.QueueMS != 17 || !yielded.RequestStartedAt.Equal(started) {
		t.Fatalf("undispatched route could not yield safely: id=%q error=%+v attempts=%d", id, err, attempts.Load())
	}
	if rows, err := l.Recent(context.Background(), 10); err != nil || len(rows) != 0 {
		t.Fatal("undispatched internal re-selection created an extra audit")
	}
	_, id, err = client.CallFromKeyCandidates("openai", context.Background(), body, "m", nil,
		CallOptions{RequestStartedAt: yielded.RequestStartedAt, PriorQueueMS: yielded.QueueMS}, []string{"ready-key"}, KeyCandidateCallbacks{})
	rows, readErr := l.Recent(context.Background(), 10)
	if err != nil || readErr != nil || len(rows) != 1 || rows[0].ID != id || rows[0].QueueMS != 17 || rows[0].LatencyMS < 20 || rows[0].TimestampMS != started.UnixMilli() || attempts.Load() != 1 {
		t.Fatalf("real attempt lost original timing or single-audit accounting: rows=%+v call=%v read=%v", rows, err, readErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = client.CallFromKeyCandidates("openai", ctx, body, "m", nil, CallOptions{}, []string{"ready-key"}, KeyCandidateCallbacks{YieldUnavailable: true})
	rows, readErr = l.Recent(context.Background(), 10)
	if err == nil || readErr != nil || len(rows) != 2 || attempts.Load() != 1 {
		t.Fatal("queue cancellation was suppressed as an internal route change")
	}
}
