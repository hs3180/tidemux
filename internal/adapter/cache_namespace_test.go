package adapter

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCacheNamespaceSeparatesConnectionGenerationAndClientProtocol(t *testing.T) {
	const session = "stable-upstream-session"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get(SessionIDHeader) != session {
			t.Errorf("internal namespace leaked into upstream session: %q", r.Header.Get(SessionIDHeader))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)), Request: r}, nil
	})
	client, store := newCandidateTestClient(t, &http.Client{Transport: transport})
	defer store.Close()
	client.PromptCache = NewPromptCache()
	defer client.PromptCache.Close()
	input, hit, output := 1., .1, 2.
	client.Prices = map[string]Price{"m": {Currency: "USD", InputCacheMiss: &input, InputCacheHit: &hit, Output: &output}}
	client.CacheNamespace = "generation:1"
	current := *client
	current.CacheNamespace = "generation:2"
	body := []byte(`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	for index, step := range []struct {
		c         *Client
		protocol  string
		hit       bool
		anonymous bool
	}{
		{client, "openai", false, false}, {client, "openai", true, false},
		{&current, "openai", false, false}, {client, "openai", true, false},
		{&current, "openai", true, false}, {&current, "anthropic", false, false},
		{&current, "anthropic", true, false}, {&current, "openai", false, true},
	} {
		_, id, err := step.c.CallFrom(step.protocol, context.Background(), body, "m", nil, CallOptions{SessionID: session, RequestScopedSession: step.anonymous})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := store.Recent(context.Background(), 20)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.ID != id {
				continue
			}
			found = true
			if row.CacheReadTokens == nil || (*row.CacheReadTokens > 0) != step.hit {
				t.Fatalf("step=%d protocol=%s cacheRead=%v expectedHit=%v", index, step.protocol, row.CacheReadTokens, step.hit)
			}
		}
		if !found {
			t.Fatal("audit missing")
		}
	}
	if entries, _ := client.PromptCache.Stats(); entries != 3 {
		t.Fatalf("unexpected retained generations/protocols: %d", entries)
	}
}
