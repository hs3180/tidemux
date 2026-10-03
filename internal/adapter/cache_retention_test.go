package adapter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCompletedSessionsPromptRetentionIsBounded(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chat1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)), Request: r}, nil
	})
	client, store := newCandidateTestClient(t, &http.Client{Transport: transport})
	defer store.Close()
	cache := NewPromptCache()
	defer cache.Close()
	client.PromptCache = cache
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 64<<10) + `"}]}`)
	for i := 0; i < 600; i++ {
		if _, _, err := client.CallFrom("openai", context.Background(), body, "m", nil, CallOptions{SessionID: fmt.Sprintf("completed-session-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, retainedBytes := cache.Stats()
	if entries > 256 || retainedBytes > 16<<20 {
		t.Fatalf("completed sessions retained %d entries / %d key and prompt bytes; limit 256 / 16 MiB", entries, retainedBytes)
	}
}
