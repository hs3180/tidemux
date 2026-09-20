package adapter

import (
	"strings"
	"testing"
)

func TestBetaHeaderValidation(t *testing.T) {
	for _, bad := range []string{"feature\n", "feature\r\nx-api-key: secret", ",feature", "feature,,other", "feature:secret", strings.Repeat("x", 4097)} {
		if (CallOptions{AnthropicBeta: bad}).Validate("anthropic") == nil {
			t.Fatal("invalid beta accepted")
		}
	}
	if (CallOptions{AnthropicBeta: "feature-2026-01-01"}).Validate("openai") == nil {
		t.Fatal("Anthropic header accepted for OpenAI")
	}
	if err := (CallOptions{AnthropicBeta: "feature-2026-01-01, another-feature"}).Validate("anthropic"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionIDValidation(t *testing.T) {
	for _, bad := range []string{"session\n", "session\r", "session\x00", strings.Repeat("x", 257)} {
		if (CallOptions{SessionID: bad}).Validate("openai") == nil {
			t.Fatalf("invalid session ID accepted: %q", bad)
		}
	}
	if err := (CallOptions{SessionID: "claude-code-session"}).Validate("anthropic"); err != nil {
		t.Fatal(err)
	}
}
