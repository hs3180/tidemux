package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/diagnostics"
)

func TestStreamFailureNewRequestRebindsWithoutReplayingOutput(t *testing.T) {
	for _, providerProtocol := range []string{"openai", "anthropic"} {
		for _, clientProtocol := range []string{"openai", "anthropic"} {
			for _, strategy := range []string{"random", "price_priority", "auto"} {
				t.Run(providerProtocol+"/"+clientProtocol+"/"+strategy, func(t *testing.T) {
					var posts atomic.Int32
					var recovered atomic.Bool
					var lastProvider atomic.Value
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == "GET" {
							io.WriteString(w, `{"data":[{"id":"one"},{"id":"two"}]}`)
							return
						}
						posts.Add(1)
						provider := strings.Split(r.URL.Path, "/")[1]
						lastProvider.Store(provider)
						var body struct{ Model string }
						json.NewDecoder(r.Body).Decode(&body)
						if provider == "a" && body.Model == "one" && !recovered.Load() {
							w.Header().Set("Content-Type", "text/event-stream")
							if providerProtocol == "anthropic" {
								io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"fixture","role":"assistant","model":"one","usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
								io.WriteString(w, "event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
								io.WriteString(w, "event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`+"\n\n")
							} else {
								io.WriteString(w, "data: "+`{"id":"fixture","object":"chat.completion.chunk","created":1,"model":"one","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
							}
							w.(http.Flusher).Flush()
							io.WriteString(w, "event: error\ndata: "+`{"error":{"code":"1999","message":"PRIVATE_UPSTREAM_MESSAGE"}}`+"\n\n")
							return
						}
						if providerProtocol == "openai" {
							io.WriteString(w, openAIResponseForModel(body.Model))
						} else {
							io.WriteString(w, fmt.Sprintf(`{"id":"fixture","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"completed"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, body.Model))
						}
					}))
					defer upstream.Close()
					c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
					for i, ref := range []string{"a", "b"} {
						addRoutingProvider(&c, ref, upstream.URL+"/"+ref, providerProtocol, "one", "two")
						p := c.Providers[ref]
						price := float64(i + 1)
						p.Prices = map[string]adapter.Price{"one": {Currency: "USD", Source: "test", Version: "1", InputCacheHit: &price, InputCacheMiss: &price, Output: &price}}
						c.Providers[ref] = p
					}
					configureRouting(&c, RoutingConfig{SharedModelStrategy: strategy})
					if strategy == "auto" {
						configureRouting(&c, RoutingConfig{})
						c.AutoChain = []AutoChainEntry{{Provider: "a", Model: "one"}, {Provider: "b", Model: "one"}}
					}
					raw, closeDB, err := NewHandler(c, upstream.Client())
					if err != nil {
						t.Fatal(err)
					}
					defer closeDB()
					h := raw.(*handler)
					h.randomIndex = func(int) int { return 0 }
					model := "one"
					if strategy == "auto" {
						model = "auto"
					}
					request := func(session, model string, stream bool, status, attempts int, provider string) *httptest.ResponseRecorder {
						t.Helper()
						before := posts.Load()
						body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"stream":%t,"messages":[{"role":"user","content":"fixture"}]}`, model, stream)
						w := affinityGatewayRequest(h, clientProtocol, session, body)
						if w.Code != status || int(posts.Load()-before) != attempts || attempts > 0 && lastProvider.Load() != provider {
							t.Fatalf("status=%d attempts=%d provider=%v body=%s", w.Code, posts.Load()-before, lastProvider.Load(), w.Body.String())
						}
						return w
					}
					failed := request("active", model, true, 200, 1, "a")
					if !strings.Contains(failed.Body.String(), "partial") || !strings.Contains(failed.Body.String(), "event: error") || strings.Contains(failed.Body.String(), "PRIVATE_UPSTREAM_MESSAGE") {
						t.Fatal("partial error stream was lost, replayed or exposed provider text")
					}
					rows, err := h.ledger.Recent(context.Background(), 10)
					if err != nil || len(rows) != 1 || rows[0].Failure == nil || rows[0].Failure.Category != "unknown" || rows[0].Failure.Retryability != "unknown" {
						t.Fatalf("unknown upstream failure was reclassified: %+v %v", rows, err)
					}
					request("active", model, false, 200, 1, "b")
					request("active", "a/one", false, 503, 0, "")
					request("active", "a/two", false, 200, 1, "a")
					// A successful real probe restores a without moving the healthy
					// random/auto binding that already recovered onto b.
					h.availability.mu.Lock()
					h.availability.entries[availabilityTarget{"a", "one", h.providerGenerations["a"]}].retryAt = time.Now().Add(-time.Second)
					h.availability.mu.Unlock()
					recovered.Store(true)
					request("new", model, false, 200, 1, "a")
					want := "b"
					if strategy == "price_priority" {
						want = "a" // price routing deliberately has no affinity
					}
					request("active", model, false, 200, 1, want)
				})
			}
		}
	}
}

func TestAvailabilityUsesBoundedUpstreamFailureEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, code, source, phase, retryability, category, reason string
		status                                                    int
	}{
		{"unknown_sse", "upstream_stream_error", "stream_error", "stream", "unknown", "", "unclassified_stream_error", 200},
		{"terminal_sse", "upstream_stream_error", "stream_error", "stream", "terminal", "", "", 200},
		{"permission", "provider_permission_denied", "stream_error", "stream", "terminal", "permission_denied", "", 200},
		{"body_timeout", "upstream_timeout", "response", "response_body", "retryable", "", "upstream_timeout", 200},
		{"headers_timeout", "upstream_timeout", "transport", "response_headers", "retryable", "", "upstream_timeout", 0},
		{"stream_cut", "incomplete_upstream_stream", "stream", "stream", "retryable", "", "stream_interrupted", 200},
		{"server_error", "upstream_error", "http_error", "response_body", "retryable", "", "upstream_error", 503},
		{"bad_request", "upstream_error", "http_error", "response_body", "terminal", "", "", 400},
		{"conversion", "upstream_timeout", "conversion", "conversion", "retryable", "", "", 200},
		{"downstream", "downstream_write_error", "downstream", "stream", "terminal", "", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &adapter.CallError{Code: tc.code, Category: adapter.ProviderErrorCategory(tc.category), Failure: &diagnostics.Failure{Source: tc.source, Phase: tc.phase, HTTPStatus: tc.status, Retryability: tc.retryability}}
			modelScope, reason, _ := availabilityFailure(err, nil, false)
			if reason != tc.reason || reason != "" && !modelScope {
				t.Fatalf("scope=%t reason=%s", modelScope, reason)
			}
			if _, reason, _ := availabilityFailure(err, context.Canceled, false); reason != "" {
				t.Fatal("caller cancellation blamed the provider")
			}
		})
	}
	s, _ := availabilityFixture()
	for n := uint16(1); n <= 20; n++ {
		if wait := s.backoff("unclassified_stream_error", n, 0); wait != 36*time.Second {
			t.Fatalf("unknown error avoidance grew beyond its provisional bound: %s", wait)
		}
	}
}

func TestAutoRecoverySkipsIncompatibleServerToolProvider(t *testing.T) {
	h := affinityUnitHandler(t)
	h.providers["c"], h.clients["c"] = h.providers["b"], &adapter.Client{Protocol: "anthropic"}
	h.clients["a"].Protocol = "anthropic"
	h.providerGenerations = map[string]uint64{"a": 1, "b": 1, "c": 1}
	h.routingEpoch = 1
	h.availability = newAvailabilityState(nil)
	h.availability.activate(1, h.providerGenerations)
	h.config.AutoChain = []AutoChainEntry{{Provider: "a", Model: "shared-model"}, {Provider: "b", Model: "shared-model"}, {Provider: "c", Model: "shared-model"}}
	h.autoChain = newAutoChainState(h.config.AutoChain, 0)
	body := []byte(`{"model":"auto","max_tokens":16,"messages":[{"role":"user","content":"search"}],"tools":[{"name":"web_search","type":"web_search_20250305"}]}`)
	key := h.callerSessionKey("auto", "anthropic", "session")
	initial, code, _ := h.resolveRoutes("auto", "anthropic", body, key)
	if code != "" || initial[0].provider != "a" {
		t.Fatal("initial native route not selected")
	}
	lease, _ := h.availability.begin(1, "a", "shared-model", 1)
	h.availability.complete(lease, false, &adapter.CallError{Code: "upstream_stream_error", Failure: &diagnostics.Failure{Source: "stream_error", Retryability: "unknown"}}, nil)
	next, code, _ := h.resolveRoutes("auto", "anthropic", body, key)
	if code != "" || next[0].provider != "c" {
		t.Fatalf("recovery selected an incompatible provider: routes=%+v code=%s", next, code)
	}
	h.clients["a"].Protocol, h.clients["c"].Protocol = "openai", "openai"
	if _, code, status := h.resolveRoutes("auto", "anthropic", body, key); code != "unsupported_request_feature" || status != 400 {
		t.Fatal("no compatible provider lost its feature rejection")
	}
}

func TestUpstreamTimeoutNewRequestAvoidsRepeatedWait(t *testing.T) {
	var badCalls, goodCalls atomic.Int32
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"one"}]}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/a/") {
			badCalls.Add(1)
			io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		goodCalls.Add(1)
		io.WriteString(w, openAIResponseForModel("one"))
	}))
	defer func() { close(release); upstream.Close() }()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	for _, ref := range []string{"a", "b"} {
		addRoutingProvider(&c, ref, upstream.URL+"/"+ref, "openai", "one")
	}
	c.Limits.UpstreamTimeoutSeconds = 1
	configureRouting(&c, RoutingConfig{SharedModelStrategy: "random"})
	raw, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	h := raw.(*handler)
	h.randomIndex = func(int) int { return 0 }
	body := `{"model":"one","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	first := affinityGatewayRequest(h, "anthropic", "session", body)
	if first.Code != 504 || !strings.Contains(first.Body.String(), "upstream_timeout") || badCalls.Load() != 1 || goodCalls.Load() != 0 {
		t.Fatalf("timeout was replayed or lost: status=%d bad=%d good=%d", first.Code, badCalls.Load(), goodCalls.Load())
	}
	rows, err := h.ledger.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].Failure == nil || rows[0].Failure.Phase != "response_headers" {
		t.Fatalf("timeout phase was lost: %+v %v", rows, err)
	}
	next := affinityGatewayRequest(h, "anthropic", "session", body)
	if next.Code != 200 || badCalls.Load() != 1 || goodCalls.Load() != 1 {
		t.Fatalf("new request repeated the failed route: status=%d bad=%d good=%d", next.Code, badCalls.Load(), goodCalls.Load())
	}
}
