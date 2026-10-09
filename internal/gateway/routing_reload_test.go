package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

func copyRoutingConfig(c Config) Config {
	next := c
	next.Providers = make(map[string]Provider, len(c.Providers))
	for ref, p := range c.Providers {
		next.Providers[ref] = p
	}
	return next
}

func reloadTestView(t *testing.T, root, previous *handler, c, previousRaw Config, client *http.Client) *handler {
	t.Helper()
	view, err := root.prepareConfigView(context.Background(), c, previousRaw, previous, client)
	if err != nil {
		t.Fatal(err)
	}
	view.activateRouting()
	return view
}

func TestReloadAutoChainSemanticBindingsAndPublication(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"one"},{"id":"two"}]}`)
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", upstream.URL, "openai", "one", "two")
	addRoutingProvider(&c, "b", upstream.URL, "openai", "one", "two")
	a, b := AutoChainEntry{Provider: "a", Model: "one"}, AutoChainEntry{Provider: "b", Model: "two"}
	c.AutoChain = []AutoChainEntry{a, b}
	h, closeHandler, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandler()
	root := h.(*handler)
	root.autoChain.selectForSession("keep-a", true)
	root.autoChain.advanceForNewSessions(0)
	root.autoChain.selectForSession("keep-b", true)
	root.autoChain.advanceForNewSessions(1)
	root.autoChain.sessions[hashAutoChainSessionID("expired")] = autoChainBinding{index: 0, lastTouch: time.Now().Add(-25 * time.Hour)}
	unchanged := reloadTestView(t, root, root, c, c, upstream.Client())
	if unchanged.autoChain != root.autoChain {
		t.Fatal("unchanged chain did not reuse its state")
	}
	if _, ok := unchanged.autoChain.selectForSession("fresh", true); ok {
		t.Fatal("unchanged reload reset exhaustion")
	}

	next := copyRoutingConfig(c)
	next.AutoChain = []AutoChainEntry{b, a}
	prepared, err := root.prepareConfigView(context.Background(), next, c, root, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	// A binding created after preparation began must be included at publication.
	root.autoChain.mu.Lock()
	root.autoChain.sessions[hashAutoChainSessionID("late")] = autoChainBinding{index: 0, lastTouch: time.Now()}
	root.autoChain.mu.Unlock()
	prepared.activateRouting()
	for session, provider := range map[string]string{"keep-a": "a", "keep-b": "b", "late": "a", "fresh": "b", "expired": "b"} {
		selected, ok := prepared.autoChain.selectForSession(session, true)
		if !ok || selected.provider != provider {
			t.Fatalf("session=%s route=%+v want=%s", session, selected, provider)
		}
	}
	if root.autoChain.entries[0] != a || root.autoChain.next != 2 {
		t.Fatal("old snapshot mutated")
	}
	root.autoChain.advanceForNewSessions(0)
	if selected, _ := prepared.autoChain.selectForSession("after-old-failure", true); selected.provider != "b" {
		t.Fatal("old failure changed new chain preference")
	}

	partial := copyRoutingConfig(next)
	partial.AutoChain = []AutoChainEntry{a}
	partial.Providers = map[string]Provider{"a": c.Providers["a"]}
	view := reloadTestView(t, root, prepared, partial, next, upstream.Client())
	if len(view.autoChain.sessions) != 2 {
		t.Fatalf("partial removal kept stale bindings: %d", len(view.autoChain.sessions))
	}
	if selected, ok := view.autoChain.selectForSession("keep-b", true); !ok || selected.provider != "a" {
		t.Fatal("removed route survived in current binding")
	}
	if selected, _ := prepared.autoChain.selectForSession("keep-b", true); selected.provider != "b" {
		t.Fatal("partial removal mutated old view")
	}

	changed := copyRoutingConfig(partial)
	p := changed.Providers["a"]
	p.APIKey = "rotated-test-key"
	changed.Providers["a"] = p
	rotated := reloadTestView(t, root, view, changed, partial, upstream.Client())
	if len(rotated.autoChain.sessions) != 3 || rotated.providerGenerations["a"] == view.providerGenerations["a"] {
		t.Fatal("credential change lost healthy bindings or reused stale cache generation")
	}
}

func TestReloadRulesThenValidAffinityThenReusableConnections(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"}]}`)
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", upstream.URL, "openai", "shared-model")
	addRoutingProvider(&c, "b", upstream.URL, "openai", "shared-model")
	configureRouting(&c, RoutingConfig{SharedModelStrategy: "random"})
	h, closeHandler, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandler()
	root := h.(*handler)
	root.randomIndex = func(int) int { return 1 }
	key := root.sharedSessionKey("shared-model", "openai", "session")
	resolve := func(view *handler, model string, key *sharedModelSessionKey) string {
		t.Helper()
		routes, code, _ := view.resolveRoutes(model, "openai", nil, key)
		if code != "" || len(routes) != 1 {
			t.Fatalf("model=%s routes=%+v code=%s", model, routes, code)
		}
		return routes[0].provider
	}
	if resolve(root, "shared-model", key) != "b" {
		t.Fatal("initial affinity not seeded")
	}
	next := copyRoutingConfig(c)
	p := next.Providers["b"]
	p.APIKey = "changed-test-key"
	next.Providers["b"] = p
	view := reloadTestView(t, root, root, next, c, upstream.Client())
	if resolve(view, "shared-model", key) != "b" {
		t.Fatal("connection change lost valid semantic affinity")
	}
	if resolve(view, "b/shared-model", key) != "b" {
		t.Fatal("reuse overrode explicit provider rule")
	}
	// An old admitted view cannot overwrite a binding created by the active view.
	for range 10 {
		resolve(root, "shared-model", key)
	}
	if view.sharedAffinity.bindings[*key].provider != "b" {
		t.Fatal("old view poisoned current affinity")
	}
	view.keyPools["b"].CooldownAt(0, time.Minute, time.Now())
	if resolve(view, "shared-model", key) != "a" {
		t.Fatal("valid affinity overrode readiness")
	}
	view.keyPools["b"].mu.Lock()
	view.keyPools["b"].cooldowns[0] = time.Now().Add(-time.Second)
	view.keyPools["b"].mu.Unlock()
	view.keyPools["a"].CooldownAt(0, time.Minute, time.Now())
	if resolve(view, "shared-model", key) != "b" {
		t.Fatal("reuse tier overrode readiness")
	}
	view.keyPools["a"].mu.Lock()
	view.keyPools["a"].cooldowns[0] = time.Now().Add(-time.Second)
	view.keyPools["a"].mu.Unlock()
	// A recovered reusable connection must not replace a valid active binding.
	if resolve(view, "shared-model", key) != "b" {
		t.Fatal("reusable connection replaced a valid affinity")
	}

	prices := copyRoutingConfig(next)
	configureRouting(&prices, RoutingConfig{SharedModelStrategy: "price_priority"})
	for name, value := range map[string]float64{"a": 10, "b": 1} {
		p := prices.Providers[name]
		price := testPrice()
		*price.InputCacheHit, *price.InputCacheMiss, *price.Output = value, value, value
		p.Prices = map[string]adapter.Price{"shared-model": price}
		prices.Providers[name] = p
	}
	priced := reloadTestView(t, root, view, prices, next, upstream.Client())
	if resolve(priced, "shared-model", key) != "b" {
		t.Fatal("reuse overrode price rule")
	}
	// Both connections are now compatible with the immediately preceding view.
	if !priced.reusedConnections["a"] || !priced.reusedConnections["b"] {
		t.Fatal("compatibility tier did not advance with the view")
	}

	scoped := copyRoutingConfig(prices)
	configureRouting(&scoped, RoutingConfig{SharedModelStrategy: "random"})
	p = scoped.Providers["a"]
	p.SupportedModels = []string{"other-model"}
	scoped.Providers["a"] = p
	limited := reloadTestView(t, root, priced, scoped, prices, upstream.Client())
	if resolve(limited, "shared-model", key) != "b" {
		t.Fatal("scope restriction lost to affinity/reuse")
	}
}

func TestReloadPreservesActualHTTPConnectionAndPolicySnapshots(t *testing.T) {
	var mu sync.Mutex
	var peers []string
	var probes int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			probes++
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"}]}`)
			return
		}
		peers = append(peers, r.RemoteAddr)
		_, _ = io.WriteString(w, openAIResponseForModel("shared-model"))
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", upstream.URL, "openai", "shared-model")
	h, closeHandler, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandler()
	root := h.(*handler)
	body := `{"model":"a/shared-model","messages":[{"role":"user","content":"hello"}]}`
	if response := affinityGatewayRequest(root, "openai", "connection", body); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	next := copyRoutingConfig(c)
	p := next.Providers["a"]
	p.Prices = map[string]adapter.Price{"shared-model": testPrice()}
	next.Providers["a"] = p
	view := reloadTestView(t, root, root, next, c, upstream.Client())
	if response := affinityGatewayRequest(view, "openai", "connection", body); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(peers) != 2 || peers[0] != peers[1] || probes != 1 {
		t.Fatalf("TCP/catalog were not preserved: peers=%v probes=%d", peers, probes)
	}
	if view.clients["a"] == root.clients["a"] || view.keyPools["a"] != root.keyPools["a"] || view.clients["a"].HTTP != root.clients["a"].HTTP || view.clients["a"].CacheNamespace != root.clients["a"].CacheNamespace || len(root.clients["a"].Prices) != 0 {
		t.Fatal("policy snapshot rebuilt incompatible transport/cache state or mutated old prices")
	}
}

func TestReloadAffinityEpochConcurrentOldReaders(t *testing.T) {
	a := &sharedModelAffinity{}
	key := newSharedModelSessionKey("caller", "openai", "model", "session")
	available := func(string) bool { return true }
	generation := func(provider string) uint64 {
		if provider == "a" {
			return 1
		}
		return 2
	}
	a.selectProviderForView(key, "model", 0, []string{"b"}, available, func(int) int { return 0 }, generation)
	a.activate(1, func(b sharedModelBinding) bool { return b.provider == "a" })
	var readers sync.WaitGroup
	for range 64 {
		readers.Go(func() {
			for range 100 {
				a.selectProviderForView(key, "model", 0, []string{"b"}, available, func(int) int { return 0 }, generation)
			}
		})
	}
	for range 100 {
		provider, ok := a.selectProviderForView(key, "model", 1, []string{"a"}, available, func(int) int { return 0 }, generation)
		if !ok || provider != "a" {
			t.Fatal("active route changed")
		}
	}
	readers.Wait()
	if b := a.bindings[key]; b.provider != "a" || b.generation != 1 {
		t.Fatal(fmt.Sprint("stale epoch binding: ", b))
	}
}

func TestReloadChainAdditionKeepsIdentityAndOldFailureIsIndependent(t *testing.T) {
	a := AutoChainEntry{Provider: "a", Model: "one"}
	b := AutoChainEntry{Provider: "b", Model: "two"}
	c := AutoChainEntry{Provider: "c", Model: "three"}
	previous := newAutoChainState([]AutoChainEntry{a, b}, defaultAutoChainSessionTTL)
	previous.selectForSession("existing", true)
	view := previous.reconfigured([]AutoChainEntry{c, b, a}, defaultAutoChainSessionTTL, map[AutoChainEntry]bool{a: true, b: true}, true)
	previous.advanceForNewSessions(0)
	if previous.next != 1 || view.next != 0 {
		t.Fatal("old classified failure changed the new chain preference")
	}
	if selected, _ := view.selectForSession("existing", true); selected.provider != "a" || selected.index != 2 {
		t.Fatal("addition moved a valid semantic binding")
	}
	if selected, _ := view.selectForSession("new", true); selected.provider != "c" {
		t.Fatal("new entry did not become the new-session preference")
	}
}

func TestReloadLegacyConnectionReusesClientAndCatalog(t *testing.T) {
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
	}))
	defer upstream.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL+"/v1")
	c.Protocol = "openai"
	h, closeHandler, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandler()
	root := h.(*handler)
	view := reloadTestView(t, root, root, c, c, upstream.Client())
	if view.clients["legacy"] != root.clients["legacy"] || view.providerGenerations["legacy"] != root.providerGenerations["legacy"] || !view.reusedConnections["legacy"] {
		t.Fatal("unchanged legacy provider connection was rebuilt")
	}
	before := probes.Load()
	next := c
	next.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	policy := reloadTestView(t, root, view, next, c, upstream.Client())
	if probes.Load() != before || policy.keyPools["legacy"] != root.keyPools["legacy"] || policy.clients["legacy"].CacheNamespace != root.clients["legacy"].CacheNamespace {
		t.Fatal("legacy policy change reset connection/catalog state")
	}
}
