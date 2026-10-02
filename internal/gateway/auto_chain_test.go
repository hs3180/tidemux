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
)

func TestAutoChainFailuresOnlyAdvanceFutureSessionsAndAttributeActualRoute(t *testing.T) {
	var requests []struct{ provider, model, session string }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"model-one"},{"id":"model-two"}]}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		provider := strings.Split(r.URL.Path, "/")[1]
		requests = append(requests, struct{ provider, model, session string }{provider, body.Model, r.Header.Get(adapter.SessionIDHeader)})
		if provider == "a" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"missing_model"}}`)
			return
		}
		_, _ = io.WriteString(w, openAIResponseForModel("provider-alias"))
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", upstream.URL+"/a", "openai", "model-one")
	addRoutingProvider(&c, "b", upstream.URL+"/b", "openai", "model-two")
	provider := c.Providers["a"]
	provider.UpstreamID = "same-vendor"
	provider.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "missing_model", HTTPStatus: 404, Category: adapter.ProviderErrorModelNotFound}}
	c.Providers["a"] = provider
	provider = c.Providers["b"]
	provider.UpstreamID = "same-vendor"
	provider.Prices = map[string]adapter.Price{"model-two": testPrice()}
	c.Providers["b"] = provider
	c.AutoChain = []AutoChainEntry{{Provider: "a", Model: "model-one"}, {Provider: "b", Model: "model-two"}}
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`
	for _, session := range []string{"existing", "existing", "new-session", "", ""} {
		w := affinityGatewayRequest(h, "openai", session, body)
		if session == "existing" {
			if w.Code != http.StatusNotFound {
				t.Fatalf("failed session replayed or changed: %d %s", w.Code, w.Body.String())
			}
		} else if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"model-two"`) {
			t.Fatalf("new session did not select the advanced preference: %d %s", w.Code, w.Body.String())
		}
	}
	if len(requests) != 5 || requests[0].provider != "a" || requests[1].provider != "a" || requests[2].provider != "b" {
		t.Fatalf("auto replayed within a request or moved an existing session: %+v", requests)
	}
	if requests[3].session == "" || requests[4].session == "" || requests[3].session == requests[4].session {
		t.Fatal("requests without IDs did not receive fresh request-scoped IDs")
	}
	state := h.(*handler)
	if len(state.autoChain.sessions) != 2 || len(state.sharedAffinity.bindings) != 0 {
		t.Fatalf("request-scoped/auto/shared state mixed: auto=%d shared=%d", len(state.autoChain.sessions), len(state.sharedAffinity.bindings))
	}
	rows, err := state.ledger.Recent(context.Background(), 10)
	if err != nil || len(rows) != 5 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.Upstream != "same-vendor" || (row.ProviderRef != "a" && row.ProviderRef != "b") || row.ProviderRef == "a" && row.Model != "model-one" || row.ProviderRef == "b" && (row.Model != "model-two" || row.Status != "ok" || row.EstimatedCost == nil || row.InputTokens == nil || *row.InputTokens != 3) {
			t.Fatalf("wrong actual provider/model usage/price attribution: %+v", row)
		}
	}
}

func TestAutoChainExpiryIsCheckedBetweenPruneSweeps(t *testing.T) {
	state := newAutoChainState([]AutoChainEntry{{Provider: "a", Model: "one"}, {Provider: "b", Model: "two"}}, defaultAutoChainSessionTTL)
	if _, ok := state.selectForSession("session", true); !ok {
		t.Fatal("initial selection failed")
	}
	state.sessions[hashAutoChainSessionID("session")] = autoChainBinding{index: 0, lastTouch: time.Now().Add(-state.ttl)}
	state.lastPrune = time.Now()
	state.advanceForNewSessions(0)
	selected, ok := state.selectForSession("session", true)
	if !ok || selected.provider != "b" {
		t.Fatalf("expired binding survived between sweeps: %+v", selected)
	}
}

func TestAutoChainSamplesTouchTimeAfterAcquiringLock(t *testing.T) {
	state := newAutoChainState([]AutoChainEntry{{Provider: "a", Model: "one"}}, defaultAutoChainSessionTTL)
	base := time.Unix(1_790_000_000, 0)
	state.now = func() time.Time { return base }
	if _, ok := state.selectForSession("session", true); !ok {
		t.Fatal("initial selection failed")
	}

	firstClockCall := make(chan struct{})
	secondClockCall := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	state.now = func() time.Time {
		switch calls.Add(1) {
		case 1:
			close(firstClockCall)
			<-releaseFirst
			return base.Add(time.Minute)
		default:
			secondClockCall <- struct{}{}
			return base.Add(2 * time.Minute)
		}
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		if _, ok := state.selectForSession("session", true); !ok {
			t.Error("first concurrent selection failed")
		}
	}()
	select {
	case <-firstClockCall:
	case <-time.After(time.Second):
		t.Fatal("first selection did not sample its touch time")
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		close(secondStarted)
		defer close(secondDone)
		if _, ok := state.selectForSession("session", true); !ok {
			t.Error("second concurrent selection failed")
		}
	}()
	<-secondStarted

	secondSampledBeforeRelease := false
	select {
	case <-secondClockCall:
		secondSampledBeforeRelease = true
	case <-time.After(time.Second):
	}
	close(releaseFirst)
	<-firstDone
	<-secondDone

	if secondSampledBeforeRelease {
		t.Fatal("second selection sampled its timestamp while the first selection held the state lock")
	}
	binding := state.sessions[hashAutoChainSessionID("session")]
	if want := base.Add(2 * time.Minute); !binding.lastTouch.Equal(want) {
		t.Fatalf("lastTouch=%s, want latest serialized touch %s", binding.lastTouch, want)
	}
}

func TestAutoChainUnavailableProviderAdvancesOnlyNewSessions(t *testing.T) {
	var attempts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts = append(attempts, r.URL.Path)
		_, _ = io.WriteString(w, openAIResponseForModel("model-two"))
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", upstream.URL+"/a", "openai", "model-one")
	addRoutingProvider(&c, "b", upstream.URL+"/b", "openai", "model-two")
	c.AutoChain = []AutoChainEntry{{Provider: "a", Model: "model-one"}, {Provider: "b", Model: "model-two"}}
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	attempts = nil // Ignore startup model discovery.
	h.(*handler).unavailableProviders["a"] = fmt.Errorf("provider unavailable")
	body := `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`
	for _, session := range []string{"existing", "existing", "new-session"} {
		w := affinityGatewayRequest(h, "openai", session, body)
		if session == "existing" && w.Code != http.StatusServiceUnavailable || session == "new-session" && w.Code != http.StatusOK {
			t.Fatalf("session=%s status=%d body=%s", session, w.Code, w.Body.String())
		}
	}
	if len(attempts) != 1 || !strings.HasPrefix(attempts[0], "/b/") {
		t.Fatalf("unavailable provider caused replay or changed an existing session: %v", attempts)
	}
}

func TestAutoChainConcurrentBindingsAndFailuresDoNotSkipPreference(t *testing.T) {
	chain := []AutoChainEntry{{Provider: "a", Model: "one"}, {Provider: "b", Model: "two"}, {Provider: "c", Model: "three"}}
	state := newAutoChainState(chain, defaultAutoChainSessionTTL)
	key := newSharedModelSessionKey("caller", "anthropic", "auto", "private-auto-session")
	var group sync.WaitGroup
	for i := 0; i < 64; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			route, _ := state.selectRoute(&key)
			if route.provider != "a" || route.model != "one" {
				t.Errorf("concurrent first binding moved: %+v", route)
			}
			state.recordFailure(*route.autoChainIndex, len(chain), &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, nil, false)
		}()
	}
	group.Wait()
	if state.next != 1 || len(state.sessions) != 1 {
		t.Fatalf("preference=%d bindings=%d", state.next, len(state.sessions))
	}
	if route, _ := state.selectRoute(&key); route.provider != "a" {
		t.Fatal("failure unpinned an existing session")
	}
	if route, _ := state.selectRoute(nil); route.provider != "b" {
		t.Fatal("new session missed the preferred entry")
	}
	state.recordFailure(1, len(chain), &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorTemporarilyUnavailable}, nil, false)
	state.recordFailure(2, len(chain), &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorInsufficientBalance}, nil, false)
	if _, ok := state.selectRoute(nil); ok {
		t.Fatal("exhausted chain accepted a new session")
	}
	if route, _ := state.selectRoute(&key); route.provider != "a" {
		t.Fatal("chain exhaustion moved an existing session")
	}
	if strings.Contains(fmt.Sprint(state.sessions), "private-auto-session") {
		t.Fatal("raw session was stored in auto state")
	}
}

func TestAutoChainAdvanceRequiresSafeClassificationBeforeOutput(t *testing.T) {
	tests := []struct {
		name            string
		callErr         *adapter.CallError
		ctxErr          error
		delivered, want bool
	}{
		{name: "missing model", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, want: true},
		{name: "temporary unavailable", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorTemporarilyUnavailable}, want: true},
		{name: "billing", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorInsufficientBalance}, want: true},
		{name: "pre-write transport", callErr: &adapter.CallError{FailoverSafe: true, UpstreamNotAttempted: true, Code: "upstream_transport_error"}, want: true},
		{name: "uncertain transport", callErr: &adapter.CallError{FailoverSafe: true, Code: "upstream_transport_error"}},
		{name: "rate limit", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorRateLimited}},
		{name: "authentication", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorAuthentication}},
		{name: "unmapped billing", callErr: &adapter.CallError{Status: 403, Code: "upstream_error"}},
		{name: "validation", callErr: &adapter.CallError{Status: 400, Code: "invalid_request", UpstreamNotAttempted: true}},
		{name: "unsafe model error", callErr: &adapter.CallError{Category: adapter.ProviderErrorModelNotFound}},
		{name: "after output", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, delivered: true},
		{name: "cancelled", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, ctxErr: context.Canceled},
		{name: "deadline", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, ctxErr: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := autoChainFailureCanAdvance(test.callErr, test.ctxErr, test.delivered); got != test.want {
				t.Fatalf("advance=%t want %t", got, test.want)
			}
		})
	}
}
