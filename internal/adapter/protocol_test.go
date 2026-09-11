package adapter

import (
	"testing"
)

func TestUsageAndPricing(t *testing.T) {
	cases := []struct {
		protocol, body string
		input, output  int64
		cost           float64
	}{
		{"openai", `{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}`, 10, 2, 24e-6},
		{"openai", `{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":4,"prompt_cache_miss_tokens":6}`, 10, 2, 24e-6},
		{"anthropic", `{"input_tokens":6,"output_tokens":2,"cache_read_input_tokens":4,"cache_creation_input_tokens":3}`, 13, 2, 36e-6},
	}
	p := Price{Currency: "USD", Source: "test", Version: "1", Input: ptr(2.0), Output: ptr(4.0), CacheRead: ptr(1.0), CacheWrite: ptr(4.0)}
	for _, c := range cases {
		if c.protocol == "openai" {
			p.CacheWrite = nil
		} else {
			p.CacheWrite = ptr(4.0)
		}
		u, err := ParseUsage(c.protocol, []byte(c.body))
		if err != nil {
			t.Fatal(err)
		}
		if *u.Input != c.input || *u.Output != c.output {
			t.Fatalf("usage %+v", u)
		}
		v := p.Estimate(c.protocol, u)
		if v == nil || *v < c.cost-1e-12 || *v > c.cost+1e-12 {
			t.Fatalf("cost %v want %v", v, c.cost)
		}
	}
}
func TestUnknownUsageAndPartialCacheRemainUnknown(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"prompt_tokens":10}`, `{"prompt_tokens":-1,"completion_tokens":2}`, `{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":3,"prompt_cache_miss_tokens":2}`} {
		u, _ := ParseUsage("openai", []byte(body))
		p := Price{Input: ptr(1.0), Output: ptr(1.0), CacheRead: ptr(1.0)}
		if p.Estimate("openai", u) != nil {
			t.Fatalf("unknown cost inferred for %s", body)
		}
	}
}
func TestStrictRequests(t *testing.T) {
	for _, body := range []string{`{"model":"m","model":"x","messages":[]}`, `{} {}`, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[]}`, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`} {
		if _, _, err := Request("openai", []byte(body), "m"); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
