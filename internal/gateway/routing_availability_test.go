package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type routingAvailabilityDispatch struct {
	provider string
	model    string
}

type routingAvailabilityFixture struct {
	h        *handler
	config   Config
	upstream *httptest.Server
	mu       sync.Mutex
	calls    []routingAvailabilityDispatch
}

func newRoutingAvailabilityFixture(t *testing.T, auto bool) *routingAvailabilityFixture {
	t.Helper()
	f := &routingAvailabilityFixture{}
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"one"},{"id":"two"},{"id":"shared-model"}]}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("fixture received invalid request JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		provider := "a"
		if r.URL.Path == "/b/v1/chat/completions" {
			provider = "b"
		} else if r.URL.Path != "/a/v1/chat/completions" {
			t.Errorf("fixture received unexpected POST path: %s", r.URL.Path)
		}
		f.mu.Lock()
		f.calls = append(f.calls, routingAvailabilityDispatch{provider: provider, model: body.Model})
		f.mu.Unlock()
		_, _ = io.WriteString(w, openAIResponseForModel(body.Model))
	}))
	t.Cleanup(f.upstream.Close)
	f.config = routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&f.config, "a", f.upstream.URL+"/a", "openai", "one", "shared-model")
	addRoutingProvider(&f.config, "b", f.upstream.URL+"/b", "openai", "two", "shared-model")
	if auto {
		f.config.AutoChain = []AutoChainEntry{{Provider: "a", Model: "one"}, {Provider: "b", Model: "two"}}
	} else {
		configureRouting(&f.config, RoutingConfig{SharedModelStrategy: "random"})
	}
	h, closeHandler, err := NewHandler(f.config, f.upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	f.h = h.(*handler)
	f.h.randomIndex = func(int) int { return 0 }
	return f
}

func (f *routingAvailabilityFixture) posted() []routingAvailabilityDispatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]routingAvailabilityDispatch(nil), f.calls...)
}

func (f *routingAvailabilityFixture) expectPost(t *testing.T, h http.Handler, session, requestModel, provider, actualModel string) {
	t.Helper()
	before := len(f.posted())
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"routing availability fixture"}]}`, requestModel)
	w := affinityGatewayRequest(h, "openai", session, body)
	if w.Code != http.StatusOK {
		t.Errorf("session=%s expected successful reassignment to %s/%s, status=%d", session, provider, actualModel, w.Code)
	}
	posts := f.posted()[before:]
	want := routingAvailabilityDispatch{provider: provider, model: actualModel}
	if len(posts) != 1 || posts[0] != want {
		t.Errorf("session=%s POSTs=%+v, want exactly one %+v", session, posts, want)
	}
}

func TestAutoChainRebindsActiveSessionBeforeDispatch(t *testing.T) {
	for _, reason := range []string{"unavailable", "all_keys_cooling_down"} {
		t.Run(reason, func(t *testing.T) {
			f := newRoutingAvailabilityFixture(t, true)
			const session = "active-routing-session"
			f.expectPost(t, f.h, session, "auto", "a", "one")
			if t.Failed() {
				t.Fatal("fixture did not establish a successful active a/one binding")
			}
			block := func(provider string) {
				if reason == "unavailable" {
					f.h.unavailableProviders[provider] = fmt.Errorf("fixture provider unavailable")
				} else {
					f.h.keyPools[provider].CooldownAll(time.Minute)
				}
			}
			block("a")
			f.expectPost(t, f.h, session, "auto", "b", "two")
			f.expectPost(t, f.h, session, "auto", "b", "two")
			f.expectPost(t, f.h, "new-routing-session", "auto", "b", "two")

			block("b")
			before := len(f.posted())
			for _, id := range []string{session, "no-available-route-session"} {
				started := time.Now()
				w := affinityGatewayRequest(f.h, "openai", id, `{"model":"auto","messages":[{"role":"user","content":"no available route fixture"}]}`)
				if w.Code != http.StatusServiceUnavailable {
					t.Errorf("no available candidate: status=%d, want 503", w.Code)
				}
				if elapsed := time.Since(started); elapsed > 2*time.Second {
					t.Errorf("no available candidate was not rejected promptly: %s", elapsed)
				}
			}
			if posts := f.posted()[before:]; len(posts) != 0 {
				t.Errorf("no available candidate dispatched upstream: %+v", posts)
			}
		})
	}
}

func TestSharedModelKeepsValidBindingWhenReusedProviderRecovers(t *testing.T) {
	f := newRoutingAvailabilityFixture(t, false)
	const session = "active-shared-routing-session"
	f.expectPost(t, f.h, session, "shared-model", "a", "shared-model")
	if t.Failed() {
		t.Fatal("fixture did not establish a successful active a binding")
	}
	f.h.keyPools["a"].CooldownAll(time.Minute)
	next := copyRoutingConfig(f.config)
	b := next.Providers["b"]
	b.APIKey = "fixture-rotated-b-key"
	next.Providers["b"] = b
	view := reloadTestView(t, f.h, f.h, next, f.config, f.upstream.Client())
	if !view.reusedConnections["a"] || view.reusedConnections["b"] {
		t.Fatal("fixture did not establish only a as the reusable connection")
	}
	f.expectPost(t, view, session, "shared-model", "b", "shared-model")
	if t.Failed() {
		t.Fatal("fixture did not establish a valid b binding while a was cooling down")
	}
	pool := view.keyPools["a"]
	pool.mu.Lock()
	for i := range pool.cooldowns {
		pool.cooldowns[i] = time.Time{}
	}
	pool.mu.Unlock()
	f.expectPost(t, view, session, "shared-model", "b", "shared-model")
	// A new session may still follow the reusable-connection preference.
	f.expectPost(t, view, "new-shared-routing-session", "shared-model", "a", "shared-model")
}
