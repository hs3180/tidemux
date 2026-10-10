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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

func availabilityFixture() (*availabilityState, *time.Time) {
	now := time.Unix(1791600000, 0)
	s := newAvailabilityState(nil)
	s.now = func() time.Time { return now }
	s.jitter = func(base time.Duration) time.Duration { return base / 5 }
	s.activate(1, map[string]uint64{"a": 1, "b": 2})
	return s, &now
}

func failAvailability(t *testing.T, s *availabilityState, model string, category adapter.ProviderErrorCategory) {
	t.Helper()
	lease, blocked := s.begin(1, "a", model, 1)
	if blocked != nil {
		t.Fatal(blocked)
	}
	s.complete(lease, false, &adapter.CallError{Category: category}, nil)
}

func TestAvailabilityScopesBackoffAndOneProbe(t *testing.T) {
	s, now := availabilityFixture()
	failAvailability(t, s, "one", adapter.ProviderErrorModelNotFound)
	if s.available(1, "a", "one", 1) || !s.available(1, "a", "two", 1) || !s.available(1, "b", "one", 2) {
		t.Fatal("model failure escaped its scope")
	}
	entry := s.entries[availabilityTarget{"a", "one", 1}]
	if entry.reason != "model_not_found" || entry.retryAt.Sub(*now) != 36*time.Second {
		t.Fatalf("initial cooldown: %+v", entry)
	}
	*now = now.Add(36 * time.Second)
	var probes atomic.Int32
	var winner *availabilityLease
	var mu sync.Mutex
	var group sync.WaitGroup
	for n := 0; n < 64; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			lease, blocked := s.begin(1, "a", "one", 1)
			if blocked == nil {
				probes.Add(1)
				mu.Lock()
				winner = lease
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	if probes.Load() != 1 {
		t.Fatalf("probes=%d", probes.Load())
	}
	s.complete(winner, false, &adapter.CallError{Category: adapter.ProviderErrorModelNotFound}, nil)
	entry = s.entries[availabilityTarget{"a", "one", 1}]
	if entry.failures != 2 || entry.retryAt.Sub(*now) != 72*time.Second {
		t.Fatalf("continued failure: %+v", entry)
	}
	*now = now.Add(72 * time.Second)
	lease, blocked := s.begin(1, "a", "one", 1)
	if blocked != nil {
		t.Fatal(blocked)
	}
	s.complete(lease, false, nil, nil)
	if !s.available(1, "a", "one", 1) || s.entries[availabilityTarget{"a", "one", 1}].state() != "available" {
		t.Fatal("successful inference did not restore model")
	}
	failAvailability(t, s, "one", adapter.ProviderErrorInsufficientBalance)
	if s.available(1, "a", "two", 1) || !s.available(1, "b", "one", 2) {
		t.Fatal("balance failure did not stay provider scoped")
	}
	if wait := s.entries[availabilityTarget{"a", "", 1}].retryAt.Sub(*now); wait != 6*time.Minute {
		t.Fatal(wait)
	}
	if delay := s.backoff("model_not_found", 16, 0); delay != 36*time.Minute {
		t.Fatal("backoff not bounded", delay)
	}
	if delay := s.backoff("model_not_found", 1, 48*time.Hour); delay != 24*time.Hour {
		t.Fatal("upstream wait not bounded", delay)
	}
}

func TestAvailabilityFencesDispatchAndCompletion(t *testing.T) {
	s, now := availabilityFixture()
	queued, _ := s.begin(1, "a", "one", 1)
	failAvailability(t, s, "one", adapter.ProviderErrorTemporarilyUnavailable)
	if s.dispatch(queued) == nil {
		t.Fatal("queued request dispatched after another call failed")
	}
	s.complete(queued, false, nil, nil)
	if s.available(1, "a", "one", 1) {
		t.Fatal("earlier success erased latest failure")
	}
	*now = now.Add(time.Minute)
	oldProbe, _ := s.begin(1, "a", "one", 1)
	s.activate(2, map[string]uint64{"a": 1, "b": 2})
	s.complete(oldProbe, false, nil, nil)
	if s.entries[availabilityTarget{"a", "one", 1}].state() != "cooling" {
		t.Fatal("old epoch confirmed recovery")
	}
	newProbe, blocked := s.begin(2, "a", "one", 1)
	if blocked != nil {
		t.Fatal("reload stranded probe", blocked)
	}
	s.activate(3, map[string]uint64{"a": 3})
	s.complete(newProbe, false, &adapter.CallError{Category: adapter.ProviderErrorModelNotFound}, nil)
	if len(s.entries) != 0 {
		t.Fatal("old generation polluted rotated credentials")
	}
	s.activate(4, map[string]uint64{})
	s.activate(5, map[string]uint64{"a": 4})
	if !s.available(5, "a", "one", 4) {
		t.Fatal("removal/re-addition inherited failure")
	}
}

func TestAvailabilityNonHealthErrorsAndIdleBound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     *adapter.CallError
		ctx     error
		handled bool
	}{
		{"capacity", &adapter.CallError{Code: "active_session_limit", UpstreamNotAttempted: true}, nil, true},
		{"budget", &adapter.CallError{Code: "budget_hard_limit", UpstreamNotAttempted: true}, nil, true},
		{"cancel", &adapter.CallError{Category: adapter.ProviderErrorModelNotFound}, context.Canceled, false},
		{"429", &adapter.CallError{Category: adapter.ProviderErrorRateLimited}, nil, false},
		{"auth", &adapter.CallError{Category: adapter.ProviderErrorAuthentication}, nil, false},
		{"format", &adapter.CallError{Category: adapter.ProviderErrorInvalidRequest}, nil, false},
		{"unknown", &adapter.CallError{Code: "upstream_error"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := availabilityFixture()
			lease, _ := s.begin(1, "a", "one", 1)
			s.complete(lease, tc.handled, tc.err, tc.ctx)
			if len(s.entries) != 0 {
				t.Fatal("non-health error recorded a failure")
			}
		})
	}
	s, now := availabilityFixture()
	for n := 0; n < availabilityLimit+100; n++ {
		failAvailability(t, s, fmt.Sprintf("model-%d", n), adapter.ProviderErrorModelNotFound)
	}
	if len(s.entries) != availabilityLimit {
		t.Fatal("target state not bounded", len(s.entries))
	}
	lease, _ := s.begin(1, "a", strings.Repeat("private-model-", 20), 1)
	s.complete(lease, false, &adapter.CallError{Category: adapter.ProviderErrorModelNotFound}, nil)
	if len(s.entries) != availabilityLimit {
		t.Fatal("oversized model retained")
	}
	h := &handler{handlerRuntime: &handlerRuntime{availability: s}, providers: map[string]Provider{"a": {}}, providerGenerations: map[string]uint64{"a": 1}}
	for hour := 0; hour < 24; hour++ {
		_ = h.availabilityReport() // querying must not keep idle targets alive
		*now = now.Add(time.Hour)
	}
	*now = now.Add(time.Minute)
	if !s.available(1, "a", "model-1", 1) || len(s.entries) != 0 {
		t.Fatal("idle state not reclaimed")
	}
}

func TestAvailabilityAcrossRoutingEntrypoints(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, strategy := range []string{"random", "price_priority"} {
			for _, session := range []string{"", "stable-session"} {
				t.Run(protocol+"/"+strategy+"/"+session, func(t *testing.T) {
					var badCalls, goodCalls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodGet {
							io.WriteString(w, `{"object":"list","data":[{"id":"one"},{"id":"two"}]}`)
							return
						}
						var body struct{ Model string }
						json.NewDecoder(r.Body).Decode(&body)
						if strings.HasPrefix(r.URL.Path, "/a/") && body.Model == "one" {
							badCalls.Add(1)
							w.WriteHeader(404)
							io.WriteString(w, `{"error":{"code":"missing_model"}}`)
							return
						}
						goodCalls.Add(1)
						if protocol == "openai" {
							io.WriteString(w, openAIResponseForModel(body.Model))
						} else {
							io.WriteString(w, fmt.Sprintf(`{"id":"fixture","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, body.Model))
						}
					}))
					defer upstream.Close()
					c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
					for _, ref := range []string{"a", "b"} {
						addRoutingProvider(&c, ref, upstream.URL+"/"+ref, protocol, "one", "two")
						p := c.Providers[ref]
						p.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "missing_model", HTTPStatus: 404, Category: adapter.ProviderErrorModelNotFound}}
						price := 1.0
						if ref == "b" {
							price = 2
						}
						p.Prices = map[string]adapter.Price{"one": {Currency: "USD", Source: "test", Version: "1", InputCacheHit: &price, InputCacheMiss: &price, Output: &price}}
						c.Providers[ref] = p
					}
					configureRouting(&c, RoutingConfig{SharedModelStrategy: strategy})
					c.AutoChain = []AutoChainEntry{{Provider: "a", Model: "one"}, {Provider: "b", Model: "one"}}
					gatewayHandler, closeHandler, err := NewHandler(c, upstream.Client())
					if err != nil {
						t.Fatal(err)
					}
					defer closeHandler()
					h := gatewayHandler.(*handler)
					h.randomIndex = func(int) int { return 0 }
					body := func(model string) string {
						return fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"private-body"}]}`, model)
					}
					failed := affinityGatewayRequest(h, protocol, session, body("a/one"))
					if failed.Code != 404 {
						t.Fatal(failed.Code, failed.Body)
					}
					cooled := affinityGatewayRequest(h, protocol, session, body("a/one"))
					if cooled.Code != 503 || cooled.Header().Get("Retry-After") == "" || !strings.Contains(cooled.Body.String(), "model_not_found_cooling_down") {
						t.Fatal(cooled.Code, cooled.Header(), cooled.Body)
					}
					for _, model := range []string{"one", "auto", "a/two", "b/one"} {
						if out := affinityGatewayRequest(h, protocol, session, body(model)); out.Code != 200 {
							t.Fatalf("%s: %d %s", model, out.Code, out.Body)
						}
					}
					if badCalls.Load() != 1 || goodCalls.Load() != 4 {
						t.Fatalf("dispatch counts bad=%d good=%d", badCalls.Load(), goodCalls.Load())
					}
					query := httptest.NewRequest(http.MethodGet, "/tidemux/availability-status", nil)
					out := httptest.NewRecorder()
					h.ServeHTTP(out, query)
					if out.Code != 401 || strings.Contains(out.Body.String(), "generation") {
						t.Fatal("unauthenticated query exposed state")
					}
					query.Header.Set("Authorization", "Bearer local-secret")
					out = httptest.NewRecorder()
					h.ServeHTTP(out, query)
					if out.Code != 200 || !strings.Contains(out.Body.String(), "model_not_found") {
						t.Fatal(out.Code, out.Body)
					}
					for _, private := range []string{"private-body", "stable-session", "provider-key", "local-secret", c.LedgerPath} {
						if strings.Contains(out.Body.String(), private) {
							t.Fatal("private data in query", private)
						}
					}
				})
			}
		}
	}
}

// A real queued request must observe the first call's health result before it
// can send its own HTTP request, then rebind only because it was not dispatched.
func TestAvailabilityQueuedDispatchRechecksConfirmedFailure(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			var aCalls, bCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					io.WriteString(w, `{"object":"list","data":[{"id":"one"}]}`)
					return
				}
				if strings.HasPrefix(r.URL.Path, "/a/") {
					if aCalls.Add(1) == 1 {
						close(entered)
						<-release
					}
					w.WriteHeader(404)
					io.WriteString(w, `{"error":{"code":"missing_model"}}`)
					return
				}
				bCalls.Add(1)
				if protocol == "openai" {
					io.WriteString(w, openAIResponseForModel("one"))
				} else {
					io.WriteString(w, `{"id":"fixture","type":"message","role":"assistant","model":"one","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				}
			}))
			defer upstream.Close()
			defer unblock()
			c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
			c.MaxInFlight, c.MaxActiveSessions = 1, 8
			for _, ref := range []string{"a", "b"} {
				addRoutingProvider(&c, ref, upstream.URL+"/"+ref, protocol, "one")
				p := c.Providers[ref]
				p.MaxActiveSessions = 8
				p.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "missing_model", HTTPStatus: 404, Category: adapter.ProviderErrorModelNotFound}}
				c.Providers[ref] = p
			}
			configureRouting(&c, RoutingConfig{SharedModelStrategy: "random"})
			gatewayHandler, closeHandler, err := NewHandler(c, upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeHandler()
			h := gatewayHandler.(*handler)
			h.randomIndex = func(int) int { return 0 }
			body := func(model string) string {
				return fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"queued-body"}]}`, model)
			}
			first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
			go func() { first <- affinityGatewayRequest(h, protocol, "first", body("a/one")) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first request did not dispatch")
			}
			go func() { second <- affinityGatewayRequest(h, protocol, "second", body("one")) }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, current, _ := h.sessions.Stats()
				if current == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("second request did not enter admission")
				}
				time.Sleep(time.Millisecond)
			}
			unblock()
			if out := <-first; out.Code != 404 {
				t.Fatal(out.Code, out.Body)
			}
			if out := <-second; out.Code != 200 {
				t.Fatal(out.Code, out.Body)
			}
			if aCalls.Load() != 1 || bCalls.Load() != 1 {
				t.Fatalf("queued call repeated known failure: a=%d b=%d", aCalls.Load(), bCalls.Load())
			}
		})
	}
}
