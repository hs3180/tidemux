package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/diagnostics"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/observability"
)

type thresholdCalls struct {
	a, b  atomic.Int32
	failA atomic.Bool
}

func thresholdTestGateway(t *testing.T, clientProtocol, providerProtocol, strategy string) (*handler, *thresholdCalls, *bytes.Buffer) {
	t.Helper()
	calls := &thresholdCalls{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"one"},{"id":"two"}]}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/a/") {
			calls.a.Add(1)
			if calls.failA.Load() {
				w.WriteHeader(503)
				io.WriteString(w, `{"error":{"type":"server_error","message":"temporary provider failure"}}`)
				return
			}
		} else {
			calls.b.Add(1)
		}
		var body struct{ Model string }
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, strings.ReplaceAll(responseBody(providerProtocol), "custom-model", body.Model))
	}))
	t.Cleanup(up.Close)
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	// Deliberately exercise the omitted production default, unlike legacy
	// recovery fixtures that explicitly choose an immediate switch.
	c.Routing = nil
	for i, ref := range []string{"a", "b"} {
		addRoutingProvider(&c, ref, up.URL+"/"+ref, providerProtocol, "one", "two")
		p := c.Providers[ref]
		price := float64(i + 1)
		p.Prices = map[string]adapter.Price{"one": {Currency: "USD", Source: "fixture", Version: "1", InputCacheHit: &price, InputCacheMiss: &price, Output: &price}}
		c.Providers[ref] = p
	}
	if strategy == "auto" {
		c.AutoChain = []AutoChainEntry{{Provider: "a", Model: "one"}, {Provider: "b", Model: "one"}}
	} else {
		configureRouting(&c, RoutingConfig{SharedModelStrategy: strategy})
	}
	var logs bytes.Buffer
	raw, closeDB, err := NewHandlerWithLogger(c, up.Client(), observability.JSONLogger(&logs))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDB() })
	h := raw.(*handler)
	h.randomIndex = func(int) int { return 0 }
	return h, calls, &logs
}

func thresholdRequest(h *handler, protocol, strategy, session string) *httptest.ResponseRecorder {
	model := "one"
	if strategy == "auto" {
		model = "auto"
	}
	return affinityGatewayRequest(h, protocol, session, fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`, model))
}

func TestCooldownThresholdRetainsShortBindingsAndSwitchesLongOnes(t *testing.T) {
	for _, cp := range []string{"openai", "anthropic"} {
		for _, pp := range []string{"openai", "anthropic"} {
			for _, strategy := range []string{"random", "price_priority", "auto"} {
				for _, remaining := range []time.Duration{30 * time.Second, 90 * time.Second} {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", cp, pp, strategy, remaining), func(t *testing.T) {
						h, calls, logs := thresholdTestGateway(t, cp, pp, strategy)
						if w := thresholdRequest(h, cp, strategy, "stable"); w.Code != 200 || calls.a.Load() != 1 {
							t.Fatalf("initial route failed: %s", w.Body.String())
						}
						now := time.Now()
						h.availability.now = func() time.Time { return now }
						target := availabilityTarget{"a", "", h.providerGenerations["a"]}
						h.availability.entries[target] = &availabilityEntry{failures: 1, reason: "rate_limited", retryAt: now.Add(remaining), touched: now}
						w := thresholdRequest(h, cp, strategy, "stable")
						id := w.Header().Get("X-TideMux-Request-ID")
						var decision *diagnostics.RoutingDecision
						if remaining <= time.Minute {
							if w.Code != 429 || calls.a.Load() != 1 || calls.b.Load() != 0 || w.Header().Get("Retry-After") != "30" {
								t.Fatalf("short cooldown lost affinity: status=%d calls=%d/%d headers=%v body=%s", w.Code, calls.a.Load(), calls.b.Load(), w.Header(), w.Body.String())
							}
							rows, err := h.ledger.RecentDiagnostics(context.Background(), 10)
							if err != nil || len(rows) != 1 || rows[0].ID != id {
								t.Fatalf("local diagnostic missing: %+v %v", rows, err)
							}
							decision = rows[0].RoutingDecision
							var payload struct {
								Error struct {
									RoutingDecision *diagnostics.RoutingDecision `json:"routing_decision"`
								}
							}
							json.Unmarshal(w.Body.Bytes(), &payload)
							if !reflect.DeepEqual(payload.Error.RoutingDecision, decision) {
								t.Fatal("local client/SQLite decision differs")
							}
							if decision == nil || decision.Action != "retain" || decision.ToProvider != "a" {
								t.Fatalf("retention decision=%+v", decision)
							}
						} else {
							if w.Code != 200 || calls.a.Load() != 1 || calls.b.Load() != 1 {
								t.Fatalf("long cooldown did not switch on the new request: %d %d/%d %s", w.Code, calls.a.Load(), calls.b.Load(), w.Body.String())
							}
							rows, err := h.ledger.Recent(context.Background(), 10)
							if err != nil || len(rows) != 2 {
								t.Fatal("wrong upstream audit count")
							}
							for _, row := range rows {
								if row.ID == id {
									decision = row.RoutingDecision
									if row.ProviderRef != "b" {
										t.Fatal("audit used the wrong provider")
									}
								}
							}
							if decision == nil || decision.Action != "switch" || decision.ToProvider != "b" {
								t.Fatalf("switch decision=%+v", decision)
							}
						}
						if decision.ThresholdMS != 60000 || decision.RemainingMS == nil || *decision.RemainingMS != remaining.Milliseconds() || decision.FromProvider != "a" || decision.CooldownUntilMS != now.Add(remaining).UnixMilli() {
							t.Fatalf("decision lost exact remaining evidence: %+v", decision)
						}
						eventName := "request_terminal"
						if remaining <= time.Minute {
							eventName = "local_rejection"
						}
						event := findRuntimeEvent(decodeRuntimeEvents(t, logs.Bytes()), eventName, id)
						data, _ := json.Marshal(decision)
						var expected map[string]any
						json.Unmarshal(data, &expected)
						if event == nil || !reflect.DeepEqual(event["routing_decision"], expected) {
							t.Fatalf("JSONL differs from SQLite: %+v", event)
						}
						// Expiry permits a real recovery request, not an automatic health
						// declaration. Successful explicit probing must not migrate B's
						// established random/auto session back to A.
						now = now.Add(remaining + time.Second)
						probe := affinityGatewayRequest(h, cp, "probe", `{"model":"a/one","max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`)
						if probe.Code != 200 {
							t.Fatalf("recovery probe failed: %s", probe.Body.String())
						}
						beforeA, beforeB := calls.a.Load(), calls.b.Load()
						if next := thresholdRequest(h, cp, strategy, "stable"); next.Code != 200 {
							t.Fatal("continuation failed")
						}
						wantA := remaining <= time.Minute || strategy == "price_priority"
						if wantA && (calls.a.Load() != beforeA+1 || calls.b.Load() != beforeB) || !wantA && (calls.a.Load() != beforeA || calls.b.Load() != beforeB+1) {
							t.Fatal("recovery violated the documented switchback rule")
						}
					})
				}
			}
		}
	}
}

func TestThresholdUsesRemainingWaitOverridesAndIndependentGates(t *testing.T) {
	h, _, _ := thresholdTestGateway(t, "openai", "openai", "random")
	start := time.Now()
	now := start.Add(3550 * time.Second)
	h.availability.now = func() time.Time { return now }
	target := availabilityTarget{"a", "", h.providerGenerations["a"]}
	h.availability.entries[target] = &availabilityEntry{failures: 1, reason: "rate_limited", retryAt: start.Add(time.Hour), touched: now}
	route := routeStep{provider: "a", model: "one"}
	alternatives := []routeStep{{provider: "b", model: "one"}}
	decision, retain := h.cooldownChoice(route, alternatives)
	if !retain || *decision.RemainingMS != 50000 || decision.ThresholdMS != 60000 {
		t.Fatalf("used original rather than remaining wait: %+v", decision)
	}
	// Exactly 60 seconds retains; one nanosecond beyond switches. No rounding
	// to a client-facing integer second may change the routing comparison.
	h.availability.entries[target].retryAt = now.Add(time.Minute)
	if _, retain = h.cooldownChoice(route, alternatives); !retain {
		t.Fatal("boundary 60 seconds switched")
	}
	h.availability.entries[target].retryAt = now.Add(time.Minute + time.Nanosecond)
	if _, retain = h.cooldownChoice(route, alternatives); retain {
		t.Fatal("over-threshold remainder was rounded down")
	}
	h.config.Routing.ProviderSwitchThreshold = new(int64(120))
	if _, retain = h.cooldownChoice(route, alternatives); !retain {
		t.Fatal("global override ignored")
	}
	p := h.providers["a"]
	p.ProviderSwitchThreshold = new(int64(30))
	h.providers["a"] = p
	if _, retain = h.cooldownChoice(route, alternatives); retain {
		t.Fatal("provider override ignored")
	}
	p.ModelSwitchThresholds = map[string]int64{"one": 180}
	h.providers["a"] = p
	if _, retain = h.cooldownChoice(route, alternatives); !retain {
		t.Fatal("model override ignored")
	}
	if h.switchThreshold("a", "two") != 30 {
		t.Fatal("model override escaped its model")
	}
	// A full server wait is an independent lower bound, even when the model
	// health gate is short. It must not be reduced to client retry cadence.
	now = start
	h.config.Routing.ProviderSwitchThreshold = nil
	p.ModelSwitchThresholds = nil
	p.ProviderSwitchThreshold = nil
	h.providers["a"] = p
	h.availability.entries[target].retryAt = now.Add(time.Second)
	h.keyPools["a"].holdProviderUntil(start.Add(time.Hour))
	decision, retain = h.cooldownChoice(route, alternatives)
	if retain || *decision.RemainingMS != 3600000 {
		t.Fatalf("lost maximum independent gate: %+v", decision)
	}
}

func TestUnknownWaitAndNoCompatibleAlternativeKeepBinding(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		h, calls, _ := thresholdTestGateway(t, "openai", "openai", "random")
		thresholdRequest(h, "openai", "random", "stable")
		now := time.Now()
		h.availability.now = func() time.Time { return now }
		h.availability.entries[availabilityTarget{"a", "", h.providerGenerations["a"]}] = &availabilityEntry{failures: 1, reason: "rate_limited", retryAt: now.Add(5 * time.Minute), touched: now}
		if unknown {
			h.availability.entries[availabilityTarget{"a", "", h.providerGenerations["a"]}] = &availabilityEntry{failures: 1, reason: "rate_limited", probing: true, touched: now}
		} else {
			p := h.providers["b"]
			p.SupportedModels = []string{"two"}
			h.providers["b"] = p
		}
		w := thresholdRequest(h, "openai", "random", "stable")
		if w.Code != 429 || calls.a.Load() != 1 || calls.b.Load() != 0 {
			t.Fatalf("unknown/unavailable alternative caused a dispatch: %s", w.Body.String())
		}
		rows, err := h.ledger.RecentDiagnostics(context.Background(), 10)
		if err != nil || len(rows) != 1 || rows[0].RoutingDecision == nil {
			t.Fatal("missing decision")
		}
		d := rows[0].RoutingDecision
		if unknown {
			if d.Reason != "unknown_wait" || !d.WaitUnknown || d.RemainingMS != nil || w.Header().Get("Retry-After") != "" {
				t.Fatalf("invented a probe completion time: %+v", d)
			}
		} else if d.Reason != "no_eligible_alternative" {
			t.Fatalf("wrong reason: %+v", d)
		}
	}
}

func TestThresholdConfigurationBoundsAndRoundTrip(t *testing.T) {
	for _, seconds := range []int64{-1, 86401} {
		if (RoutingConfig{ProviderSwitchThreshold: &seconds}).Validate() == nil {
			t.Fatal("accepted invalid global threshold")
		}
		if (Provider{ProviderSwitchThreshold: &seconds}).validateSwitchThresholds() == nil {
			t.Fatal("accepted invalid provider threshold")
		}
		if (Provider{ModelSwitchThresholds: map[string]int64{"one": seconds}}).validateSwitchThresholds() == nil {
			t.Fatal("accepted invalid model threshold")
		}
	}
	for _, seconds := range []*int64{nil, new(int64(0)), new(int64(60)), new(int64(86400))} {
		original := Provider{ProviderSwitchThreshold: seconds, ModelSwitchThresholds: map[string]int64{"one": 120}}
		data, _ := json.Marshal(original)
		var restored Provider
		if err := json.Unmarshal(data, &restored); err != nil || !reflect.DeepEqual(original, restored) {
			t.Fatal("threshold did not survive configuration persistence")
		}
	}
}

func TestQueuedThresholdDecisionDoesNotReplayOrLoseAudit(t *testing.T) {
	for _, remaining := range []time.Duration{30 * time.Second, 90 * time.Second} {
		t.Run(remaining.String(), func(t *testing.T) {
			f := newRoutingAvailabilityFixture(t, false, func(c *Config) {
				c.Routing.ProviderSwitchThreshold = nil
				p := c.Providers["a"]
				p.APIKeys = []string{p.APIKey, p.APIKey + "-peer"}
				c.Providers["a"] = p
			})
			h := f.h
			if _, err := h.gate.Acquire(context.Background()); err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			release := func() { once.Do(h.gate.Release) }
			defer release()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- affinityGatewayRequest(h, "openai", "stable", `{"model":"shared-model","messages":[{"role":"user","content":"fixture"}]}`)
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				h.keyPools["a"].mu.Lock()
				selected := h.keyPools["a"].next == 1
				h.keyPools["a"].mu.Unlock()
				if selected {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("request did not queue after choosing A")
				}
				time.Sleep(time.Millisecond)
			}
			// This wait appears after route choice and before the HTTP attempt.
			h.keyPools["a"].CooldownAll(remaining)
			release()
			select {
			case w := <-done:
				rows, err := h.ledger.Recent(context.Background(), 10)
				if err != nil {
					t.Fatal(err)
				}
				if remaining <= time.Minute {
					locals, err := h.ledger.RecentDiagnostics(context.Background(), 10)
					if w.Code != 429 || len(f.posted()) != 0 || len(rows) != 0 || err != nil || len(locals) != 1 || locals[0].RoutingDecision == nil || locals[0].RoutingDecision.Action != "retain" {
						t.Fatalf("short queued wait switched or created an attempted audit: %d %+v %+v %v", w.Code, rows, locals, err)
					}
				} else if w.Code != 200 || len(f.posted()) != 1 || f.posted()[0].provider != "b" || len(rows) != 1 || rows[0].RoutingDecision == nil || rows[0].RoutingDecision.Action != "switch" {
					t.Fatalf("long queued wait did not yield exactly once: %d %+v", w.Code, rows)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("queued decision did not finish")
			}
		})
	}
}

func TestThresholdReloadPreservesCoolingBindingAndAdmittedPolicy(t *testing.T) {
	for _, strategy := range []string{"random", "auto"} {
		t.Run(strategy, func(t *testing.T) {
			f := newRoutingAvailabilityFixture(t, strategy == "auto", func(c *Config) {
				if c.Routing != nil {
					c.Routing.ProviderSwitchThreshold = nil
				}
			})
			h := f.h
			model := "shared-model"
			if strategy == "auto" {
				model = "auto"
			}
			if w := affinityGatewayRequest(h, "openai", "stable", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"fixture"}]}`, model)); w.Code != 200 {
				t.Fatal("initial request failed")
			}
			h.keyPools["a"].CooldownAll(30 * time.Second)
			next := copyRoutingConfig(h.config)
			settings := next.EffectiveRouting()
			settings.ProviderSwitchThreshold = new(int64(1))
			next.Routing = &settings
			view, err := h.prepareConfigView(context.Background(), next, h.config, h, f.upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			view.activateRouting()
			view.randomIndex = func(int) int { return 0 }
			// Preparation/publication keep the original connection/pool and the
			// bound A, so each admitted view can apply its own threshold.
			if view.keyPools["a"] != h.keyPools["a"] || view.providerGenerations["a"] != h.providerGenerations["a"] || h.switchThreshold("a", "shared-model") != 60 || view.switchThreshold("a", "shared-model") != 1 {
				t.Fatal("reload mutated admitted policy or connection identity")
			}
			// Old views cannot replace current shared affinity; their eligibility
			// remains dispatch-fenced. The new view must still observe A as the
			// source of its lazy switch instead of losing it during publication.
			w := affinityGatewayRequest(view, "openai", "stable", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"fixture"}]}`, model))
			rows, err := h.ledger.Recent(context.Background(), 10)
			var decision *diagnostics.RoutingDecision
			for _, row := range rows {
				if row.ID == w.Header().Get("X-TideMux-Request-ID") {
					decision = row.RoutingDecision
				}
			}
			if err != nil || w.Code != 200 || decision == nil || decision.FromProvider != "a" || decision.ToProvider != "b" || decision.ThresholdMS != 1000 {
				t.Fatalf("reload discarded cooling affinity: status=%d decision=%+v err=%v", w.Code, decision, err)
			}
		})
	}
}

func TestActualUpstreamFailureKeepsShortAffinityUntilNextRequest(t *testing.T) {
	for _, strategy := range []string{"random", "price_priority", "auto"} {
		t.Run(strategy, func(t *testing.T) {
			h, calls, _ := thresholdTestGateway(t, "anthropic", "anthropic", strategy)
			thresholdRequest(h, "anthropic", strategy, "stable")
			calls.failA.Store(true)
			failed := thresholdRequest(h, "anthropic", strategy, "stable")
			if failed.Code != 503 || calls.a.Load() != 2 || calls.b.Load() != 0 || failed.Header().Get("x-should-retry") != "true" || failed.Header().Get("Retry-After") != "30" {
				t.Fatalf("failed request was replayed or advertised an immediate switch: %d %v", failed.Code, failed.Header())
			}
			var envelope struct{ Error struct{ Retry *retryAdvice } }
			json.Unmarshal(failed.Body.Bytes(), &envelope)
			if envelope.Error.Retry == nil || envelope.Error.Retry.ReselectProvider {
				t.Fatal("first error advice discarded short affinity")
			}
			waiting := thresholdRequest(h, "anthropic", strategy, "stable")
			if waiting.Code != 429 || calls.a.Load() != 2 || calls.b.Load() != 0 {
				t.Fatalf("failure callback erased the binding: %s", waiting.Body.String())
			}
			rows, err := h.ledger.RecentDiagnostics(context.Background(), 10)
			if err != nil || len(rows) != 1 || rows[0].RoutingDecision == nil || rows[0].RoutingDecision.FromProvider != "a" || rows[0].RoutingDecision.Action != "retain" {
				t.Fatal("new request did not retain the actual failed binding")
			}
		})
	}
}

func TestThresholdNeverSwitchesToAnIncompatibleProviderOrBypassesAdmission(t *testing.T) {
	h, calls, _ := thresholdTestGateway(t, "anthropic", "anthropic", "random")
	thresholdRequest(h, "anthropic", "random", "stable")
	now := time.Now()
	h.availability.now = func() time.Time { return now }
	h.availability.entries[availabilityTarget{"a", "", h.providerGenerations["a"]}] = &availabilityEntry{failures: 1, reason: "rate_limited", retryAt: now.Add(time.Hour), touched: now}
	h.clients["b"].Protocol = "openai"
	body := `{"model":"one","max_tokens":16,"messages":[{"role":"user","content":"fixture"}],"tools":[{"name":"web_search","type":"web_search_20250305"}]}`
	w := affinityGatewayRequest(h, "anthropic", "stable", body)
	if w.Code != 429 || calls.a.Load() != 1 || calls.b.Load() != 0 {
		t.Fatalf("long cooldown escaped native-tool compatibility: %s", w.Body.String())
	}
	// A compatible alternative is still subject to its budget guard. A routing
	// decision must not turn that local rejection into another provider attempt.
	h.clients["b"].Protocol = "anthropic"
	p := h.providers["b"]
	p.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, Mode: "hard"}
	h.providers["b"] = p
	h.blockBudget("b", "USD")
	w = thresholdRequest(h, "anthropic", "random", "stable")
	if w.Code != 429 || !strings.Contains(w.Body.String(), "budget_usage_unknown") || calls.a.Load() != 1 || calls.b.Load() != 0 {
		t.Fatalf("switch bypassed the alternative's budget: %s", w.Body.String())
	}
}
