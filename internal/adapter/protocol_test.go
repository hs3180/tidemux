package adapter

import (
	"strings"
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
	for _, body := range []string{`{"model":"m","model":"x","messages":[]}`, `{} {}`, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}]}`, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":"invalid"}`} {
		if _, _, err := Request("openai", []byte(body), "m"); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestExplicitThinkingDisable(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		body := []byte(`{"model":"deepseek-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)
		encoded, _, err := Request(protocol, body, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"thinking":{"type":"disabled"}`) {
			t.Fatal("explicit setting lost")
		}
		if _, _, err = Request(protocol, []byte(strings.Replace(string(body), "disabled", "invalid", 1)), ""); err == nil {
			t.Fatal("unsupported thinking accepted")
		}
	}
}

func TestThinkingDisplay(t *testing.T) {
	for _, display := range []string{"summarized", "omitted"} {
		body := `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive","display":"` + display + `"}}`
		encoded, _, err := Request("anthropic", []byte(body), "")
		if err != nil || !strings.Contains(string(encoded), `"display":"`+display+`"`) {
			t.Fatalf("display not preserved: %v", err)
		}
		for _, invalid := range []string{strings.Replace(body, display, "invalid", 1), strings.Replace(body, "adaptive", "disabled", 1)} {
			if _, _, err := Request("anthropic", []byte(invalid), ""); err == nil {
				t.Fatal("invalid thinking display accepted")
			}
		}
		if _, _, err := Request("openai", []byte(strings.Replace(body, "adaptive", "enabled", 1)), ""); err == nil {
			t.Fatal("Anthropic display accepted for OpenAI")
		}
	}
}
