package adapter

import (
	"strings"
	"testing"
)

func TestLocalEstimateUsesSessionPromptPrefixAsCacheHit(t *testing.T) {
	input, output, hit := 1.0, 2.0, .1
	price := Price{Currency: "USD", Source: "test", Version: "1", InputCacheMiss: &input, InputCacheHit: &hit, Output: &output}
	cache := NewPromptCache()
	first := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	second := `{"model":"m","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"world"}]}`
	firstCost, firstUsage, source := cache.LocalEstimate("openai", "m", "session", []byte(first), []byte(`{"choices":[{"message":{"content":"ok"}}]}`), price)
	if firstCost == nil || source != "local_estimated_cache_prefix" || firstUsage.CacheRead == nil || *firstUsage.CacheRead != 0 {
		t.Fatalf("first cost=%v usage=%+v source=%s", firstCost, firstUsage, source)
	}
	secondCost, secondUsage, _ := cache.LocalEstimate("openai", "m", "session", []byte(second), []byte(`{"choices":[{"message":{"content":"ok"}}]}`), price)
	if secondCost == nil || secondUsage.CacheRead == nil || *secondUsage.CacheRead <= 0 {
		t.Fatalf("second cost=%v usage=%+v", secondCost, secondUsage)
	}
	if *secondUsage.Input <= *secondUsage.CacheRead || !strings.Contains(string(normalizedPrompt([]byte(second))), "assistant") {
		t.Fatalf("unexpected prefix usage=%+v", secondUsage)
	}
}

func TestLocalEstimateCountsPartialOutput(t *testing.T) {
	input, output := 1.0, 1.0
	price := Price{Currency: "USD", Source: "test", Version: "1", InputCacheMiss: &input, InputCacheHit: &input, Output: &output}
	cache := NewPromptCache()
	cost, usage, source := cache.LocalEstimate("openai", "m", "session", []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`), []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"), price)
	if cost == nil || source != "local_estimated_cache_prefix" || usage.Output == nil || *usage.Output == 0 {
		t.Fatalf("cost=%v usage=%+v source=%s", cost, usage, source)
	}
}
