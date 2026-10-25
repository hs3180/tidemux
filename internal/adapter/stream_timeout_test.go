package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

type timeoutStreamFixture struct {
	Request                 json.RawMessage
	Start, Progress, Finish string
}

func timeoutStreamFixtures(t *testing.T) map[string]timeoutStreamFixture {
	t.Helper()
	data, err := os.ReadFile("../../scripts/fixtures/stream-timeouts.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]timeoutStreamFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

type timedStreamStep struct {
	delay time.Duration
	data  string
}

type timedStreamBody struct {
	ctx     context.Context
	steps   []timedStreamStep
	pending *strings.Reader
	closed  bool
}

func (b *timedStreamBody) Read(p []byte) (int, error) {
	for b.pending == nil || b.pending.Len() == 0 {
		if len(b.steps) == 0 {
			return 0, io.EOF
		}
		step := b.steps[0]
		b.steps = b.steps[1:]
		if step.delay > 0 {
			select {
			case <-b.ctx.Done():
				return 0, b.ctx.Err()
			case <-time.After(step.delay):
			}
		}
		b.pending = strings.NewReader(step.data)
		if step.data == "" {
			return 0, nil
		}
	}
	return b.pending.Read(p)
}

func TestStreamTimeoutBoundaries(t *testing.T) {
	fixtures := timeoutStreamFixtures(t)
	for _, protocol := range []string{"openai", "anthropic"} {
		fixture := fixtures[protocol]
		for _, test := range []struct {
			name           string
			headerDelay    time.Duration
			steps          []timedStreamStep
			totalSeconds   int
			cancelAfter    time.Duration
			parentDeadline time.Duration
			sinkDelay      time.Duration
			status         int
			wantElapsed    time.Duration
			wantCode       string
			wantPhase      string
			wantOutcome    string
		}{
			{name: "waiting-for-headers", headerDelay: 601 * time.Second, wantElapsed: 600 * time.Second, wantCode: "upstream_timeout", wantPhase: "response_headers", wantOutcome: "timeout"},
			{name: "waiting-for-first-body-byte", steps: []timedStreamStep{{delay: 601 * time.Second, data: fixture.Start}}, wantElapsed: 600 * time.Second, wantCode: "upstream_timeout", wantPhase: "stream", wantOutcome: "timeout"},
			{name: "stalls-after-progress", steps: []timedStreamStep{{data: fixture.Start}, {delay: 500 * time.Second, data: fixture.Progress}, {delay: 601 * time.Second, data: fixture.Finish}}, wantElapsed: 1100 * time.Second, wantCode: "upstream_timeout", wantPhase: "stream", wantOutcome: "timeout"},
			{name: "error-body-stalls", status: 503, steps: []timedStreamStep{{delay: 601 * time.Second, data: "unused"}}, wantElapsed: 600 * time.Second, wantCode: "upstream_timeout", wantPhase: "response_body", wantOutcome: "timeout"},
			{name: "headers-and-body-have-separate-waits", headerDelay: 500 * time.Second, steps: []timedStreamStep{{delay: 500 * time.Second, data: fixture.Start}, {delay: 500 * time.Second, data: fixture.Finish}}, wantElapsed: 1500 * time.Second, wantOutcome: "success"},
			{name: "heartbeats-refresh-idle-budget", steps: []timedStreamStep{{data: fixture.Start}, {delay: 500 * time.Second, data: ": heartbeat\n\n"}, {delay: 500 * time.Second, data: ": heartbeat\n\n"}, {delay: 500 * time.Second, data: fixture.Finish}}, wantElapsed: 1500 * time.Second, wantOutcome: "success"},
			{name: "downstream-processing-is-not-upstream-idle", sinkDelay: 700 * time.Second, steps: []timedStreamStep{{data: fixture.Start}, {delay: 500 * time.Second, data: fixture.Finish}}, wantElapsed: 1200 * time.Second, wantOutcome: "success"},
			{name: "empty-reads-do-not-refresh", steps: []timedStreamStep{{data: fixture.Start}, {delay: 250 * time.Second}, {delay: 250 * time.Second}, {delay: 250 * time.Second, data: fixture.Finish}}, wantElapsed: 600 * time.Second, wantCode: "upstream_timeout", wantPhase: "stream", wantOutcome: "timeout"},
			{name: "explicit-total-still-bounds-active-stream", totalSeconds: 600, steps: []timedStreamStep{{data: fixture.Start}, {delay: 500 * time.Second, data: fixture.Progress}, {delay: 500 * time.Second, data: fixture.Finish}}, wantElapsed: 600 * time.Second, wantCode: "upstream_timeout", wantPhase: "stream", wantOutcome: "timeout"},
			{name: "explicit-total-replaces-default-idle-policy", totalSeconds: 1800, steps: []timedStreamStep{{data: fixture.Start}, {delay: 1200 * time.Second, data: fixture.Finish}}, wantElapsed: 1200 * time.Second, wantOutcome: "success"},
			{name: "parent-deadline", parentDeadline: 700 * time.Second, steps: []timedStreamStep{{data: fixture.Start}, {delay: 500 * time.Second, data: fixture.Progress}, {delay: 500 * time.Second, data: fixture.Finish}}, wantElapsed: 700 * time.Second, wantCode: "request_canceled", wantPhase: "stream", wantOutcome: "timeout"},
			{name: "parent-cancellation", cancelAfter: 700 * time.Second, steps: []timedStreamStep{{data: fixture.Start}, {delay: 500 * time.Second, data: fixture.Progress}, {delay: 500 * time.Second, data: fixture.Finish}}, wantElapsed: 700 * time.Second, wantCode: "request_canceled", wantPhase: "stream", wantOutcome: "canceled"},
		} {
			t.Run(protocol+"/"+test.name, func(t *testing.T) {
				calls := 0
				var stream *timedStreamBody
				client, store := newCandidateTestClient(t, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
						trace.WroteHeaders()
					}
					if test.headerDelay > 0 {
						select {
						case <-r.Context().Done():
							return nil, r.Context().Err()
						case <-time.After(test.headerDelay):
						}
					}
					stream = &timedStreamBody{ctx: r.Context(), steps: append([]timedStreamStep(nil), test.steps...)}
					status := test.status
					if status == 0 {
						status = 200
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: stream}, nil
				})})
				defer store.Close()
				client.Protocol = protocol
				client.Limits.UpstreamTimeoutSeconds = test.totalSeconds
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if test.parentDeadline > 0 {
						var deadlineCancel context.CancelFunc
						ctx, deadlineCancel = context.WithTimeout(ctx, test.parentDeadline)
						defer deadlineCancel()
					}
					if test.cancelAfter > 0 {
						timer := time.AfterFunc(test.cancelAfter, cancel)
						defer timer.Stop()
					}
					started := time.Now()
					var results []HTTPAttemptResult
					var emitted strings.Builder
					delayed := false
					terminal, id, err := client.CallFromKeyCandidates(protocol, ctx, fixture.Request, "m", func(_ string, frame []byte) error {
						emitted.Write(frame)
						if !delayed && test.sinkDelay > 0 {
							delayed = true
							time.Sleep(test.sinkDelay)
						}
						return nil
					}, CallOptions{}, []string{"fixture-key", "unused-key"}, KeyCandidateCallbacks{Attempt: func(int) func(HTTPAttemptResult) {
						return func(result HTTPAttemptResult) { results = append(results, result) }
					}})
					if calls != 1 || len(results) != 1 || results[0].Outcome != test.wantOutcome || time.Since(started) != test.wantElapsed || stream != nil && !stream.closed {
						t.Fatalf("calls=%d results=%v elapsed=%s err=%v", calls, results, time.Since(started), err)
					}
					if streamSignalsCompletion(protocol, []byte(emitted.String())) {
						t.Fatal("completion escaped the audit gate")
					}
					if test.wantCode == "" {
						if err != nil || !streamSignalsCompletion(protocol, terminal) {
							t.Fatalf("terminal=%s err=%v", terminal, err)
						}
					} else {
						var failure *CallError
						if !errors.As(err, &failure) || failure.Code != test.wantCode || failure.Failure == nil || failure.Failure.Phase != test.wantPhase || terminal != nil {
							t.Fatalf("terminal=%s err=%+v", terminal, failure)
						}
					}
					rows, readErr := store.Recent(context.Background(), 10)
					if readErr != nil || len(rows) != 1 || rows[0].ID != id || rows[0].ErrorCode != test.wantCode {
						t.Fatalf("audit=%+v err=%v", rows, readErr)
					}
					wantStatus := "ok"
					if test.wantCode == "request_canceled" {
						wantStatus = "canceled"
					} else if test.wantCode != "" {
						wantStatus = "error"
					}
					if rows[0].Status != wantStatus || test.wantCode != "" && (rows[0].OutputTokens != nil || rows[0].EstimatedCost != nil) {
						t.Fatalf("incorrect status/unknown accounting: %+v", rows[0])
					}
				})
			})
		}
	}
}

func (b *timedStreamBody) Close() error { b.closed = true; return nil }

func TestActiveStreamsSurviveDefault600SecondLifetime(t *testing.T) {
	fixtures := timeoutStreamFixtures(t)
	for _, clientProtocol := range []string{"openai", "anthropic"} {
		for _, providerProtocol := range []string{"openai", "anthropic"} {
			t.Run(clientProtocol+"-to-"+providerProtocol, func(t *testing.T) {
				var calls int
				var stream *timedStreamBody
				fixture := fixtures[providerProtocol]
				client, store := newCandidateTestClient(t, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					stream = &timedStreamBody{ctx: r.Context(), steps: []timedStreamStep{
						{data: fixture.Start},
						{delay: 500 * time.Second, data: fixture.Progress},
						{delay: 500 * time.Second, data: fixture.Progress},
						{delay: 500 * time.Second, data: fixture.Finish},
					}}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: stream}, nil
				})})
				defer store.Close()
				client.Protocol = providerProtocol
				synctest.Test(t, func(t *testing.T) {
					started := time.Now()
					var emitted strings.Builder
					terminal, id, err := client.CallFrom(clientProtocol, context.Background(), fixtures[clientProtocol].Request, "m", func(_ string, frame []byte) error {
						emitted.Write(frame)
						return nil
					}, CallOptions{})
					if err != nil || len(terminal) == 0 || calls != 1 || stream == nil || !stream.closed || time.Since(started) != 1500*time.Second {
						t.Fatalf("calls=%d elapsed=%s terminal=%s err=%v", calls, time.Since(started), terminal, err)
					}
					if !strings.Contains(emitted.String(), "working") || streamSignalsCompletion(clientProtocol, []byte(emitted.String())) || !streamSignalsCompletion(clientProtocol, terminal) {
						t.Fatal("completion escaped the audit gate, or progress was lost")
					}
					rows, err := store.Recent(context.Background(), 10)
					if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Status != "ok" || rows[0].InputTokens == nil || *rows[0].InputTokens != 2 || rows[0].OutputTokens == nil || *rows[0].OutputTokens != 1 {
						t.Fatalf("audit=%+v err=%v", rows, err)
					}
				})
			})
		}
	}
}

type longStreamTransport struct{}

func (longStreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &longStreamBody{ctx: r.Context()}, Request: r}, nil
}

type longStreamBody struct {
	ctx  context.Context
	sent bool
}

func (b *longStreamBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	b.sent = true
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(61 * time.Second):
	}
	data := `data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\ndata: [DONE]\n\n"
	return copy(p, data), nil
}
func (*longStreamBody) Close() error { return nil }

func TestLongStreamDefaultAndExplicitDeadline(t *testing.T) {
	store, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit-60s"}[explicit], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate, _ := limiter.NewConcurrencyGate(1)
				client := &Client{Protocol: "openai", BaseURL: "http://local.test/v1", APIKey: "provider-secret", Upstream: "test", HTTP: &http.Client{Transport: longStreamTransport{}}, Ledger: store, Gate: gate}
				if explicit {
					client.Limits.UpstreamTimeoutSeconds = 60
				}
				body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":true}`)
				terminal, _, err := client.CallStream(context.Background(), body, "test-model", func(string, []byte) error { return nil })
				if explicit {
					if err == nil || err.Error() != "upstream_timeout" || terminal != nil {
						t.Fatalf("explicit deadline: terminal=%s error=%v", terminal, err)
					}
				} else if err != nil || !strings.Contains(string(terminal), "[DONE]") {
					t.Fatalf("61-second stream: terminal=%s error=%v", terminal, err)
				}
			})
		})
	}
}

func TestNonstreamRetainsDefault60SecondDeadline(t *testing.T) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(timeoutStreamFixtures(t)["openai"].Request, &request); err != nil {
		t.Fatal(err)
	}
	delete(request, "stream")
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	client, store := newCandidateTestClient(t, &http.Client{Transport: longStreamTransport{}})
	defer store.Close()
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		response, id, err := client.Call(context.Background(), body, "m")
		if err == nil || err.Error() != "upstream_timeout" || response != nil || time.Since(started) != 60*time.Second {
			t.Fatalf("elapsed=%s response=%s err=%v", time.Since(started), response, err)
		}
		rows, err := store.Recent(context.Background(), 10)
		if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Status != "error" || rows[0].ErrorCode != "upstream_timeout" {
			t.Fatalf("audit=%+v err=%v", rows, err)
		}
	})
}
