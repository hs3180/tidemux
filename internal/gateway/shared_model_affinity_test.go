package gateway

import (
	"context"
	"errors"
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

func affinityUnitHandler(t *testing.T) *handler {
	t.Helper()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", "https://a.example", "openai", "shared-model", "other-model", "vendor/model")
	addRoutingProvider(&c, "b", "https://b.example", "openai", "shared-model", "other-model", "vendor/model")
	configureRouting(&c, RoutingConfig{SharedModelStrategy: "random", BillingExhaustionFailover: true})
	return &handler{config: c, providers: c.Providers, clients: map[string]*adapter.Client{
		"a": {Protocol: "openai"}, "b": {Protocol: "openai"},
	}, keyPools: map[string]*providerKeyPool{"a": newProviderKeyPool([]string{"key-a"}), "b": newProviderKeyPool([]string{"key-b"})}}
}

func affinityRoute(t *testing.T, h *handler, model, protocol, session string, body []byte) routeStep {
	t.Helper()
	routes, code, status := h.resolveRoutes(model, protocol, body, h.sharedSessionKey(model, protocol, session))
	if code != "" || status != 0 || len(routes) == 0 {
		t.Fatalf("routes=%+v code=%q status=%d", routes, code, status)
	}
	return routes[0]
}

func TestSharedModelSessionKeySeparatesIdentityAndNormalizesWhitespace(t *testing.T) {
	base := newSharedModelSessionKey("caller", "openai", "shared-model", "session")
	if got := newSharedModelSessionKey("caller", "openai", " shared-model ", " session "); got != base {
		t.Fatal("normalized model/session did not share the same key")
	}
	for _, key := range []sharedModelSessionKey{
		newSharedModelSessionKey("other", "openai", "shared-model", "session"),
		newSharedModelSessionKey("caller", "anthropic", "shared-model", "session"),
		newSharedModelSessionKey("caller", "openai", "other-model", "session"),
		newSharedModelSessionKey("caller", "openai", "shared-model", "other-session"),
	} {
		if key == base {
			t.Fatal("distinct routing identities shared a key")
		}
	}
	if newSharedModelSessionKey("a", "bc", "d", "e") == newSharedModelSessionKey("ab", "c", "d", "e") {
		t.Fatal("ambiguous field boundaries in the composite hash")
	}
}

func TestSharedModelAffinityConcurrentFirstSelection(t *testing.T) {
	h := affinityUnitHandler(t)
	var choices atomic.Int32
	h.randomIndex = func(size int) int { return int(choices.Add(1)-1) % size }
	const count = 64
	providers := make(chan string, count)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			providers <- affinityRoute(t, h, "shared-model", "openai", "private-session", nil).provider
		}()
	}
	close(start)
	group.Wait()
	close(providers)
	for provider := range providers {
		if provider != "a" {
			t.Fatalf("concurrent session selected %q instead of a", provider)
		}
	}
	if choices.Load() != 1 || len(h.sharedAffinity.bindings) != 1 {
		t.Fatalf("choices=%d bindings=%d", choices.Load(), len(h.sharedAffinity.bindings))
	}
	if stored := fmt.Sprint(h.sharedAffinity.bindings); strings.Contains(stored, "private-session") || strings.Contains(stored, "local-secret") {
		t.Fatal("routing state retained a raw session or credential")
	}
}

func TestSharedModelAffinityRefreshesIdleTTLAndClearsOnRestart(t *testing.T) {
	h := affinityUnitHandler(t)
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	h.sharedAffinity.now = func() time.Time { return now }
	choices := 0
	h.randomIndex = func(size int) int { selected := choices % size; choices++; return selected }
	if route := affinityRoute(t, h, "shared-model", "openai", "session", nil); route.provider != "a" {
		t.Fatal(route)
	}
	now = now.Add(sharedModelSessionIdleTTL - time.Second)
	if route := affinityRoute(t, h, "shared-model", "openai", "session", nil); route.provider != "a" || choices != 1 {
		t.Fatal("active binding expired before the idle TTL")
	}
	now = now.Add(sharedModelSessionIdleTTL - time.Second)
	if route := affinityRoute(t, h, "shared-model", "openai", "session", nil); route.provider != "a" || choices != 1 {
		t.Fatal("use did not refresh the idle TTL")
	}
	now = now.Add(sharedModelSessionIdleTTL)
	if route := affinityRoute(t, h, "shared-model", "openai", "session", nil); route.provider != "b" || choices != 2 {
		t.Fatal("binding did not expire at the idle TTL")
	}
	_ = affinityRoute(t, h, "shared-model", "openai", "inactive", nil)
	now = now.Add(sharedModelSessionIdleTTL)
	_ = affinityRoute(t, h, "shared-model", "openai", "fresh", nil)
	if len(h.sharedAffinity.bindings) != 1 {
		t.Fatal("expired sessions were not swept from memory")
	}
	restarted := affinityUnitHandler(t)
	restarted.randomIndex = func(int) int { return 0 }
	if route := affinityRoute(t, restarted, "shared-model", "openai", "session", nil); route.provider != "a" {
		t.Fatal("new handler inherited a previous handler's binding")
	}
}

func TestSharedModelAffinityRebindsOnlyToEligibleAvailableProviders(t *testing.T) {
	for _, reason := range []string{"cooldown", "unavailable", "scope", "protocol features", "missing client"} {
		t.Run(reason, func(t *testing.T) {
			h := affinityUnitHandler(t)
			h.randomIndex = func(int) int { return 0 }
			if route := affinityRoute(t, h, "shared-model", "anthropic", "session", nil); route.provider != "a" {
				t.Fatal(route)
			}
			var body []byte
			switch reason {
			case "cooldown":
				h.keyPools["a"].CooldownAll(time.Minute)
			case "unavailable":
				h.unavailableProviders = map[string]error{"a": errors.New("unavailable")}
			case "scope":
				provider := h.providers["a"]
				provider.SupportedModels = []string{"other-model"}
				h.providers["a"] = provider
			case "protocol features":
				h.clients["b"].Protocol = "anthropic"
				body = []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
			case "missing client":
				delete(h.clients, "a")
			}
			route := affinityRoute(t, h, "shared-model", "anthropic", "session", body)
			if route.provider != "b" || route.sessionKey == nil {
				t.Fatalf("did not rebind before dispatch: %+v", route)
			}
		})
	}
}

func TestSharedModelAffinityKeepsRandomWithoutIDAndSeparateExplicitAutoRoutes(t *testing.T) {
	h := affinityUnitHandler(t)
	choices := 0
	h.randomIndex = func(size int) int { selected := choices % size; choices++; return selected }
	for _, want := range []string{"a", "b", "a", "b"} {
		if route := affinityRoute(t, h, "shared-model", "openai", "", nil); route.provider != want || route.sessionKey != nil {
			t.Fatalf("request-scoped random changed: %+v, want %s", route, want)
		}
	}
	if len(h.sharedAffinity.bindings) != 0 {
		t.Fatal("requests without a stable ID created bindings")
	}
	for _, protocol := range []string{"openai", "anthropic"} {
		_ = affinityRoute(t, h, "shared-model", protocol, "same-session", nil)
	}
	_ = affinityRoute(t, h, "other-model", "openai", "same-session", nil)
	_ = affinityRoute(t, h, "vendor/model", "openai", "same-session", nil)
	_ = affinityRoute(t, h, "shared-model", "openai", "other-session", nil)
	h.config.AccessToken = "another-authenticated-caller"
	_ = affinityRoute(t, h, "shared-model", "openai", "same-session", nil)
	if len(h.sharedAffinity.bindings) != 6 {
		t.Fatalf("protocol/model/session/caller identities collided: %d bindings", len(h.sharedAffinity.bindings))
	}
	h.config.AutoChain = []AutoChainEntry{{Provider: "a", Model: "shared-model"}, {Provider: "b", Model: "other-model"}}
	h.autoChain = newAutoChainState(h.config.AutoChain, defaultAutoChainSessionTTL)
	for _, model := range []string{"a/shared-model", "auto"} {
		if route := affinityRoute(t, h, model, "openai", "same-session", nil); route.provider != "a" || route.sessionKey != nil {
			t.Fatalf("explicit/auto request used shared affinity: %+v", route)
		}
	}
	if _, code, status := h.resolveRoutes("a/auto", "openai", nil, nil); code != "auto_model_must_be_unqualified" || status != http.StatusBadRequest {
		t.Fatal("provider-qualified auto request was accepted")
	}
	if len(h.sharedAffinity.bindings) != 6 {
		t.Fatal("explicit/auto routing changed shared-model bindings")
	}
}

func affinityGateway(t *testing.T, postHandler http.HandlerFunc) *handler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"}]}`)
			return
		}
		postHandler(w, r)
	}))
	t.Cleanup(upstream.Close)
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", upstream.URL+"/a", "openai", "shared-model")
	addRoutingProvider(&c, "b", upstream.URL+"/b", "openai", "shared-model")
	provider := c.Providers["a"]
	provider.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "credits_empty", HTTPStatus: 403, Category: adapter.ProviderErrorInsufficientBalance}}
	c.Providers["a"] = provider
	configureRouting(&c, RoutingConfig{SharedModelStrategy: "random", BillingExhaustionFailover: true})
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	return h.(*handler)
}

func affinityGatewayRequest(h http.Handler, protocol, session, body string) *httptest.ResponseRecorder {
	path := "/v1/chat/completions"
	if protocol == "anthropic" {
		path = "/v1/messages"
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer local-secret")
	r.Header.Set(adapter.SessionIDHeader, session)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSharedModelAffinityUsesHeaderAndAnthropicMetadataFallback(t *testing.T) {
	var sessions []string
	h := affinityGateway(t, func(w http.ResponseWriter, r *http.Request) {
		sessions = append(sessions, r.Header.Get(adapter.SessionIDHeader))
		_, _ = io.WriteString(w, openAIResponseForModel("shared-model"))
	})
	choices := 0
	h.randomIndex = func(size int) int { choices++; return (choices - 1) % size }
	body := `{"model":"shared-model","max_tokens":32,"metadata":{"user_id":" metadata-session "},"messages":[{"role":"user","content":"hello"}]}`
	for _, session := range []string{"", "metadata-session", " header-session "} {
		w := affinityGatewayRequest(h, "anthropic", session, body)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if choices != 2 || strings.Join(sessions, ",") != "metadata-session,metadata-session,header-session" {
		t.Fatalf("choices=%d forwarded=%v", choices, sessions)
	}
	w := affinityGatewayRequest(h, "anthropic", "header-session", strings.Replace(body, `"model":"shared-model"`, `"model":" shared-model "`, 1))
	if w.Code != http.StatusOK || choices != 2 {
		t.Fatalf("normalized bare model lost its binding: status=%d choices=%d", w.Code, choices)
	}
}

func TestSharedModelAffinityRechecksBeforeDispatch(t *testing.T) {
	var paths []string
	h := affinityGateway(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, openAIResponseForModel("shared-model"))
	})
	h.randomIndex = func(int) int { return 0 }
	selections := 0
	h.sharedAffinity.now = func() time.Time {
		selections++
		if selections == 2 {
			h.keyPools["a"].CooldownAll(time.Minute)
		}
		return time.Now()
	}
	w := affinityGatewayRequest(h, "openai", "session", `{"model":"shared-model","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusOK || strings.Join(paths, ",") != "/b/v1/chat/completions" {
		t.Fatalf("status=%d paths=%v body=%s", w.Code, paths, w.Body.String())
	}
}

func TestSharedModelAffinityDoesNotFailOverDispatchedRequests(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var paths []string
			h := affinityGateway(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if strings.HasPrefix(r.URL.Path, "/b/") {
					_, _ = io.WriteString(w, openAIResponseForModel("shared-model"))
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"code\":\"credits_empty\",\"status\":403}}\n\n")
					return
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"code":"credits_empty","message":"private billing detail"}}`)
			})
			h.randomIndex = func(int) int { return 0 }
			body := fmt.Sprintf(`{"model":"shared-model","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
			w := affinityGatewayRequest(h, "openai", "session", body)
			if strings.Join(paths, ",") != "/a/v1/chat/completions" || strings.Contains(w.Body.String(), "private billing detail") {
				t.Fatalf("request replayed: status=%d paths=%v body=%s", w.Code, paths, w.Body.String())
			}
			if stream && (w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "partial") || !strings.Contains(w.Body.String(), "event: error")) {
				t.Fatalf("stream failure was not surfaced: %s", w.Body.String())
			}
			if !stream && w.Code != http.StatusForbidden {
				t.Fatalf("billing error was not surfaced: status=%d body=%s", w.Code, w.Body.String())
			}
			if !stream {
				w = affinityGatewayRequest(h, "openai", "session", `{"model":"shared-model","messages":[{"role":"user","content":"next"}]}`)
				if w.Code != http.StatusOK || strings.Join(paths, ",") != "/a/v1/chat/completions,/b/v1/chat/completions" {
					t.Fatal("a later request did not rebind after billing cooldown")
				}
			}
			rows, err := h.ledger.Recent(context.Background(), 10)
			if err != nil || len(rows) != len(paths) {
				t.Fatalf("attempt audit mismatch: rows=%+v err=%v", rows, err)
			}
		})
	}
}
