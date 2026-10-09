package gateway

import (
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
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

func TestSharedModelFailureBindingReassignsLaterRequestsAndPreservesOtherRoutes(t *testing.T) {
	const activeSession = "shared-failure-active-session"
	type post struct{ provider, model, session string }
	var mu sync.Mutex
	var posts []post
	activeAttempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"},{"id":"other-model"}]}`)
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
		provider := strings.Split(r.URL.Path, "/")[1]
		item := post{provider: provider, model: body.Model, session: r.Header.Get(adapter.SessionIDHeader)}
		mu.Lock()
		posts = append(posts, item)
		if item.session == activeSession && item.model == "shared-model" {
			activeAttempts++
		}
		missing := provider == "a" && item.session == activeSession && item.model == "shared-model" && activeAttempts == 2
		mu.Unlock()
		if missing {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"fixture_missing_model"}}`)
			return
		}
		_, _ = io.WriteString(w, openAIResponseForModel(body.Model))
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", upstream.URL+"/a", "openai", "shared-model", "other-model")
	addRoutingProvider(&c, "b", upstream.URL+"/b", "openai", "shared-model", "other-model")
	p := c.Providers["a"]
	p.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "fixture_missing_model", HTTPStatus: 404, Category: adapter.ProviderErrorModelNotFound}}
	c.Providers["a"] = p
	configureRouting(&c, RoutingConfig{SharedModelStrategy: "random", BillingExhaustionFailover: true})
	gateway, closeHandler, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandler()
	h := gateway.(*handler)
	h.randomIndex = func(int) int { return 0 }
	type auditExpectation struct{ provider, model, status, errorCode string }
	expected := make(map[string]auditExpectation)
	request := func(session, model, provider string, status int) {
		t.Helper()
		mu.Lock()
		before := len(posts)
		mu.Unlock()
		w := affinityGatewayRequest(h, "openai", session, fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"shared failure binding fixture"}]}`, model))
		if w.Code != status {
			t.Fatalf("model=%s status=%d, want %d", model, w.Code, status)
		}
		mu.Lock()
		actual := append([]post(nil), posts[before:]...)
		mu.Unlock()
		want := post{provider: provider, model: model, session: session}
		if len(actual) != 1 || actual[0] != want {
			t.Fatalf("request replayed or selected wrong route: posts=%+v, want exactly one %+v", actual, want)
		}
		id := w.Header().Get("X-TideMux-Request-ID")
		if id == "" {
			t.Fatal("dispatched request has no audit ID")
		}
		if _, exists := expected[id]; exists {
			t.Fatal("requests reused an audit ID")
		}
		item := auditExpectation{provider: provider, model: model, status: "ok"}
		if status == http.StatusNotFound {
			item.status, item.errorCode = "error", "provider_model_not_found"
			if !strings.Contains(w.Body.String(), item.errorCode) {
				t.Fatal("mapped model-not-found failure was not surfaced")
			}
		}
		expected[id] = item
	}
	request(activeSession, "shared-model", "a", 200)
	// A different session on b and this session's other model on a are valid
	// controls: neither shares the failing a/shared-model route.
	h.randomIndex = func(int) int { return 1 }
	request("other-shared-session", "shared-model", "b", 200)
	h.randomIndex = func(int) int { return 0 }
	request(activeSession, "other-model", "a", 200)
	request(activeSession, "shared-model", "a", 404)
	request(activeSession, "shared-model", "b", 200)
	request(activeSession, "shared-model", "b", 200)
	request("other-shared-session", "shared-model", "b", 200)
	request(activeSession, "other-model", "a", 200)
	mu.Lock()
	var sequence []string
	for _, item := range posts {
		if item.session == activeSession && item.model == "shared-model" {
			sequence = append(sequence, item.provider)
		}
	}
	postCount := len(posts)
	mu.Unlock()
	if !reflect.DeepEqual(sequence, []string{"a", "a", "b", "b"}) {
		t.Fatalf("active-session POST sequence=%v, want a,a,b,b", sequence)
	}
	rows, err := h.ledger.Recent(context.Background(), 20)
	if err != nil || len(rows) != len(expected) || len(rows) != postCount {
		t.Fatalf("dispatch/audit counts disagree: audits=%d expected=%d posts=%d error=%v", len(rows), len(expected), postCount, err)
	}
	for _, row := range rows {
		want, exists := expected[row.ID]
		if !exists || row.ProviderRef != want.provider || row.Upstream != want.provider || row.Model != want.model || row.Status != want.status || row.ErrorCode != want.errorCode {
			t.Fatalf("audit attribution differs for %s: provider=%s model=%s status=%s error=%s", row.ID, row.ProviderRef, row.Model, row.Status, row.ErrorCode)
		}
	}
}

func TestSharedModelFailureBindingInvalidatesOnlyClassifiedRoutes(t *testing.T) {
	for _, test := range []struct {
		name           string
		callErr        *adapter.CallError
		ctxErr         error
		delivered      bool
		wantInvalidate bool
	}{
		{name: "unmapped_403", callErr: &adapter.CallError{Status: 403, Code: "upstream_error"}},
		{name: "rate_limit", callErr: &adapter.CallError{Status: 429, FailoverSafe: true, Category: adapter.ProviderErrorRateLimited}},
		{name: "capacity", callErr: &adapter.CallError{Status: 429, Code: "active_session_limit", UpstreamNotAttempted: true}},
		{name: "canceled", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, ctxErr: context.Canceled},
		{name: "unclassified_after_output", callErr: &adapter.CallError{Status: 403, Code: "upstream_error"}, delivered: true},
		{name: "model_not_found_after_output", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, delivered: true, wantInvalidate: true},
		{name: "balance_after_output", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorInsufficientBalance}, delivered: true, wantInvalidate: true},
		{name: "temporary_unavailable_after_output", callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorTemporarilyUnavailable}, delivered: true, wantInvalidate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := &sharedModelAffinity{}
			a.activate(1, func(sharedModelBinding) bool { return true })
			key := newSharedModelSessionKey("fixture", "openai", "shared-model", "session")
			generation := func(string) uint64 { return 1 }
			if provider, ok := a.selectProviderForView(key, "shared-model", 1, []string{"a", "b"}, func(string) bool { return true }, func(int) int { return 0 }, generation); !ok || provider != "a" {
				t.Fatal("fixture did not establish a valid a binding")
			}
			before := a.bindings[key]
			a.recordFailure(1, "a", "shared-model", 1, test.callErr, test.ctxErr, test.delivered)
			if test.wantInvalidate {
				if _, exists := a.bindings[key]; exists || len(a.failedRoutes) != 1 {
					t.Fatal("classified failure after output did not invalidate only future routing")
				}
				return
			}
			if binding, exists := a.bindings[key]; !exists || binding != before || len(a.failedRoutes) != 0 {
				t.Fatal("unsafe classification or cancellation invalidated a valid binding")
			}
		})
	}
}

func TestSharedModelFailureBindingProtectionAndReconfiguration(t *testing.T) {
	for _, category := range []adapter.ProviderErrorCategory{adapter.ProviderErrorModelNotFound, adapter.ProviderErrorTemporarilyUnavailable, adapter.ProviderErrorInsufficientBalance} {
		t.Run(string(category), func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			started := now
			a := &sharedModelAffinity{now: func() time.Time { return now }}
			a.activate(1, func(sharedModelBinding) bool { return true })
			gen := uint64(1)
			generation := func(string) uint64 { return gen }
			selectProvider := func(session string, epoch uint64, want string) sharedModelSessionKey {
				t.Helper()
				key := newSharedModelSessionKey("fixture", "openai", "shared-model", session)
				provider, ok := a.selectProviderForView(key, "shared-model", epoch, []string{"a", "b"}, func(string) bool { return true }, func(int) int { return 0 }, generation)
				if !ok || provider != want {
					t.Fatalf("selected=%s available=%t, want %s", provider, ok, want)
				}
				return key
			}
			selectProvider("active", 1, "a")
			failure := &adapter.CallError{FailoverSafe: true, Category: category}
			a.recordFailure(1, "a", "shared-model", gen, failure, nil, false)
			selectProvider("active", 1, "b")
			now = started.Add(5*time.Minute - time.Nanosecond)
			selectProvider("before-protection-expiry", 1, "b")
			now = started.Add(5 * time.Minute)
			want := "a"
			if category == adapter.ProviderErrorModelNotFound {
				now = started.Add(48 * time.Hour)
				want = "b"
			}
			selectProvider("after-protection-expiry", 1, want)
			if category != adapter.ProviderErrorModelNotFound {
				selectProvider("active", 1, "b")
			}
			gen = 2
			a.activate(2, func(binding sharedModelBinding) bool { return binding.generation == gen })
			if len(a.failedRoutes) != 0 {
				t.Fatal("generation change retained an obsolete failed route")
			}
			key := selectProvider("active", 2, "a")
			a.activate(3, func(binding sharedModelBinding) bool { return binding.generation == gen })
			before := a.bindings[key]
			// The old admitted view has the same connection generation here;
			// epoch isolation must still prevent it from poisoning active state.
			a.recordFailure(2, "a", "shared-model", gen, failure, nil, false)
			if binding, exists := a.bindings[key]; !exists || binding != before || len(a.failedRoutes) != 0 {
				t.Fatal("old-epoch failure poisoned active binding or failure state")
			}
		})
	}
}

func TestSharedModelFailureBindingSurvivesPolicyReloadDuringCooldown(t *testing.T) {
	a := &sharedModelAffinity{}
	valid := func(sharedModelBinding) bool { return true }
	generation := func(string) uint64 { return 1 }
	a.activateForView(1, valid, generation, func(string) bool { return true })
	a.recordFailure(1, "a", "shared-model", 1, &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorModelNotFound}, nil, false)
	// Readiness changes cannot erase a confirmed model failure in the same
	// connection generation, even when a policy-only configuration is applied.
	a.activateForView(2, valid, generation, func(provider string) bool { return provider != "a" })
	key := newSharedModelSessionKey("fixture", "openai", "shared-model", "recovered-key")
	provider, ok := a.selectProviderForView(key, "shared-model", 2, []string{"a", "b"}, func(string) bool { return true }, func(int) int { return 0 }, generation)
	if !ok || provider != "b" {
		t.Fatal("policy reload and key recovery revived a confirmed missing model")
	}
}
