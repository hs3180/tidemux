package adapter

import (
	"strings"
	"testing"
)

func TestDshCompatibilityFields(t *testing.T) {
	for _, test := range []struct {
		name            string
		protocol        string
		reasoningEffort string
		wantReasoning   bool
	}{
		{name: "anthropic high", protocol: "anthropic", reasoningEffort: "high"},
		{name: "anthropic low", protocol: "anthropic", reasoningEffort: "low"},
		{name: "openai high", protocol: "openai", reasoningEffort: "high", wantReasoning: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"model":"m","dsh_plugin_packages":[{"name":"demo"}],"reasoning_effort":"` + test.reasoningEffort + `","messages":[{"role":"user","content":"hi"}]}`
			if test.protocol == "anthropic" {
				body = `{"model":"m","max_tokens":256,"dsh_plugin_packages":[{"name":"demo"}],"reasoning_effort":"` + test.reasoningEffort + `","messages":[{"role":"user","content":"hi"}]}`
			}
			encoded, model, err := Request(test.protocol, []byte(body), "fallback")
			if err != nil || model != "m" {
				t.Fatalf("model=%q err=%v body=%s", model, err, encoded)
			}
			if strings.Contains(string(encoded), `"dsh_plugin_packages"`) {
				t.Fatalf("client metadata was forwarded: %s", encoded)
			}
			gotReasoning := strings.Contains(string(encoded), `"reasoning_effort"`)
			if gotReasoning != test.wantReasoning {
				t.Fatalf("reasoning_effort forwarded=%v want %v: %s", gotReasoning, test.wantReasoning, encoded)
			}
		})
	}
}

func TestDshCompatibilityFieldsOpenAIToAnthropic(t *testing.T) {
	body := []byte(`{"model":"m","dsh_plugin_packages":{"packages":["demo"]},"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	encoded, model, err := PrepareRequest("openai", "anthropic", body, "fallback", 0)
	if err != nil || model != "m" {
		t.Fatalf("model=%q err=%v body=%s", model, err, encoded)
	}
	if strings.Contains(string(encoded), `"dsh_plugin_packages"`) || strings.Contains(string(encoded), `"reasoning_effort"`) {
		t.Fatalf("compatibility fields were forwarded: %s", encoded)
	}
}
