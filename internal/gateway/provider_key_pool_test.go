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
)

func TestProviderGroupResolvesMultipleKeychainReferences(t *testing.T) {
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "https://legacy.example/v1")
	c.Protocol, c.BaseURL, c.APIVersion = "", "", ""
	c.APIKey, c.Model, c.UpstreamID = "", "", ""
	c.UpstreamKeychain = KeychainReference{}
	c.ModelCapabilities, c.Prices = ModelCapabilities{}, nil
	c.Providers = map[string]Provider{
		"main": {
			Protocol: "openai", BaseURL: "https://provider.example/v1", Model: "model-a",
			UpstreamKeychains: []KeychainReference{
				{Service: "provider", Account: "first"},
				{Service: "provider", Account: "second"},
			},
		},
	}
	c.DefaultProviders = map[string]string{"openai": "main"}
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
		body := requestBody("openai")
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
