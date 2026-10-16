package diagnostics

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMessageRedactionPrecedesBoundsAndPreservesFacts(t *testing.T) {
	secret := "opaque-credential-without-a-recognizable-shape"
	message := "prompt is too long: 236663 tokens > 200000 tokens. " + strings.Repeat("文", 430) + secret + " Bearer other-token sk-other123456 " + strings.Repeat("f", 48) + " account_id=private-account user@example.com\x1b[31m\n\u202e"
	f := (&Failure{ProviderMessage: message}).Redact(secret)
	if f.ActualTokens != 236663 || f.LimitTokens != 200000 || !strings.HasPrefix(f.ProviderMessage, "prompt is too long: 236663 tokens > 200000 tokens.") || utf8.RuneCountInString(f.ProviderMessage) > 512 {
		t.Fatalf("facts/bound lost: %+v", f)
	}
	for _, value := range []string{secret[:10], "other-token", "sk-other", "private-account", "user@example.com", "\x1b", "\u202e", "\n", strings.Repeat("f", 32)} {
		if strings.Contains(f.ProviderMessage, value) {
			t.Fatalf("message retained sensitive/control value %q", value)
		}
	}
	short := (&Failure{ProviderMessage: "Bearer token-value sk-shape12345 account_id=private-account a@example.com " + strings.Repeat("a", 40) + "\x1b[31m\n\u202e safe"}).Redact()
	if strings.Contains(short.ProviderMessage, "token-value") || strings.Contains(short.ProviderMessage, "private-account") || strings.Contains(short.ProviderMessage, "example.com") || strings.Contains(short.ProviderMessage, "[31m") || !strings.HasSuffix(short.ProviderMessage, "safe") {
		t.Fatalf("shape redaction failed: %+v", short)
	}
}

func TestTokenFactsRequireUnambiguousExplicitComparisons(t *testing.T) {
	for _, tc := range []struct {
		message       string
		actual, limit int64
	}{
		{"prompt is too long: 236663 tokens > 200000", 236663, 200000},
		{"maximum context length is 200000 tokens, however you requested 236663 tokens", 236663, 200000},
		{"10 tokens > 5 tokens; 10 tokens > 5 tokens", 10, 5},
		{"10 tokens > 5 tokens; 12 tokens > 5 tokens", 0, 0},
		{"10 tokens > 5 tokens; 3 tokens > 4 tokens", 0, 0},
		{"model supports 1000000 tokens; prompt is too long", 0, 0},
		{"5 tokens > 10 tokens", 0, 0},
		{"10 tokens > 0 tokens", 0, 0},
		{"10.5 tokens > 5 tokens", 0, 0},
		{"1,000 tokens > 500 tokens", 0, 0},
		{"-10 tokens > 5 tokens", 0, 0},
		{"9223372036854775808 tokens > 5", 0, 0},
	} {
		a, l := TokenFacts(tc.message)
		if a != tc.actual || l != tc.limit {
			t.Errorf("%q: got %d/%d, want %d/%d", tc.message, a, l, tc.actual, tc.limit)
		}
	}
}
