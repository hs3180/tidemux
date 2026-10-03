package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConfigWatcherAtomicViewsAndSharedRuntime(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
			return
		}
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer upstream.Close()
	directory := t.TempDir()
	config := routingTestConfig(filepath.Join(directory, "ledger.db"))
	addRoutingProvider(&config, "a", upstream.URL, "openai", "custom-model")
	config, err := config.ResolveCredentials(context.Background(), testSecrets{"test.provider": "provider-key-a", "test.gateway": "local-secret"})
	if err != nil {
		t.Fatal(err)
	}
	h, closeGateway, err := NewHandler(config, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	root := h.(*handler)
	path := filepath.Join(directory, "config.json")
	save := func(c Config) {
		data, _ := json.Marshal(c)
		temporary := path + ".new"
		if err := os.WriteFile(temporary, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, path); err != nil {
			t.Fatal(err)
		}
	}
	save(config)
	stop := WatchConfig(h, path, config, testSecrets{"test.provider": "provider-key-a", "test.gateway": "local-secret"}, upstream.Client())
	defer stop()
	wait := func(check func() bool) {
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("watcher did not apply view")
	}
	wait(func() bool { return root.reload.applicationStatus()["status"] == "applied" })
	// Keep an admitted call across swaps; shutdown must still find and cancel it.
	admitted, release, ok := root.beginCall(context.Background())
	if !ok {
		t.Fatal("admission failed")
	}
	defer release()
	root.blockBudget("a", "USD")
	root.keyPools["a"].CooldownAt(0, time.Minute, time.Now())
	var readers sync.WaitGroup
	done := make(chan struct{})
	for range 8 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				r := httptest.NewRequest("GET", "/v1/models", nil)
				r.Header.Set("Authorization", "Bearer local-secret")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Errorf("catalog status=%d", w.Code)
					return
				}
			}
		})
	}
	next := config
	next.Providers = map[string]Provider{}
	for name, p := range config.Providers {
		next.Providers[name] = p
	}
	addRoutingProvider(&next, "b", upstream.URL, "openai", "custom-model")
	p := next.Providers["b"]
	p.APIKey = "provider-key-a"
	next.Providers["b"] = p
	save(next)
	wait(func() bool { return len(root.reload.active.Load().providers) == 2 })
	view := root.reload.active.Load()
	if view.handlerRuntime != root.handlerRuntime || view.ledger != root.ledger || view.sessions != root.sessions || view.clients["a"] != root.clients["a"] || view.keyPools["a"] != root.keyPools["a"] || !view.isBudgetBlocked("a", "USD") {
		t.Fatal("swap reset shared runtime or unaffected provider state")
	}
	if admitted.Err() != nil {
		t.Fatal("swap canceled admitted request")
	}
	next.Providers = nil
	save(next)
	wait(func() bool { return len(root.reload.active.Load().providers) == 0 })
	if len(view.providers) != 2 {
		t.Fatal("old view mutated")
	}
	// A process-wide limit cannot silently reset while old requests hold slots.
	next.MaxInFlight++
	save(next)
	wait(func() bool { return root.reload.applicationStatus()["error_code"] == "config_requires_restart" })
	if len(root.reload.active.Load().providers) != 0 {
		t.Fatal("invalid view applied")
	}
	close(done)
	readers.Wait()
	root.beginShutdown()
	if admitted.Err() == nil {
		t.Fatal("shutdown lost an old-view admitted call")
	}
}

func TestConfigPreparationDeadlineKeepsOldView(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", fast.URL, "openai", "custom-model")
	h, closeGateway, err := NewHandler(c, fast.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	root := h.(*handler)
	changed := c
	changed.Providers = map[string]Provider{}
	p := c.Providers["a"]
	p.BaseURL = slow.URL + "/v1"
	changed.Providers["a"] = p
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	view, err := root.prepareConfigView(ctx, changed, c, root, fast.Client())
	if err == nil || view != nil || time.Since(start) > time.Second {
		t.Fatal("failed probe escaped deadline or produced a view")
	}
	if root.providers["a"].BaseURL != c.Providers["a"].BaseURL {
		t.Fatal("failed preparation changed active provider")
	}
}

func TestExplicitProviderWithoutModelEndpointStillReloads(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", upstream.URL, "openai", "custom-model")
	h, closeGateway, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	root := h.(*handler)
	for _, scenario := range []string{"scope", "addition"} {
		t.Run(scenario, func(t *testing.T) {
			next := c
			next.Providers = map[string]Provider{}
			for name, p := range c.Providers {
				next.Providers[name] = p
			}
			if scenario == "scope" {
				p := next.Providers["a"]
				p.SupportedModels = []string{"new-model"}
				next.Providers["a"] = p
			} else {
				addRoutingProvider(&next, "b", upstream.URL, "openai", "new-model")
			}
			view, err := root.prepareConfigView(context.Background(), next, c, root, upstream.Client())
			if err != nil {
				t.Fatalf("explicit protocol/scoped provider with unsupported model discovery could not reload: %v", err)
			}
			name := "a"
			if scenario == "addition" {
				name = "b"
			}
			if models, _ := view.modelsForProvider(name); len(models) != 1 || models[0] != "new-model" {
				t.Fatal("active scope/catalog mismatch")
			}
		})
	}
}
