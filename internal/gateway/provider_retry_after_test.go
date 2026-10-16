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
	"testing"
	"time"
)

func TestProviderRetryAfterSeparatesPauseFromAlternateRetry(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, status := range []int{429, 503} {
			for _, headerKind := range []string{"seconds", "date", "overflow"} {
				t.Run(fmt.Sprintf("%s/%d/%s", protocol, status, headerKind), func(t *testing.T) {
					var mu sync.Mutex
					posts := map[string]int{}
					until := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
					header := "259200"
					if headerKind == "date" {
						header = until.Format(http.TimeFormat)
					} else if headerKind == "overflow" {
						header = "999999999999999999999999"
					}
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == "GET" {
							_, _ = io.WriteString(w, `{"data":[{"id":"shared-model"},{"id":"one"}]}`)
							return
						}
						ref := strings.Split(r.URL.Path, "/")[1]
						mu.Lock()
						posts[ref]++
						mu.Unlock()
						if ref == "a" {
							w.Header().Set("Retry-After", header)
							w.WriteHeader(status)
							_, _ = io.WriteString(w, `{"error":{"message":"Bearer PRIVATE_PROVIDER_TEXT"}}`)
							return
						}
						var body struct{ Model string }
						_ = json.NewDecoder(r.Body).Decode(&body)
						_, _ = io.WriteString(w, openAIResponseForModel(body.Model))
					}))
					defer upstream.Close()
					c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
					for _, ref := range []string{"a", "b"} {
						addRoutingProvider(&c, ref, upstream.URL+"/"+ref, "openai", "shared-model", "one")
					}
					p := c.Providers["a"]
					p.APIKeys = []string{p.APIKey, p.APIKey + "-peer"}
					c.Providers["a"] = p
					configureRouting(&c, RoutingConfig{SharedModelStrategy: "random"})
					gateway, closeHandler, err := NewHandler(c, upstream.Client())
					if err != nil {
						t.Fatal(err)
					}
					defer closeHandler()
					h := gateway.(*handler)
					h.randomIndex = func(int) int { return 0 }
					request := func(model string) *httptest.ResponseRecorder {
						return affinityGatewayRequest(h, protocol, "session", fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`, model))
					}
					first := request("shared-model")
					var payload retryErrorEnvelope
					if err := json.Unmarshal(first.Body.Bytes(), &payload); err != nil {
						t.Fatal(err)
					}
					advice := payload.Error.Retry
					clientStatus := status
					if status == 503 {
						clientStatus = 502 // preserve the unmapped upstream 5xx contract
					}
					if first.Code != clientStatus || first.Header().Get("Retry-After") != "2" || advice.AfterSeconds == nil || *advice.AfterSeconds != 2 || !advice.ReselectProvider || len(advice.Alternatives) != 1 || advice.Alternatives[0] != "b/shared-model" || advice.ProviderRetryAt.Before(until) || strings.Contains(first.Body.String(), "PRIVATE_PROVIDER_TEXT") {
						t.Fatalf("upstream wait leaked into agent wait or alternate lost: status=%d header=%s advice=%+v", first.Code, first.Header().Get("Retry-After"), advice)
					}
					pool := h.keyPools["a"]
					if keys, _ := pool.CandidatesAt(time.Now().Add(25 * time.Hour)); len(keys) != 0 {
						t.Fatal("provider became dispatchable before a >24h server deadline")
					}
					if second := request("shared-model"); second.Code != 200 {
						t.Fatalf("new request did not recover on B: %d", second.Code)
					}
					// The same provider's other model and healthy peer key also wait.
					explicit := request("a/one")
					explicitAdvice := decodeRetryError(t, explicit).Error.Retry
					if explicitAdvice.ReselectProvider || !explicitAdvice.AlternativesRequireModelChange || explicit.Header().Get("Retry-After") != "2" {
						t.Fatal("explicit provider choice lost manual recovery guidance")
					}
					h.keyPools["b"].holdProviderUntil(time.Now().Add(2 * time.Hour))
					allBlocked := request("shared-model")
					allAdvice := decodeRetryError(t, allBlocked).Error.Retry
					if allBlocked.Header().Get("Retry-After") != "30" || allAdvice.EligibleAfterSeconds == nil || *allAdvice.EligibleAfterSeconds < 7199 || len(allAdvice.Alternatives) != 0 {
						t.Fatal("all-blocked cadence hid readiness or retained a long client wait")
					}
					mu.Lock()
					defer mu.Unlock()
					rows, err := h.ledger.Recent(context.Background(), 10)
					if posts["a"] != 1 || posts["b"] != 1 || err != nil || len(rows) != 2 {
						t.Fatalf("retry replayed or dispatched a held provider: posts=%v rows=%d err=%v", posts, len(rows), err)
					}
				})
			}
		}
	}
}

func TestProviderRetryAfterHoldSurvivesOldSuccessAndOwnsOneProbe(t *testing.T) {
	p := newProviderKeyPool([]string{"first", "peer"})
	now := time.Unix(1792000000, 0)
	_, _, old := p.candidateStateAt(1, now)
	until := now.Add(72 * time.Hour)
	p.holdProviderUntil(until)
	p.holdProviderUntil(now.Add(time.Hour))
	if p.rateSucceeded(1, old) || p.diagnosticStatus(now).ProviderRetryAt != until {
		t.Fatal("late success or shorter concurrent wait erased the hold")
	}
	for _, index := range []int{0, 1} {
		if ready, _, _ := p.candidateStateAt(index, until.Add(-time.Nanosecond)); ready {
			t.Fatal("queued peer key bypassed provider pause")
		}
	}
	first, blocked := p.beginRateRecovery(until)
	if blocked != nil || first == nil {
		t.Fatal("expired server wait did not admit a probe")
	}
	if _, blocked := p.beginRateRecovery(until); blocked == nil {
		t.Fatal("concurrent caller bypassed the one-probe gate")
	}
	p.finishRateRecovery(first)
	second, blocked := p.beginRateRecovery(until)
	if blocked != nil || second == nil {
		t.Fatal("cancelled probe stranded recovery")
	}
	ready, _, current := p.candidateStateAt(0, until)
	if !ready || !p.rateSucceeded(0, current) || !p.diagnosticStatus(until).ProviderRetryAt.IsZero() {
		t.Fatal("fresh successful probe did not release the provider")
	}
	p.finishRateRecovery(second)
}

func TestProviderRetryAfterSurvivesKeyRotationOnSameEndpoint(t *testing.T) {
	for _, replaceEndpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(replaceEndpoint), func(t *testing.T) {
			f := newRoutingAvailabilityFixture(t, false)
			until := time.Now().Add(72 * time.Hour)
			f.h.keyPools["a"].holdProviderUntil(until)
			next := copyRoutingConfig(f.config)
			p := next.Providers["a"]
			p.APIKey += "-rotated"
			if replaceEndpoint {
				p.BaseURL = f.upstream.URL + "/replacement"
			}
			next.Providers["a"] = p
			view := reloadTestView(t, f.h, f.h, next, f.config, f.upstream.Client())
			pool := view.keyPools["a"]
			if pool == f.h.keyPools["a"] {
				t.Fatal("fixture did not replace credentials")
			}
			status := pool.diagnosticStatus(time.Now())
			if replaceEndpoint {
				if !status.ProviderRetryAt.IsZero() {
					t.Fatal("a different endpoint inherited another server's pause")
				}
			} else if !status.ProviderRetryAt.Equal(until) {
				t.Fatal("credential rotation bypassed provider pause")
			}
		})
	}
}
