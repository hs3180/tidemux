package adapter

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func cachePrompt(text string) []byte {
	return []byte(`{"messages":[{"role":"user","content":"` + text + `"}]}`)
}

func TestPromptCacheLRUExpiryAndEstimate(t *testing.T) {
	var elapsed atomic.Int64
	now := func() time.Time { return time.Unix(0, elapsed.Load()) }
	c := newPromptCache(2, 4096, time.Minute, now)
	defer c.Close()
	rate, hit := 1.0, .1
	price := Price{Currency: "USD", Source: "test", Version: "1", InputCacheMiss: &rate, InputCacheHit: &hit, Output: &rate}
	first, usage, _ := c.LocalEstimate("openai", "m", "a", cachePrompt("first"), nil, price)
	if usage.CacheRead == nil || *usage.CacheRead != 0 {
		t.Fatal("new session got a cache hit")
	}
	c.Remember("openai", "m", "b", cachePrompt("second"))
	c.LocalEstimate("openai", "m", "a", cachePrompt("first"), nil, price)
	c.Remember("openai", "m", "c", cachePrompt("third"))
	_, usage, _ = c.LocalEstimate("openai", "m", "a", cachePrompt("first"), nil, price)
	if *usage.CacheRead == 0 {
		t.Fatal("recent entry was evicted")
	}
	_, usage, _ = c.LocalEstimate("openai", "m", "b", cachePrompt("second"), nil, price)
	if *usage.CacheRead != 0 {
		t.Fatal("least recent entry survived eviction")
	}
	elapsed.Store(int64(time.Minute))
	last, usage, _ := c.LocalEstimate("openai", "m", "a", cachePrompt("first"), nil, price)
	if *usage.CacheRead != 0 || first == nil || last == nil || *first != *last {
		t.Fatal("expiry changed cache-miss price semantics")
	}
}

func TestPromptCacheBytesOversizeAndClose(t *testing.T) {
	c := newPromptCache(100, 256, time.Minute, time.Now)
	defer c.Close()
	for i := 0; i < 100; i++ {
		c.Remember("openai", "m", fmt.Sprint(i), cachePrompt(strings.Repeat("a", 80)))
	}
	entries, size := c.Stats()
	if entries == 0 || entries > 2 || size > 256 {
		t.Fatalf("byte eviction: %d/%d", entries, size)
	}
	c.Remember("openai", "m", "large", cachePrompt(strings.Repeat("x", 1024)))
	if nextEntries, nextSize := c.Stats(); entries != nextEntries || size != nextSize {
		t.Fatal("oversized entry evicted unrelated prefixes")
	}
	c.Close()
	c.Remember("openai", "m", "after-close", cachePrompt("hello"))
	if entries, size := c.Stats(); entries != 0 || size != 0 {
		t.Fatal("closed cache retained prompts")
	}
}

func TestPromptCacheQuietExpiryReleasesMemory(t *testing.T) {
	c := newPromptCache(10, 4096, 40*time.Millisecond, time.Now)
	defer c.Close()
	c.Remember("openai", "m", "ended-session", cachePrompt("temporary"))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		// Inspect actual retained state without access-triggered expiry.
		c.mu.Lock()
		empty := len(c.prompts) == 0 && c.bytes == 0
		c.mu.Unlock()
		if empty {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("quiet expired session retained prompt memory")
}

func TestPromptCacheConcurrentChurn(t *testing.T) {
	c := newPromptCache(8, 4096, time.Minute, time.Now)
	defer c.Close()
	var wg sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				id := fmt.Sprintf("%d-%d", worker, i)
				c.Remember("openai", "m", id, cachePrompt("remember"))
				c.LocalEstimate("openai", "m", id, cachePrompt("estimate"), nil, Price{})
			}
		}()
	}
	wg.Wait()
	if entries, size := c.Stats(); entries > 8 || size > 4096 {
		t.Fatalf("concurrent bound exceeded: %d/%d", entries, size)
	}
}
