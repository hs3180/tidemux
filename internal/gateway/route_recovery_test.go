package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

func TestAutoChainExhaustedRouteBecomesEligibleWithoutReload(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	s := newAutoChainState([]AutoChainEntry{{Provider: "a", Model: "one"}}, defaultAutoChainSessionTTL)
	s.now = func() time.Time { return now }
	s.recordFailure(0, 1, &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, nil, false)
	if _, ok := s.selectRoute(nil); ok {
		t.Fatal("failed route was selected during cooldown")
	}
	now = now.Add(30 * time.Second)
	if route, ok := s.selectRoute(nil); !ok || route.provider != "a" {
		t.Fatal("exhausted auto chain never reconsidered its recovered provider")
	}
}

func TestRouteRecoveryRespectsBillingAndUpstreamCooldown(t *testing.T) {
	for _, test := range []struct {
		name    string
		callErr *adapter.CallError
		wait    time.Duration
	}{
		{"balance", &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorInsufficientBalance}, 5 * time.Minute},
		{"longer_retry_delay", &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorTemporarilyUnavailable, Cooldown: 2 * time.Minute}, 2 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(1_790_000_000, 0)
			s := newAutoChainState([]AutoChainEntry{{Provider: "a", Model: "one"}}, defaultAutoChainSessionTTL)
			s.now = func() time.Time { return now }
			s.recordFailure(0, 1, test.callErr, nil, false)
			now = now.Add(test.wait - time.Nanosecond)
			if _, ok := s.beginRecovery(0); ok {
				t.Fatal("probe ignored required cooldown")
			}
			now = now.Add(time.Nanosecond)
			if p, ok := s.beginRecovery(0); !ok || p == nil {
				t.Fatal("probe never became eligible")
			}
		})
	}
}

func TestRouteRecoveryClaimsAndCompletion(t *testing.T) {
	for _, mode := range []string{"auto", "shared"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1_790_000_000, 0)
			missing := &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}
			var fail func()
			var begin func() (*routeFailure, bool)
			var finish func(*routeFailure, bool, error, error)
			if mode == "auto" {
				s := newAutoChainState([]AutoChainEntry{{Provider: "a", Model: "one"}}, defaultAutoChainSessionTTL)
				s.now = func() time.Time { return now }
				fail = func() { s.recordFailure(0, 1, missing, nil, false) }
				begin = func() (*routeFailure, bool) { return s.beginRecovery(0) }
				finish = func(p *routeFailure, handled bool, err, ctxErr error) { s.finishRecovery(0, p, handled, err, ctxErr) }
			} else {
				a := &sharedModelAffinity{now: func() time.Time { return now }}
				a.activate(1, func(sharedModelBinding) bool { return true })
				route := sharedModelRoute{provider: "a", model: "one", generation: 1}
				fail = func() { a.recordFailure(1, "a", "one", 1, missing, nil, false) }
				begin = func() (*routeFailure, bool) { return a.beginRecovery(1, route) }
				finish = func(p *routeFailure, handled bool, err, ctxErr error) {
					a.finishRecovery(1, route, p, handled, err, ctxErr)
				}
			}
			fail()
			if _, ok := begin(); ok {
				t.Fatal("probe admitted before cooldown")
			}
			now = now.Add(routeRecoveryCooldown)
			var probes atomic.Int32
			var winner *routeFailure
			var group sync.WaitGroup
			for i := 0; i < 64; i++ {
				group.Add(1)
				go func() {
					defer group.Done()
					if p, ok := begin(); ok {
						probes.Add(1)
						winner = p
					}
				}()
			}
			group.Wait()
			if probes.Load() != 1 || winner == nil {
				t.Fatalf("concurrent recovery probes=%d, want exactly one", probes.Load())
			}
			// A response with an unclassified error is not proof of recovery.
			finish(winner, false, &adapter.CallError{Status: 502, Code: "upstream_error"}, nil)
			if _, ok := begin(); ok {
				t.Fatal("failed recovery probe did not renew cooldown")
			}
			now = now.Add(routeRecoveryCooldown)
			for _, rejection := range []struct {
				handled     bool
				err, ctxErr error
			}{
				{handled: true}, // Capacity or budget rejected locally.
				{ctxErr: context.Canceled},
				{err: &adapter.CallError{Code: "provider_keys_cooling_down", UpstreamNotAttempted: true}},
			} {
				p, ok := begin()
				if !ok || p == nil {
					t.Fatal("local rejection cleared or stranded the failure marker")
				}
				finish(p, rejection.handled, rejection.err, rejection.ctxErr)
			}
			p, _ := begin()
			fail() // An older in-flight failure is newer evidence than this probe.
			finish(p, false, nil, nil)
			if _, ok := begin(); ok {
				t.Fatal("stale successful probe erased a newer failure")
			}
			now = now.Add(routeRecoveryCooldown)
			p, _ = begin()
			finish(p, false, nil, nil)
			if p, ok := begin(); !ok || p != nil {
				t.Fatal("successful upstream call did not clear failed-route state")
			}
		})
	}
}

func TestRouteRecoveryReloadDoesNotStrandProbeOrAcceptOldCompletion(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	failure := &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}
	entry := AutoChainEntry{Provider: "a", Model: "one"}
	s := newAutoChainState([]AutoChainEntry{entry}, defaultAutoChainSessionTTL)
	s.now = func() time.Time { return now }
	s.recordFailure(0, 1, failure, nil, false)
	now = now.Add(routeRecoveryCooldown)
	oldProbe, _ := s.beginRecovery(0)
	preserve := map[AutoChainEntry]bool{entry: true}
	next := s.reconfigured([]AutoChainEntry{entry}, defaultAutoChainSessionTTL, preserve, true)
	s.finishRecovery(0, oldProbe, false, nil, nil)
	if probe, ok := next.beginRecovery(0); !ok || probe == nil {
		t.Fatal("old auto view's probe completion changed or stranded the new view")
	}
	a := &sharedModelAffinity{now: func() time.Time { return now }}
	valid := func(sharedModelBinding) bool { return true }
	a.activate(1, valid)
	a.recordFailure(1, "a", "one", 1, failure, nil, false)
	now = now.Add(routeRecoveryCooldown)
	route := sharedModelRoute{provider: "a", model: "one", generation: 1}
	oldProbe, _ = a.beginRecovery(1, route)
	a.activate(2, valid)
	a.finishRecovery(1, route, oldProbe, false, nil, nil)
	if probe, ok := a.beginRecovery(2, route); !ok || probe == nil {
		t.Fatal("old shared view's probe completion changed or stranded the new view")
	}
}

func TestGatewayRecoveryConcurrentDispatchAndHealthyBinding(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, mode := range []string{"auto", "shared"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", protocol, mode, stream), func(t *testing.T) {
					var phase atomic.Int32
					var mu sync.Mutex
					var posts []string
					entered, release := make(chan struct{}), make(chan struct{})
					var releaseOnce sync.Once
					unblock := func() { releaseOnce.Do(func() { close(release) }) }
					t.Cleanup(unblock)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodGet {
							_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"one"}]}`)
							return
						}
						provider := strings.Split(r.URL.Path, "/")[1]
						mu.Lock()
						posts = append(posts, provider)
						mu.Unlock()
						if provider == "a" && phase.Load() == 0 {
							if !stream {
								w.WriteHeader(404)
								_, _ = io.WriteString(w, `{"error":{"code":"missing_model"}}`)
							} else {
								w.Header().Set("Content-Type", "text/event-stream")
								if protocol == "openai" {
									_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
								} else {
									_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"one\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
								}
								_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"code\":\"missing_model\",\"status\":404}}\n\n")
								return
							}
							return
						}
						if provider == "a" && phase.Load() == 1 {
							close(entered)
							<-release
						}
						if protocol == "openai" {
							_, _ = io.WriteString(w, openAIResponseForModel("one"))
						} else {
							_, _ = io.WriteString(w, `{"id":"fixture","type":"message","role":"assistant","model":"one","content":[{"type":"text","text":"completed"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
						}
					}))
					defer upstream.Close()
					defer unblock()
					c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
					c.MaxInFlight = 16
					for _, ref := range []string{"a", "b"} {
						addRoutingProvider(&c, ref, upstream.URL+"/"+ref, protocol, "one")
						p := c.Providers[ref]
						p.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "missing_model", HTTPStatus: 404, Category: adapter.ProviderErrorModelNotFound}}
						c.Providers[ref] = p
					}
					configureRouting(&c, RoutingConfig{SharedModelStrategy: "random"})
					if mode == "auto" {
						c.AutoChain = []AutoChainEntry{{Provider: "a", Model: "one"}, {Provider: "b", Model: "one"}}
					}
					gateway, closeHandler, err := NewHandler(c, upstream.Client())
					if err != nil {
						t.Fatal(err)
					}
					defer closeHandler()
					h := gateway.(*handler)
					h.randomIndex = func(int) int { return 0 }
					var clock atomic.Int64
					clock.Store(time.Now().UnixNano())
					now := func() time.Time { return time.Unix(0, clock.Load()) }
					h.autoChain.now, h.sharedAffinity.now = now, now
					h.availability.now = now
					h.availability.jitter = func(time.Duration) time.Duration { return 0 }
					model := "one"
					if mode == "auto" {
						model = "auto"
					}
					request := func(session string, streaming bool) *httptest.ResponseRecorder {
						return affinityGatewayRequest(h, protocol, session, fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"private-recovery-body"}]}`, model, streaming))
					}
					failed := request("failed-session", stream)
					if !stream && failed.Code != 404 || stream && (failed.Code != 200 || !strings.Contains(failed.Body.String(), "partial") || !strings.Contains(failed.Body.String(), "event: error")) {
						t.Fatalf("initial failure status=%d body=%s", failed.Code, failed.Body.String())
					}
					if r := request("healthy-session", false); r.Code != 200 {
						t.Fatal(r.Body.String())
					}
					clock.Add(int64(routeRecoveryCooldown))
					phase.Store(1)
					done := make(chan *httptest.ResponseRecorder, 1)
					go func() { done <- request("recovery-session", false) }()
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("recovery request did not reach a")
					}
					var group sync.WaitGroup
					for i := 0; i < 8; i++ {
						group.Add(1)
						go func(i int) {
							defer group.Done()
							if r := request(fmt.Sprintf("concurrent-%d", i), false); r.Code != 200 {
								t.Errorf("concurrent request rejected: %d", r.Code)
							}
						}(i)
					}
					group.Wait()
					phase.Store(2)
					unblock()
					if r := <-done; r.Code != 200 {
						t.Fatal(r.Body.String())
					}
					if r := request("fresh-session", false); r.Code != 200 {
						t.Fatal(r.Body.String())
					}
					if r := request("healthy-session", false); r.Code != 200 {
						t.Fatal(r.Body.String())
					}
					if mode == "auto" {
						if r := request("", false); r.Code != 200 {
							t.Fatal(r.Body.String())
						}
					}
					mu.Lock()
					actual := append([]string(nil), posts...)
					mu.Unlock()
					want := []string{"a", "b", "a"}
					for i := 0; i < 8; i++ {
						want = append(want, "b")
					}
					want = append(want, "a", "b")
					if mode == "auto" {
						want = append(want, "a")
					}
					if strings.Join(actual, ",") != strings.Join(want, ",") {
						t.Fatalf("replayed request, concurrent probes or moved healthy binding: %v, want %v", actual, want)
					}
					rows, err := h.ledger.Recent(context.Background(), 30)
					if err != nil || len(rows) != len(actual) {
						t.Fatalf("audit/dispatch count mismatch: rows=%d posts=%d err=%v", len(rows), len(actual), err)
					}
				})
			}
		}
	}
}

func TestSharedMissingModelBecomesEligibleWithoutReload(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	a := &sharedModelAffinity{now: func() time.Time { return now }}
	a.activate(1, func(sharedModelBinding) bool { return true })
	a.recordFailure(1, "a", "one", 1, &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, nil, false)
	key := newSharedModelSessionKey("fixture", "anthropic", "one", "recovery")
	selectProvider := func() bool {
		_, ok := a.selectProviderForView(key, "one", 1, []string{"a"}, func(string) bool { return true }, func(int) int { return 0 }, func(string) uint64 { return 1 })
		return ok
	}
	if selectProvider() {
		t.Fatal("failed model was selected during cooldown")
	}
	now = now.Add(30 * time.Second)
	if !selectProvider() {
		t.Fatal("a recovered missing model remained permanently excluded")
	}
}
