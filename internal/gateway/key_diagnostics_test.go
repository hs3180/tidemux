package gateway

import (
	"context"
	"encoding/json"
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

func keyDiagnosticConfig(t *testing.T, url, protocol string, enabled bool) Config {
	t.Helper()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	c.HealthDiagnostics = &enabled
	addRoutingProvider(&c, "a", url, protocol, "one")
	p := c.Providers["a"]
	p.APIKey, p.APIKeys = "first-private-secret", []string{"first-private-secret", "second-private-secret"}
	c.Providers["a"] = p
	return c
}

func TestKeyDiagnosticsActualFailoverAndSameKeyRetries(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, rateLimit := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				t.Run(protocol+"/"+map[bool]string{true: "429", false: "auth"}[rateLimit]+"/"+map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
					var posts atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == "GET" {
							io.WriteString(w, `{"object":"list","data":[{"id":"one"}]}`)
							return
						}
						n := posts.Add(1)
						key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
						if protocol == "anthropic" {
							key = r.Header.Get("x-api-key")
						}
						if rateLimit && n < 3 {
							if key != "first-private-secret" {
								t.Error("429 switched credentials during same-key retries")
							}
							w.Header().Set("Retry-After", "0")
							w.WriteHeader(429)
							io.WriteString(w, `{"error":{"message":"private upstream explanation"}}`)
							return
						}
						if !rateLimit && key == "first-private-secret" {
							w.WriteHeader(401)
							io.WriteString(w, `{"error":{"message":"private upstream explanation"}}`)
							return
						}
						if protocol == "openai" {
							io.WriteString(w, openAIResponseForModel("one"))
						} else {
							io.WriteString(w, `{"id":"fixture","type":"message","role":"assistant","model":"one","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
						}
					}))
					defer upstream.Close()
					c := keyDiagnosticConfig(t, upstream.URL, protocol, enabled)
					h, closeHandler, err := NewHandler(c, upstream.Client())
					if err != nil {
						t.Fatal(err)
					}
					defer closeHandler()
					root := h.(*handler)
					body := `{"model":"a/one","max_tokens":16,"messages":[{"role":"user","content":"private request body"}]}`
					response := affinityGatewayRequest(h, protocol, "private-session", body)
					wantPosts := int32(2)
					if rateLimit {
						wantPosts = 3
					}
					if response.Code != 200 || posts.Load() != wantPosts {
						t.Fatalf("status=%d attempts=%d", response.Code, posts.Load())
					}
					pool := root.keyPools["a"].diagnosticStatus(time.Now())
					if pool.Capacity != 2 || pool.Cooling != 1 || pool.Eligible != 1 || pool.TelemetryEnabled != enabled {
						t.Fatalf("pool=%+v", pool)
					}
					if enabled {
						first, second := *pool.Keys[0].Counters, *pool.Keys[1].Counters
						if rateLimit {
							if first != (KeyCounters{Requests: 1, HTTPAttempts: 3, Failures: 2, Successes: 1}) || second != (KeyCounters{}) || len(pool.RecentFailovers) != 0 {
								t.Fatalf("same-key counters: %+v %+v", first, second)
							}
						} else if first != (KeyCounters{Requests: 1, HTTPAttempts: 1, Failures: 1}) || second != (KeyCounters{Requests: 1, HTTPAttempts: 1, Successes: 1}) || len(pool.RecentFailovers) != 1 || pool.RecentFailovers[0].Reason != "authentication" {
							t.Fatalf("failover counters: %+v %+v decisions=%+v", first, second, pool.RecentFailovers)
						}
					} else if pool.Keys[0].Counters != nil || pool.Keys[1].Counters != nil || len(pool.RecentFailovers) != 0 {
						t.Fatal("disabled telemetry retained counters/history")
					}
					// Local validation and a fully cooled pool must not begin HTTP attempts.
					before := posts.Load()
					invalid := affinityGatewayRequest(h, protocol, "", `{"model":"a/one","messages":[]}`)
					if invalid.Code != 400 || posts.Load() != before {
						t.Fatal("local validation initiated HTTP")
					}
					root.keyPools["a"].CooldownAll(time.Minute)
					blocked := affinityGatewayRequest(h, protocol, "", body)
					if blocked.Code != 503 || posts.Load() != before {
						t.Fatal("cooled candidates initiated HTTP")
					}
					rows, err := root.ledger.Recent(context.Background(), 10)
					if err != nil || len(rows) != 1 || rows[0].Status != "ok" {
						t.Fatalf("authoritative audits=%d err=%v", len(rows), err)
					}
					encoded, _ := json.Marshal(root.availabilityReport())
					for _, private := range []string{"first-private-secret", "second-private-secret", "private upstream explanation", "private request body", "private-session", c.LedgerPath} {
						if strings.Contains(string(encoded), private) {
							t.Fatal("query exposed private data")
						}
					}
				})
			}
		}
	}
}

func TestKeyDiagnosticsInFlightCancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{true: "timeout", false: "cancel"}[timeout], func(t *testing.T) {
			entered := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, `{"data":[{"id":"one"}]}`)
					return
				}
				io.Copy(io.Discard, r.Body)
				close(entered)
				<-r.Context().Done()
			}))
			defer upstream.Close()
			client := upstream.Client()
			if timeout {
				client.Timeout = 200 * time.Millisecond
			}
			c := keyDiagnosticConfig(t, upstream.URL, "openai", true)
			h, closeHandler, err := NewHandler(c, client)
			if err != nil {
				t.Fatal(err)
			}
			defer closeHandler()
			root := h.(*handler)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"a/one","messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer local-secret")
			done := make(chan struct{})
			go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
			<-entered
			status := root.keyPools["a"].diagnosticStatus(time.Now())
			if status.Keys[0].Counters.InFlight != 1 || status.Keys[0].Counters.HTTPAttempts != 1 {
				t.Fatal("actual in-flight attempt missing")
			}
			if !timeout {
				cancel()
			}
			<-done
			got := *root.keyPools["a"].diagnosticStatus(time.Now()).Keys[0].Counters
			want := KeyCounters{Requests: 1, HTTPAttempts: 1, Cancellations: 1}
			if timeout {
				want.Cancellations, want.Timeouts = 0, 1
			}
			if got != want {
				t.Fatalf("result=%+v want=%+v", got, want)
			}
		})
	}
}

func TestKeyDiagnosticsReloadRotationAndToggle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"list","data":[{"id":"one"}]}`)
	}))
	defer upstream.Close()
	c := keyDiagnosticConfig(t, upstream.URL, "openai", true)
	h, closeHandler, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandler()
	root := h.(*handler)
	pool := root.keyPools["a"]
	oldFinish := pool.beginAttempt(0, true, -1, "", time.Now())
	pool.CooldownAt(1, time.Hour, time.Now(), "authentication")
	before := pool.diagnosticStatus(time.Now())
	next := copyRoutingConfig(c)
	configureRouting(&next, RoutingConfig{SharedModelStrategy: "price_priority"})
	view := reloadTestView(t, root, root, next, c, upstream.Client())
	if view.keyPools["a"] != pool || view.keyPools["a"].diagnosticStatus(time.Now()).Keys[0].Label != before.Keys[0].Label {
		t.Fatal("compatible reload changed key identity")
	}
	off := false
	next.HealthDiagnostics = &off
	disabled := reloadTestView(t, root, view, next, view.config, upstream.Client())
	if disabled.keyPools["a"] != pool || pool.diagnosticStatus(time.Now()).CounterEpoch == before.CounterEpoch {
		t.Fatal("telemetry flag was not applied at publication")
	}
	on := true
	next.HealthDiagnostics = &on
	enabled := reloadTestView(t, root, disabled, next, disabled.config, upstream.Client())
	oldFinish(adapter.HTTPAttemptResult{Outcome: "success"})
	after := pool.diagnosticStatus(time.Now())
	if *after.Keys[0].Counters != (KeyCounters{}) || after.Keys[1].State != "cooling" || after.Keys[0].Label != before.Keys[0].Label {
		t.Fatal("counter reset changed routing or accepted an old completion")
	}
	oldFinish = pool.beginAttempt(0, true, -1, "", time.Now())
	rotatedConfig := copyRoutingConfig(next)
	p := rotatedConfig.Providers["a"]
	p.APIKey, p.APIKeys = "rotated-private-secret", []string{"rotated-private-secret"}
	rotatedConfig.Providers["a"] = p
	rotated := reloadTestView(t, root, enabled, rotatedConfig, next, upstream.Client())
	newPool := rotated.keyPools["a"]
	oldFinish(adapter.HTTPAttemptResult{Outcome: "failure", FailureClass: "authentication"})
	if newPool == pool || newPool.labels[0] == before.Keys[0].Label || *newPool.diagnosticStatus(time.Now()).Keys[0].Counters != (KeyCounters{}) || rotated.providerGenerations["a"] == enabled.providerGenerations["a"] {
		t.Fatal("credential rotation retained old diagnostics")
	}
	deletedConfig := copyRoutingConfig(rotatedConfig)
	deletedConfig.Providers = map[string]Provider{}
	deleted := reloadTestView(t, root, rotated, deletedConfig, rotatedConfig, upstream.Client())
	readded := reloadTestView(t, root, deleted, rotatedConfig, deletedConfig, upstream.Client())
	if readded.keyPools["a"].labels[0] == newPool.labels[0] {
		t.Fatal("removed key identity was retained after re-addition")
	}
}

func TestKeyDiagnosticsBoundedHistoryAndConcurrentResults(t *testing.T) {
	pool := newProviderKeyPool([]string{"one", "two"})
	now := time.Now()
	for n := 0; n < keyDecisionLimit*10; n++ {
		pool.beginAttempt(n%2, true, 1-n%2, "authentication", now.Add(time.Duration(n)*time.Second))(adapter.HTTPAttemptResult{Outcome: "success"})
	}
	if len(pool.diagnosticStatus(now.Add(10*time.Minute)).RecentFailovers) != keyDecisionLimit {
		t.Fatal("failover history is not bounded")
	}
	if len(pool.diagnosticStatus(now.Add(2*time.Hour)).RecentFailovers) != 0 {
		t.Fatal("idle history did not expire")
	}
	pool.setDiagnostics(false)
	pool.setDiagnostics(true)
	var group sync.WaitGroup
	for n := 0; n < 64; n++ {
		group.Go(func() { pool.beginAttempt(0, true, -1, "", now)(adapter.HTTPAttemptResult{Outcome: "success"}) })
	}
	group.Wait()
	if got := *pool.diagnosticStatus(now).Keys[0].Counters; got != (KeyCounters{Requests: 64, HTTPAttempts: 64, Successes: 64}) {
		t.Fatal(got)
	}
	// Rotation stores only configured slots; retired labels have no global registry.
	labels := map[string]bool{}
	for n := 0; n < 1_000; n++ {
		p := newProviderKeyPool([]string{"same secret"})
		if labels[p.labels[0]] || len(p.labels) != 1 || len(p.decisions) != 0 {
			t.Fatal("rotation reused labels or retained history")
		}
		labels[p.labels[0]] = true
	}
}
