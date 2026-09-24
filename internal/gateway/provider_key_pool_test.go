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
	"testing"
	"time"
)

func TestProviderGroupResolvesMultipleKeychainReferences(t *testing.T) {
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "https://legacy.example/v1")
	c.Protocol, c.BaseURL, c.APIVersion = "", "", ""
	c.APIKey, c.Model, c.UpstreamID = "", "", ""
	c.UpstreamKeychain = KeychainReference{}
	c.ModelCapabilities, c.Prices = ModelCapabilities{}, nil
	c.Providers = map[string]Provider{
		"main": {
			Protocol: "openai", BaseURL: "https://provider.example/v1",
			UpstreamKeychains: []KeychainReference{
				{Service: "provider", Account: "first"},
				{Service: "provider", Account: "second"},
			},
		},
	}
	secrets := keyedTestSecrets{
		"provider/first":       "provider-secret-one",
		"provider/second":      "provider-secret-two",
		"test.gateway/default": "gateway-secret",
	}

	resolved, err := c.ResolveCredentials(context.Background(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	got := resolved.Providers["main"].ResolvedAPIKeys()
	if len(got) != 2 || got[0] != "provider-secret-one" || got[1] != "provider-secret-two" {
		t.Fatalf("resolved keys = %#v", got)
	}
	if resolved.Providers["main"].APIKey != got[0] {
		t.Fatal("legacy APIKey compatibility value is not the first key")
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"provider-secret-one", "provider-secret-two", "gateway-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("serialized configuration leaked key material: %s", data)
		}
	}
	if !strings.Contains(string(data), `"upstream_keychains"`) || !strings.Contains(string(data), `"account":"first"`) {
		t.Fatalf("keychain references were not serialized: %s", data)
	}
	duplicateSecrets := keyedTestSecrets{
		"provider/first":       "provider-secret-one",
		"provider/second":      "provider-secret-one",
		"test.gateway/default": "gateway-secret",
	}
	if _, err := c.ResolveCredentials(context.Background(), duplicateSecrets); err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("duplicate key material should be rejected without revealing it: %v", err)
	}
}

func TestProviderKeychainReferencesRejectAmbiguousOrDuplicateRefs(t *testing.T) {
	valid := KeychainReference{Service: "provider", Account: "one"}
	for name, provider := range map[string]Provider{
		"both forms": {
			UpstreamKeychain:  valid,
			UpstreamKeychains: []KeychainReference{{Service: "provider", Account: "two"}},
		},
		"duplicate refs": {UpstreamKeychains: []KeychainReference{valid, valid}},
		"empty ref":      {UpstreamKeychains: []KeychainReference{{Service: "provider"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := provider.KeychainReferences(); err == nil {
				t.Fatal("invalid keychain references accepted")
			}
		})
	}
}

func TestProviderKeyPoolRoundRobinIsConcurrencySafe(t *testing.T) {
	pool := newProviderKeyPool([]string{"key-a", "key-b"})
	if pool.Next() != "key-a" || pool.Next() != "key-b" || pool.Next() != "key-a" {
		t.Fatal("provider key pool did not round-robin in order")
	}

	const calls = 10000
	counts := map[string]int{"key-a": 0, "key-b": 0}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := pool.Next()
			mu.Lock()
			counts[key]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if counts["key-a"]+counts["key-b"] != calls || counts["key-a"] != calls/2 || counts["key-b"] != calls/2 {
		t.Fatalf("concurrent distribution = %#v", counts)
	}
}

func TestProviderKeyPoolCooldownSkipsAndExpiresDeterministically(t *testing.T) {
	pool := newProviderKeyPool([]string{"key-a", "key-b", "key-c"})
	now := time.Unix(1_800_000_000, 0)
	pool.CooldownAt(0, 30*time.Second, now)
	pool.CooldownAt(1, 10*time.Second, now)

	candidates, retryAfter := pool.CandidatesAt(now)
	if len(candidates) != 1 || candidates[0].index != 2 || retryAfter != 0 {
		t.Fatalf("healthy candidates=%+v retryAfter=%s", candidates, retryAfter)
	}

	candidates, retryAfter = pool.CandidatesAt(now.Add(11 * time.Second))
	if len(candidates) != 2 || retryAfter != 0 {
		t.Fatalf("partially recovered candidates=%+v retryAfter=%s", candidates, retryAfter)
	}
	seen := map[int]bool{}
	for _, candidate := range candidates {
		seen[candidate.index] = true
	}
	if !seen[1] || !seen[2] || seen[0] {
		t.Fatalf("cooldown expiry candidates=%+v", candidates)
	}

	candidates, retryAfter = pool.CandidatesAt(now.Add(31 * time.Second))
	if len(candidates) != 3 || retryAfter != 0 {
		t.Fatalf("fully recovered candidates=%+v retryAfter=%s", candidates, retryAfter)
	}
}

func TestSingleKeyPoolCooldownReturnsRetryDelay(t *testing.T) {
	pool := newProviderKeyPool([]string{"only-key"})
	now := time.Unix(1_800_000_000, 0)
	pool.CooldownAt(0, 12*time.Second, now)
	candidates, retryAfter := pool.CandidatesAt(now)
	if len(candidates) != 0 || retryAfter != 12*time.Second {
		t.Fatalf("candidates=%+v retryAfter=%s", candidates, retryAfter)
	}
	candidates, retryAfter = pool.CandidatesAt(now.Add(12 * time.Second))
	if len(candidates) != 1 || candidates[0].key != "only-key" || retryAfter != 0 {
		t.Fatalf("recovered candidates=%+v retryAfter=%s", candidates, retryAfter)
	}
}

func TestProviderKeyPoolRoundRobinKeepsKeyForEachRequest(t *testing.T) {
	seen := make(chan string, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		seen <- r.Header.Get("Authorization")
		var request struct {
			Stream bool `json:"stream"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &request)
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer upstream.Close()

	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL+"/v1"), "main")
	provider := c.Providers["main"]
	provider.APIKeys = []string{"key-a", "key-b"}
	provider.APIKey = "key-a"
	c.Providers["main"] = provider
	h, closeGateway, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()

	for _, stream := range []bool{false, false, true} {
		body := requestBodyFor("openai", "main", "custom-model")
		if stream {
			body = strings.TrimSuffix(body, "}") + `,"stream":true}`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer local-secret")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if out.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
		}
	}
	want := []string{"Bearer key-a", "Bearer key-b", "Bearer key-a"}
	for i, expected := range want {
		if got := <-seen; got != expected {
			t.Fatalf("request %d used %q, want %q", i+1, got, expected)
		}
	}
}

func TestGatewayFailsOverWithinProviderKeyGroup(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		key := r.Header.Get("Authorization")
		calls = append(calls, key)
		if key == "Bearer key-a" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"private auth detail"}}`)
			return
		}
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer upstream.Close()

	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL+"/v1"), "main")
	provider := c.Providers["main"]
	provider.APIKeys = []string{"key-a", "key-b"}
	provider.APIKey = "key-a"
	c.Providers["main"] = provider
	h, closeGateway, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody("openai")))
	req.Header.Set("Authorization", "Bearer local-secret")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusOK || len(calls) != 2 || calls[0] != "Bearer key-a" || calls[1] != "Bearer key-b" {
		t.Fatalf("status=%d calls=%v body=%s", out.Code, calls, out.Body.String())
	}
	rows, err := h.(*handler).ledger.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].Status != "ok" || !hasGatewayEvent(rows[0].Events, "key_failover") {
		t.Fatalf("audits=%+v err=%v", rows, err)
	}
}

func hasGatewayEvent(events []string, target string) bool {
	for _, event := range events {
		if event == target {
			return true
		}
	}
	return false
}

func TestGatewayRejectsRequestsWhileSingleKeyIsCoolingDown(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"private auth detail"}}`)
	}))
	defer upstream.Close()

	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL+"/v1"), "main")
	provider := c.Providers["main"]
	provider.APIKeys = []string{"only-key"}
	provider.APIKey = "only-key"
	c.Providers["main"] = provider
	h, closeGateway, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()

	for i, wantStatus := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody("openai")))
		req.Header.Set("Authorization", "Bearer local-secret")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if out.Code != wantStatus {
			t.Fatalf("request %d status=%d want=%d body=%s", i+1, out.Code, wantStatus, out.Body.String())
		}
		if out.Header().Get("Retry-After") == "" {
			t.Fatalf("request %d missing Retry-After", i+1)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream was called %d times; cooling key should not be reused", calls)
	}
}
