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
)

func TestLateProviderRetryAfterSurvivesPolicyReload(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
			return
		}
		if posts.Add(1) == 1 {
			close(started)
			<-release
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
			return
		}
		io.WriteString(w, responseBody("openai"))
	}))
	defer upstream.Close()
	defer once.Do(func() { close(release) })
	config := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&config, "a", upstream.URL, "openai", "custom-model")
	gateway, closeGateway, err := NewHandler(config, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	root := gateway.(*handler)
	state := &configReload{}
	root.reload = state
	state.active.Store(root)
	const body = `{"model":"a/custom-model","messages":[{"role":"user","content":"fixture"}]}`
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- routingRequest(t, root, "openai", body) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached upstream")
	}
	changed := config
	changed.Providers = map[string]Provider{"a": config.Providers["a"]}
	provider := changed.Providers["a"]
	provider.MaxActiveSessions = 3
	changed.Providers["a"] = provider
	view, err := root.prepareConfigView(context.Background(), changed, config, root, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	publishRetryTestView(state, view)
	once.Do(func() { close(release) })
	select {
	case response := <-first:
		if response.Code != 429 {
			t.Fatalf("first status=%d", response.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first response never completed")
	}
	second := routingRequest(t, root, "openai", body)
	if second.Code != 429 || posts.Load() != 1 {
		t.Fatalf("late server wait lost across policy reload: second=%d upstream_posts=%d", second.Code, posts.Load())
	}
}

func TestLateServerWaitUsesCurrentProviderIdentityAndRecoveryRoutes(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, status := range []int{429, 503} {
			for _, change := range []string{"policy", "unrelated_policy", "credentials", "endpoint", "protocol", "removed_and_readded"} {
				t.Run(fmt.Sprintf("%s/%d/%s", protocol, status, change), func(t *testing.T) {
					started, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					var mu sync.Mutex
					posts := map[string]int{}
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodGet {
							io.WriteString(w, `{"object":"list","has_more":false,"data":[{"id":"custom-model","type":"model"}]}`)
							return
						}
						ref := strings.Split(r.URL.Path, "/")[1]
						mu.Lock()
						posts[ref]++
						firstA := ref == "a" && posts[ref] == 1
						mu.Unlock()
						if firstA {
							close(started)
							<-release
							w.Header().Set("Retry-After", "3600")
							w.WriteHeader(status)
							io.WriteString(w, `{"error":{"message":"Bearer PRIVATE_PROVIDER_TEXT"}}`)
							return
						}
						upstreamProtocol := "openai"
						if strings.HasSuffix(r.URL.Path, "/messages") {
							upstreamProtocol = "anthropic"
						}
						io.WriteString(w, responseBody(upstreamProtocol))
					}))
					defer upstream.Close()
					defer once.Do(func() { close(release) })
					config := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
					for _, ref := range []string{"a", "b"} {
						addRoutingProvider(&config, ref, upstream.URL+"/"+ref, "openai", "custom-model")
					}
					configureRouting(&config, RoutingConfig{SharedModelStrategy: "random"})
					gateway, closeGateway, err := NewHandler(config, upstream.Client())
					if err != nil {
						t.Fatal(err)
					}
					defer closeGateway()
					root := gateway.(*handler)
					root.randomIndex = func(int) int { return 0 }
					state := &configReload{}
					root.reload = state
					state.active.Store(root)
					const body = `{"model":"custom-model","max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`
					first := make(chan *httptest.ResponseRecorder, 1)
					go func() { first <- routingRequest(t, root, protocol, body) }()
					select {
					case <-started:
					case <-time.After(5 * time.Second):
						t.Fatal("first request never reached A")
					}
					changed := config
					changed.Providers = map[string]Provider{"a": config.Providers["a"], "b": config.Providers["b"]}
					provider := changed.Providers["a"]
					switch change {
					case "policy":
						provider.MaxActiveSessions = 3
					case "unrelated_policy":
						peer := changed.Providers["b"]
						peer.MaxActiveSessions = 3
						changed.Providers["b"] = peer
					case "credentials":
						provider.APIKey += "-rotated"
					case "endpoint":
						provider.BaseURL = upstream.URL + "/fresh/v1"
					case "protocol":
						provider.Protocol = "anthropic"
					}
					changed.Providers["a"] = provider
					previousConfig, previousView := config, root
					if change == "removed_and_readded" {
						removed := config
						removed.Providers = map[string]Provider{"b": config.Providers["b"]}
						removedView, err := root.prepareConfigView(context.Background(), removed, config, root, upstream.Client())
						if err != nil {
							t.Fatal(err)
						}
						publishRetryTestView(state, removedView)
						previousConfig, previousView = removed, removedView
					}
					view, err := root.prepareConfigView(context.Background(), changed, previousConfig, previousView, upstream.Client())
					if err != nil {
						t.Fatal(err)
					}
					view.randomIndex = func(int) int { return 0 }
					publishRetryTestView(state, view)
					_, _, earlierSuccess := view.keyPools["a"].candidateStateAt(0, time.Now())
					once.Do(func() { close(release) })
					var response *httptest.ResponseRecorder
					select {
					case response = <-first:
					case <-time.After(5 * time.Second):
						t.Fatal("first response never completed")
					}
					wantStatus := status
					if status == 503 {
						wantStatus = 502
					}
					if response.Code != wantStatus || response.Header().Get("Retry-After") != "2" || strings.Contains(response.Body.String(), "PRIVATE_PROVIDER_TEXT") {
						t.Fatalf("stale response did not use current short recovery advice: status=%d header=%s", response.Code, response.Header().Get("Retry-After"))
					}
					paused := change != "endpoint" && change != "protocol" && change != "removed_and_readded"
					if paused {
						var payload retryErrorEnvelope
						if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
							t.Fatal(err)
						}
						advice := payload.Error.Retry
						if !advice.ReselectProvider || len(advice.Alternatives) != 1 || advice.Alternatives[0] != "b/custom-model" {
							t.Fatal("new-request recovery did not use the current available B")
						}
						if view.keyPools["a"].rateSucceeded(0, earlierSuccess) || view.keyPools["a"].CooldownWaitAt(time.Now()) < 3500*time.Second {
							t.Fatal("an earlier success erased the late server wait")
						}
						if rejected := routingRequest(t, root, protocol, strings.Replace(body, "custom-model", "a/custom-model", 1)); rejected.Code != 429 {
							t.Fatalf("explicit current A bypassed the pause: %d", rejected.Code)
						}
					} else if view.keyPools["a"].CooldownWaitAt(time.Now()) > 0 {
						t.Fatal("old service's wait poisoned its replacement")
					}
					nextBody := body
					if !paused {
						nextBody = strings.Replace(body, "custom-model", "a/custom-model", 1)
					}
					if second := routingRequest(t, root, protocol, nextBody); second.Code != 200 {
						t.Fatalf("new request did not recover: %d", second.Code)
					}
					mu.Lock()
					defer mu.Unlock()
					if paused && (posts["a"] != 1 || posts["b"] != 1) || change == "endpoint" && (posts["a"] != 1 || posts["fresh"] != 1 || posts["b"] != 0) || (change == "protocol" || change == "removed_and_readded") && (posts["a"] != 2 || posts["b"] != 0) {
						t.Fatalf("unexpected dispatches after reload: %v", posts)
					}
				})
			}
		}
	}
}

func TestPublicationCarriesServerWaitObservedDuringCredentialPreparation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
	}))
	defer upstream.Close()
	config := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&config, "a", upstream.URL, "openai", "custom-model")
	gateway, closeGateway, err := NewHandler(config, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	root := gateway.(*handler)
	state := &configReload{}
	root.reload = state
	state.active.Store(root)
	changed := config
	provider := config.Providers["a"]
	provider.APIKey += "-rotated"
	changed.Providers = map[string]Provider{"a": provider}
	view, err := root.prepareConfigView(context.Background(), changed, config, root, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	root.keyPools["a"].holdProviderUntil(until)
	publishRetryTestView(state, view)
	if view.keyPools["a"].CooldownWaitAt(time.Now()) < 3500*time.Second {
		t.Fatal("publication lost the wait observed after the credential view was prepared")
	}
}

func TestLateServerWaitAlsoProtectsAdmittedViewsAcrossKeyRotations(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
			return
		}
		close(started)
		<-release
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	defer once.Do(func() { close(release) })
	config := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&config, "a", upstream.URL, "openai", "custom-model")
	gateway, closeGateway, err := NewHandler(config, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	root := gateway.(*handler)
	state := &configReload{}
	root.reload = state
	state.active.Store(root)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- routingRequest(t, root, "openai", `{"model":"a/custom-model","messages":[{"role":"user","content":"fixture"}]}`)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached upstream")
	}
	previous := root
	pools := []*providerKeyPool{root.keyPools["a"]}
	for range 2 {
		next := config
		provider := config.Providers["a"]
		provider.APIKey += "-rotated"
		next.Providers = map[string]Provider{"a": provider}
		view, err := root.prepareConfigView(context.Background(), next, config, previous, upstream.Client())
		if err != nil {
			t.Fatal(err)
		}
		publishRetryTestView(state, view)
		config, previous = next, view
		pools = append(pools, view.keyPools["a"])
	}
	_, _, oldSuccess := pools[2].candidateStateAt(0, time.Now())
	once.Do(func() { close(release) })
	select {
	case response := <-done:
		if response.Code != 429 {
			t.Fatal(response.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("response never completed")
	}
	for i, pool := range pools {
		if ready, _ := pool.CandidateReadyAt(0, time.Now()); ready {
			t.Fatalf("admitted view %d could dispatch after the provider's late wait", i)
		}
	}
	if pools[2].rateSucceeded(0, oldSuccess) {
		t.Fatal("success from the new pool erased a wait from an earlier credential")
	}
	probeTime := time.Now().Add(2 * time.Hour)
	first, blocked := pools[0].beginRateRecovery(probeTime)
	if first == nil || blocked != nil {
		t.Fatal("expired pause did not allow one probe")
	}
	if _, blocked := pools[2].beginRateRecovery(probeTime); blocked == nil {
		t.Fatal("key rotation allowed a second concurrent provider probe")
	}
	pools[0].finishRateRecovery(first)
	second, blocked := pools[2].beginRateRecovery(probeTime)
	if second == nil || blocked != nil {
		t.Fatal("cancelled probe did not release shared recovery")
	}
	_, _, fresh := pools[2].candidateStateAt(0, probeTime)
	if !pools[2].rateSucceeded(0, fresh) || !pools[0].diagnosticStatus(probeTime).ProviderRetryAt.IsZero() {
		t.Fatal("fresh recovery did not clear the pause across admitted views")
	}
	pools[2].finishRateRecovery(second)
}

func publishRetryTestView(state *configReload, view *handler) {
	state.mu.Lock()
	defer state.mu.Unlock()
	view.reload = state
	view.activateRouting()
	state.active.Store(view)
}

func TestServerWaitExpiringInQueueAllowsOneProviderProbe(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			started, release := make(chan struct{}, 2), make(chan struct{})
			var once sync.Once
			var posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
					return
				}
				posts.Add(1)
				started <- struct{}{}
				<-release
				io.WriteString(w, responseBody("openai"))
			}))
			defer upstream.Close()
			config := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
			config.MaxInFlight = 2
			addRoutingProvider(&config, "a", upstream.URL, "openai", "custom-model")
			provider := config.Providers["a"]
			provider.APIKeys = []string{provider.APIKey, provider.APIKey + "-second", provider.APIKey + "-third"}
			config.Providers["a"] = provider
			gateway, closeGateway, err := NewHandler(config, upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeGateway()
			h := gateway.(*handler)
			for range 2 {
				if _, err := h.gate.Acquire(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			var gateOnce sync.Once
			releaseGate := func() { gateOnce.Do(func() { h.gate.Release(); h.gate.Release() }) }
			defer releaseGate()
			defer once.Do(func() { close(release) })
			done := make(chan *httptest.ResponseRecorder, 2)
			for range 2 {
				go func() {
					done <- routingRequest(t, h, protocol, `{"model":"a/custom-model","max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`)
				}()
			}
			pool := h.keyPools["a"]
			deadline := time.Now().Add(5 * time.Second)
			for {
				pool.mu.Lock()
				selected := pool.next == 2
				pool.mu.Unlock()
				if selected {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("two requests did not enter the held dispatch queue")
				}
				time.Sleep(time.Millisecond)
			}
			// Neither request needed recovery when admitted. The server wait
			// arrives and expires while both are still waiting for dispatch.
			pool.holdProviderUntil(time.Now().Add(-time.Second))
			releaseGate()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("no recovery probe reached upstream")
			}
			select {
			case response := <-done:
				if response.Code != 429 || posts.Load() != 1 {
					t.Fatalf("queued peer bypassed the recovery claim: status=%d posts=%d", response.Code, posts.Load())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("queued peer did not yield while the probe was in flight")
			}
			once.Do(func() { close(release) })
			select {
			case response := <-done:
				if response.Code != 200 || posts.Load() != 1 || !pool.diagnosticStatus(time.Now()).ProviderRetryAt.IsZero() {
					t.Fatal("successful probe did not restore the provider")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("recovery probe did not complete")
			}
		})
	}
}
