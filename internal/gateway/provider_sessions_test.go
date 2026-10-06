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
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

func providerCapacityFixture(t *testing.T, cap, global int, post func(http.ResponseWriter, *http.Request)) (*handler, Config, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"},{"id":"other-model"}]}`)
			return
		}
		calls.Add(1)
		if post != nil {
			post(w, r)
		} else {
			io.WriteString(w, responseBody("openai"))
		}
	}))
	t.Cleanup(upstream.Close)
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	c.MaxInFlight, c.MaxActiveSessions = 64, global
	addRoutingProvider(&c, "glm", upstream.URL, "openai", "custom-model", "other-model")
	addRoutingProvider(&c, "other", upstream.URL, "openai", "custom-model")
	p := c.Providers["glm"]
	p.MaxActiveSessions = cap
	c.Providers["glm"] = p
	h, closeGateway, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeGateway() })
	return h.(*handler), c, calls
}

func providerCapacityRequest(h http.Handler, protocol, session, ref, model string) *httptest.ResponseRecorder {
	return affinityGatewayRequest(h, protocol, session, requestBodyFor(protocol, ref, model))
}

func requireCapacityRejection(t *testing.T, out *httptest.ResponseRecorder, scope string, limit int) {
	t.Helper()
	var payload struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if out.Code != 429 || out.Header().Get("Retry-After") != "" || payload.Error["code"] != "active_session_limit" || payload.Error["scope"] != scope || payload.Error["limit"] != float64(limit) {
		t.Fatalf("capacity response=%d %s %s", out.Code, out.Header(), out.Body)
	}
}

func TestProviderCapacityGLMFiveIndependentFromOtherAndGlobal(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			h, _, calls := providerCapacityFixture(t, 5, 6, nil)
			for i := range 5 {
				if out := providerCapacityRequest(h, protocol, fmt.Sprint(i), "glm", "custom-model"); out.Code != 200 {
					t.Fatalf("admission %d: %d %s", i, out.Code, out.Body)
				}
			}
			requireCapacityRejection(t, providerCapacityRequest(h, protocol, "sixth", "glm", "custom-model"), "provider", 5)
			if calls.Load() != 5 {
				t.Fatalf("rejected request dispatched: %d", calls.Load())
			}
			if _, current, _ := h.sessions.Stats(); current != 5 {
				t.Fatalf("first provider refusal left a global lease: %d", current)
			}
			if _, current, _ := h.providerSessions["glm"].Stats(); current != 5 {
				t.Fatalf("first provider refusal changed provider occupancy: %d", current)
			}
			for _, r := range []struct{ session, ref, model string }{{"0", "glm", "other-model"}, {"different-sixth", "other", "custom-model"}, {"0", "other", "custom-model"}} {
				if out := providerCapacityRequest(h, protocol, r.session, r.ref, r.model); out.Code != 200 {
					t.Fatalf("reuse/independence: %d %s", out.Code, out.Body)
				}
			}
			requireCapacityRejection(t, providerCapacityRequest(h, protocol, "seventh", "other", "custom-model"), "gateway", 6)
			if calls.Load() != 8 {
				t.Fatalf("global refusal dispatched: %d", calls.Load())
			}
			if _, current, _ := h.sessions.Stats(); current != 6 {
				t.Fatalf("global sessions=%d", current)
			}
			req := httptest.NewRequest("GET", "/tidemux/session-status", nil)
			req.Header.Set("Authorization", "Bearer local-secret")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != 200 || strings.Contains(out.Body.String(), "sixth") || strings.Contains(out.Body.String(), "local-secret") {
				t.Fatalf("unsafe status: %d %s", out.Code, out.Body)
			}
			var status struct {
				Providers map[string]sessionCapacityStatus `json:"providers"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Providers["glm"].Current != 5 || status.Providers["other"].Current != 2 || status.Providers["glm"].Rejected != 1 {
				t.Fatalf("status=%+v", status)
			}
		})
	}
}

func TestProviderCapacitySessionHeaderMetadataAndProtocolNamespace(t *testing.T) {
	h, _, calls := providerCapacityFixture(t, 1, 2, nil)
	if out := providerCapacityRequest(h, "anthropic", "same", "glm", "custom-model"); out.Code != 200 {
		t.Fatal(out.Body)
	}
	body := strings.TrimSuffix(requestBodyFor("anthropic", "glm", "other-model"), "}") + `,"metadata":{"user_id":"same"}}`
	if out := affinityGatewayRequest(h, "anthropic", "", body); out.Code != 200 {
		t.Fatalf("metadata did not reuse header session: %d %s", out.Code, out.Body)
	}
	requireCapacityRejection(t, providerCapacityRequest(h, "openai", "same", "glm", "custom-model"), "provider", 1)
	if calls.Load() != 2 {
		t.Fatalf("protocol namespace collided: calls=%d", calls.Load())
	}
	unauthorized := httptest.NewRequest("GET", "/tidemux/session-status", nil)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, unauthorized)
	if out.Code != 401 || strings.Contains(out.Body.String(), "current") {
		t.Fatalf("unauthenticated capacity disclosure: %d %s", out.Code, out.Body)
	}
}

func TestProviderCapacityConcurrentAdmissionsAndSameSessionReferences(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	h, _, calls := providerCapacityFixture(t, 5, 0, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, responseBody("openai"))
	})
	var rejected atomic.Int64
	var wg sync.WaitGroup
	results := make(chan int, 33)
	for i := range 30 {
		wg.Go(func() {
			out := providerCapacityRequest(h, "openai", fmt.Sprint(i), "glm", "custom-model")
			if out.Code == 429 {
				rejected.Add(1)
			}
			results <- out.Code
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (calls.Load() != 5 || rejected.Load() != 25) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 5 || rejected.Load() != 25 {
		t.Fatalf("last-slot race: calls=%d rejected=%d", calls.Load(), rejected.Load())
	}
	// Reuse an actually admitted session, independent of goroutine ordering.
	h.providerSessionMu.Lock()
	_, active, _ := h.providerSessions["glm"].Stats()
	h.providerSessionMu.Unlock()
	if active != 5 {
		t.Fatalf("active=%d", active)
	}
	// Explicit same-session concurrency is tested separately below.
	unblock()
	wg.Wait()
	close(results)
	accepted := 0
	for status := range results {
		if status == 200 {
			accepted++
		} else if status != 429 {
			t.Fatalf("status=%d", status)
		}
	}
	if accepted != 5 {
		t.Fatalf("accepted=%d", accepted)
	}
}

func TestProviderCapacityAnonymousConcurrentAndCancellation(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			started, release := make(chan struct{}, 1), make(chan struct{})
			var first atomic.Bool
			h, _, calls := providerCapacityFixture(t, 1, 0, func(w http.ResponseWriter, r *http.Request) {
				if first.CompareAndSwap(false, true) {
					started <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				io.WriteString(w, responseBody("openai"))
			})
			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest("POST", clientEndpoint(protocol), strings.NewReader(requestBodyFor(protocol, "glm", "custom-model"))).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer local-secret")
			done := make(chan struct{})
			go func() { h.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("anonymous did not dispatch")
			}
			requireCapacityRejection(t, providerCapacityRequest(h, protocol, "", "glm", "custom-model"), "provider", 1)
			if calls.Load() != 1 {
				t.Fatal("concurrent anonymous exceeded cap")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation did not release")
			}
			close(release)
			for range 2 {
				if out := providerCapacityRequest(h, protocol, "", "glm", "custom-model"); out.Code != 200 {
					t.Fatalf("anonymous retained: %d %s", out.Code, out.Body)
				}
			}
			if _, current, _ := h.providerSessions["glm"].Stats(); current != 0 {
				t.Fatalf("anonymous current=%d", current)
			}
			if entries, _ := h.cache.Stats(); entries != 0 {
				t.Fatalf("anonymous prompt retention=%d", entries)
			}
		})
	}
}

func TestProviderCapacityReloadTracksUnlimitedAndRetainsDeletedRef(t *testing.T) {
	h, c, calls := providerCapacityFixture(t, 0, 0, nil)
	for _, id := range []string{"a", "b"} {
		if out := providerCapacityRequest(h, "openai", id, "glm", "custom-model"); out.Code != 200 {
			t.Fatal(out.Body)
		}
	}
	next := c
	next.Providers = map[string]Provider{}
	for ref, p := range c.Providers {
		next.Providers[ref] = p
	}
	p := next.Providers["glm"]
	p.MaxActiveSessions = 1
	next.Providers["glm"] = p
	view, err := h.prepareConfigView(context.Background(), next, c, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireCapacityRejection(t, providerCapacityRequest(view, "openai", "new", "glm", "custom-model"), "provider", 1)
	if out := providerCapacityRequest(view, "openai", "a", "glm", "other-model"); out.Code != 200 {
		t.Fatalf("existing denied after lowering: %d %s", out.Code, out.Body)
	}
	removed := next
	removed.Providers = map[string]Provider{"other": next.Providers["other"]}
	deleted, err := h.prepareConfigView(context.Background(), removed, next, view, nil)
	if err != nil {
		t.Fatal(err)
	}
	readded, err := h.prepareConfigView(context.Background(), next, removed, deleted, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireCapacityRejection(t, providerCapacityRequest(readded, "openai", "new", "glm", "custom-model"), "provider", 1)
	if calls.Load() != 3 {
		t.Fatalf("reload leaked dispatch=%d", calls.Load())
	}
	if h.handlerRuntime != readded.handlerRuntime {
		t.Fatal("runtime reset")
	}
}

func TestProviderCapacityTTLDoesNotExpireInFlightAndSameSessionConcurrent(t *testing.T) {
	started, release := make(chan struct{}, 4), make(chan struct{})
	h, _, calls := providerCapacityFixture(t, 1, 0, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		io.WriteString(w, responseBody("openai"))
	})
	h.config.ActiveSessionIdleTimeoutSeconds = 1
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if out := providerCapacityRequest(h, "openai", "same", "glm", "custom-model"); out.Code != 200 {
				t.Errorf("same=%d %s", out.Code, out.Body)
			}
		})
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("same session did not reuse")
		}
	}
	time.Sleep(1100 * time.Millisecond)
	requireCapacityRejection(t, providerCapacityRequest(h, "openai", "new", "glm", "custom-model"), "provider", 1)
	if calls.Load() != 2 {
		t.Fatal("idle expired in flight")
	}
	close(release)
	wg.Wait()
	// Output activity starts the idle interval after the held requests finish.
	requireCapacityRejection(t, providerCapacityRequest(h, "openai", "new", "glm", "custom-model"), "provider", 1)
	time.Sleep(1100 * time.Millisecond)
	if out := providerCapacityRequest(h, "openai", "new", "glm", "custom-model"); out.Code != 200 {
		t.Fatalf("idle slot not released: %d %s", out.Code, out.Body)
	}
}

func TestProviderCapacityFailoverKeepsPriorAuditAndBudget(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, retained := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/retained_%v", protocol, retained), func(t *testing.T) {
				h, c, targetCalls := providerCapacityFixture(t, 1, 3, func(w http.ResponseWriter, r *http.Request) {
					io.WriteString(w, responseBody(protocol))
				})
				var upstreamCalls atomic.Int64
				var exhausted atomic.Bool
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "GET" {
						io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
						return
					}
					upstreamCalls.Add(1)
					if exhausted.Load() {
						w.WriteHeader(402)
						io.WriteString(w, `{"error":{"code":"insufficient_balance","message":"synthetic"}}`)
						return
					}
					io.WriteString(w, responseBody(protocol))
				}))
				defer upstream.Close()
				p := c.Providers["other"]
				p.Protocol = protocol
				p.BaseURL = upstream.URL + "/v1"
				p.MaxActiveSessions = 1
				p.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "insufficient_balance", HTTPStatus: 402, Category: adapter.ProviderErrorInsufficientBalance}}
				p.Prices = map[string]adapter.Price{"custom-model": testPrice()}
				p.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 100, WeeklyLimit: 100, Mode: "hard", AlertThreshold: .8}
				c.Providers["other"] = p
				target := c.Providers["glm"]
				target.Protocol = protocol
				c.Providers["glm"] = target
				c.Routing = &RoutingConfig{BillingExhaustionFailover: true}
				view, err := h.prepareConfigView(context.Background(), c, h.config, h, nil)
				if err != nil {
					t.Fatal(err)
				}
				if out := providerCapacityRequest(view, protocol, "occupied", "glm", "custom-model"); out.Code != 200 {
					t.Fatal(out.Body)
				}
				kept := 0
				if retained {
					kept = 1
					if out := providerCapacityRequest(view, protocol, "retained", "other", "custom-model"); out.Code != 200 {
						t.Fatal(out.Body)
					}
				}
				exhausted.Store(true)
				out := providerCapacityRequest(view, protocol, "retained", "other", "custom-model")
				requireCapacityRejection(t, out, "provider", 1)
				if upstreamCalls.Load() != int64(kept+1) || targetCalls.Load() != 1 {
					t.Fatalf("actual source/target dispatch=%d/%d", upstreamCalls.Load(), targetCalls.Load())
				}
				var audits, charges, pending int
				if err := h.ledger.QueryRow(context.Background(), "SELECT COUNT(*) FROM request_audit").Scan(&audits); err != nil {
					t.Fatal(err)
				}
				if err := h.ledger.QueryRow(context.Background(), "SELECT COUNT(*) FROM budget_charges WHERE state='settled'").Scan(&charges); err != nil {
					t.Fatal(err)
				}
				if err := h.ledger.QueryRow(context.Background(), "SELECT COUNT(*) FROM budget_charges WHERE state='pending'").Scan(&pending); err != nil {
					t.Fatal(err)
				}
				if audits != kept+2 || charges != kept+1 || pending != 0 {
					t.Fatalf("prior settlement lost audits=%d charges=%d pending=%d", audits, charges, pending)
				}
				if _, current, _ := h.sessions.Stats(); current != kept+1 {
					t.Fatalf("refusal removed existing/global lease or leaked new lease: %d", current)
				}
				for ref, want := range map[string]int{"glm": 1, "other": kept} {
					if _, current, _ := h.providerSessions[ref].Stats(); current != want {
						t.Fatalf("refusal changed retained provider %s: %d", ref, current)
					}
				}
			})
		}
	}
}

func TestProviderCapacityReleaseOnFailureStreamDisconnectAndPanic(t *testing.T) {
	for _, scenario := range []string{"error", "stream", "disconnect", "panic"} {
		t.Run(scenario, func(t *testing.T) {
			h, _, _ := providerCapacityFixture(t, 1, 1, func(w http.ResponseWriter, r *http.Request) {
				if scenario == "error" {
					w.WriteHeader(500)
					return
				}
				if scenario == "stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
					return
				}
				io.WriteString(w, responseBody("openai"))
			})
			body := requestBodyFor("openai", "glm", "custom-model")
			if scenario == "stream" {
				body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			}
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-secret")
			req.Header.Set(adapter.SessionIDHeader, "one")
			var out http.ResponseWriter = httptest.NewRecorder()
			if scenario == "disconnect" {
				out = &disconnectedResponseWriter{header: http.Header{}, failOnWrite: true}
			}
			if scenario == "panic" {
				out = &capacityPanicWriter{header: http.Header{}}
			}
			h.ServeHTTP(out, req)
			if _, current, _ := h.providerSessions["glm"].Stats(); current != 0 {
				t.Fatalf("provider retained on %s: %d", scenario, current)
			}
			if _, current, _ := h.sessions.Stats(); current != 0 {
				t.Fatalf("global retained on %s: %d", scenario, current)
			}
		})
	}
}

type capacityPanicWriter struct{ header http.Header }

func (w *capacityPanicWriter) Header() http.Header       { return w.header }
func (w *capacityPanicWriter) WriteHeader(int)           {}
func (w *capacityPanicWriter) Write([]byte) (int, error) { panic("synthetic private panic") }

func TestProviderCapacityQueuedCancelAndShutdownRelease(t *testing.T) {
	for _, scenario := range []string{"cancel", "shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			started, release := make(chan struct{}, 1), make(chan struct{})
			defer close(release)
			h, _, calls := providerCapacityFixture(t, 2, 2, func(w http.ResponseWriter, r *http.Request) {
				started <- struct{}{}
				select {
				case <-release:
					io.WriteString(w, responseBody("openai"))
				case <-r.Context().Done():
				}
			})
			// Only the first request reaches the upstream; the second holds its
			// session lease while waiting in the existing concurrency gate.
			h.gate, _ = limiter.NewConcurrencyGate(1)
			for _, client := range h.clients {
				client.Gate = h.gate
			}
			firstCtx, firstCancel := context.WithCancel(context.Background())
			defer firstCancel()
			request := func(ctx context.Context, id string, done chan<- struct{}) {
				r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(requestBodyFor("openai", "glm", "custom-model"))).WithContext(ctx)
				r.Header.Set("Authorization", "Bearer local-secret")
				r.Header.Set(adapter.SessionIDHeader, id)
				h.ServeHTTP(httptest.NewRecorder(), r)
				done <- struct{}{}
			}
			done := make(chan struct{}, 2)
			go request(firstCtx, "first", done)
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("first did not dispatch")
			}
			queuedCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go request(queuedCtx, "queued", done)
			deadline := time.Now().Add(3 * time.Second)
			queued := false
			for time.Now().Before(deadline) {
				h.providerSessionMu.Lock()
				_, n, _ := h.providerSessions["glm"].Stats()
				h.providerSessionMu.Unlock()
				if n == 2 {
					queued = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !queued {
				t.Fatal("second request never acquired its queued session slot")
			}
			if scenario == "cancel" {
				cancel()
			} else {
				h.beginShutdown()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("queued cancellation stuck")
			}
			if calls.Load() != 1 {
				t.Fatalf("queued request dispatched %d", calls.Load())
			}
			firstCancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("active cancellation stuck")
			}
			if _, n, _ := h.providerSessions["glm"].Stats(); n != 0 {
				t.Fatalf("provider leaked %d", n)
			}
			if _, n, _ := h.sessions.Stats(); n != 0 {
				t.Fatalf("global leaked %d", n)
			}
		})
	}
}
