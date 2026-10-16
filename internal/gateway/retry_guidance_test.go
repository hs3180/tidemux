package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

type retryErrorEnvelope struct {
	RequestID string `json:"request_id"`
	Error     struct {
		Type, Message string
		RequestID     string `json:"request_id"`
		Retry         retryAdvice
	} `json:"error"`
}

func decodeRetryError(t *testing.T, w *httptest.ResponseRecorder) retryErrorEnvelope {
	t.Helper()
	var payload retryErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	id := w.Header().Get("X-TideMux-Request-ID")
	if w.Header().Get("request-id") != id || w.Header().Get("x-request-id") != id {
		t.Fatal("standard SDK request ID headers disagree with the gateway ID")
	}
	if w.Code != 429 || payload.Error.Type != "rate_limit_error" || id == "" || payload.RequestID != id || payload.Error.RequestID != id || !strings.Contains(payload.Error.Message, id) || payload.Error.Retry.MaxAttempts != 3 {
		t.Fatalf("cooldown lost native type, bounded guidance or visible correlation: status=%d", w.Code)
	}
	return payload
}

func TestCooldownGuidanceUsesRealEarliestRouteDeadline(t *testing.T) {
	for _, strategy := range []string{"random", "price_priority", "auto"} {
		t.Run(strategy, func(t *testing.T) {
			f := newRoutingAvailabilityFixture(t, strategy == "auto")
			h := f.h
			h.serviceVersion = "0.3.5-test"
			model := "shared-model"
			if strategy == "auto" {
				model = "auto"
			} else if strategy == "price_priority" {
				configureRouting(&h.config, RoutingConfig{SharedModelStrategy: strategy})
				for i, ref := range []string{"a", "b"} {
					provider := h.providers[ref]
					price := float64(i + 1)
					provider.Prices = map[string]adapter.Price{"shared-model": {Currency: "USD", Source: "test", Version: "1", InputCacheHit: &price, InputCacheMiss: &price, Output: &price}}
					h.providers[ref] = provider
				}
			}
			// Establish affinity before the routes cool, then test stable and
			// anonymous retries against the same real operational state.
			f.expectPost(t, h, "bound", model, "a", map[bool]string{true: "one", false: "shared-model"}[strategy == "auto"])
			now := time.Now()
			h.availability.now = func() time.Time { return now }
			h.keyPools["a"].CooldownAt(0, 1250*time.Millisecond, now)
			h.keyPools["b"].CooldownAt(0, 2250*time.Millisecond, now)
			for _, ref := range []string{"a", "b"} {
				wait := 6250 * time.Millisecond
				if ref == "b" {
					wait = 4250 * time.Millisecond
				}
				for _, m := range []string{"one", "two", "shared-model"} {
					h.availability.entries[availabilityTarget{ref, m, h.providerGenerations[ref]}] = &availabilityEntry{reason: "temporarily_unavailable", failures: 1, retryAt: now.Add(wait), touched: now}
				}
			}
			for _, protocol := range []string{"openai", "anthropic"} {
				for _, session := range []string{"", "bound"} {
					for _, target := range []struct {
						model   string
						seconds int64
					}{{model, 5}, {"a/one", 7}, {"one", 7}} {
						body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`, target.model)
						w := affinityGatewayRequest(h, protocol, session, body)
						payload := decodeRetryError(t, w)
						if w.Header().Get("Retry-After") != fmt.Sprint(target.seconds) || payload.Error.Retry.AfterSeconds == nil || *payload.Error.Retry.AfterSeconds != target.seconds || w.Header().Get("X-TideMux-Version") != "0.3.5-test" {
							t.Fatalf("target=%s lost max-gate/min-route/ceiling deadline: headers=%v advice=%+v", target.model, w.Header(), payload.Error.Retry)
						}
					}
				}
			}
			if len(f.posted()) != 1 {
				t.Fatal("cooling routes dispatched work")
			}
		})
	}
}

func TestCooldownGuidanceKeepsUnknownProbeTimeAndExplicitChoice(t *testing.T) {
	f := newRoutingAvailabilityFixture(t, false)
	h := f.h
	now := time.Now()
	h.availability.now = func() time.Time { return now }
	h.keyPools["a"].CooldownAt(0, 2*time.Second, now)
	body := `{"model":"a/shared-model","max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`
	w := affinityGatewayRequest(h, "anthropic", "session", body)
	payload := decodeRetryError(t, w)
	advice := payload.Error.Retry
	if advice.ReselectProvider || !advice.AlternativesRequireModelChange || len(advice.Alternatives) != 1 || advice.Alternatives[0] != "b/shared-model" || len(f.posted()) != 0 {
		t.Fatalf("explicit choice was changed or its manual alternative lost: %+v", advice)
	}
	// A real new bare-model request can select the healthy alternative now.
	f.expectPost(t, h, "session", "shared-model", "b", "shared-model")
	h.keyPools["a"].mu.Lock()
	h.keyPools["a"].cooldowns[0] = time.Time{}
	h.keyPools["a"].mu.Unlock()
	for _, ref := range []string{"a", "b"} {
		h.availability.entries[availabilityTarget{ref, "shared-model", h.providerGenerations[ref]}] = &availabilityEntry{reason: "temporarily_unavailable", failures: 1, retryAt: now.Add(-time.Second), touched: now}
		if _, blocked := h.availability.begin(h.routingEpoch, ref, "shared-model", h.providerGenerations[ref]); blocked != nil {
			t.Fatal(blocked)
		}
	}
	for _, model := range []string{"shared-model", "a/shared-model"} {
		w := affinityGatewayRequest(h, "anthropic", "session", strings.Replace(body, "a/shared-model", model, 1))
		payload := decodeRetryError(t, w)
		if w.Header().Get("Retry-After") != "" || payload.Error.Retry.AfterSeconds != nil || payload.Error.Retry.Strategy != "exponential_backoff_with_jitter" || payload.Error.Retry.MaxDelaySeconds != 30 {
			t.Fatal("active recovery probe invented an exact completion time")
		}
	}
	if len(f.posted()) != 1 {
		t.Fatal("parallel probe dispatched upstream")
	}
}

func TestUndispatchedCooldownRebindHasOneAudit(t *testing.T) {
	for _, pause := range []string{"key_cooldowns", "upstream_retry_after"} {
		t.Run(pause, func(t *testing.T) {

			f := newRoutingAvailabilityFixture(t, false, func(c *Config) {
				provider := c.Providers["a"]
				provider.APIKeys = []string{provider.APIKey, provider.APIKey + "-second"}
				c.Providers["a"] = provider
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
				done <- affinityGatewayRequest(h, "openai", "session", `{"model":"shared-model","messages":[{"role":"user","content":"fixture"}]}`)
			}()
			pool := h.keyPools["a"]
			deadline := time.Now().Add(5 * time.Second)
			for {
				pool.mu.Lock()
				selected := pool.next == 1
				pool.mu.Unlock()
				if selected {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("request did not select a before the held dispatch gate")
				}
				time.Sleep(time.Millisecond)
			}
			// Selection happened while the gate was held. Cool both selected keys
			// before allowing the adapter's post-queue readiness check to run.
			if pause == "upstream_retry_after" {
				pool.holdProviderUntil(time.Now().Add(time.Hour))
			} else {
				pool.CooldownAll(time.Minute)
			}
			release()
			select {
			case w := <-done:
				if w.Code != 200 {
					t.Fatalf("undispatched request did not reselect: %d", w.Code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("re-selection did not finish")
			}
			posts := f.posted()
			rows, err := h.ledger.Recent(context.Background(), 10)
			if len(posts) != 1 || posts[0].provider != "b" || err != nil || len(rows) != 1 || rows[0].ProviderRef != "b" || rows[0].Status != "ok" {
				t.Fatalf("undispatched route added work or a duplicate audit: posts=%+v rows=%+v err=%v", posts, rows, err)
			}

		})
	}
}
