package diagnostics

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFailureRetainsOnlyBoundedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, code, want, state string
		configured              bool
	}{
		{name: "unmapped numeric code", code: "1310", want: "1310", state: "retained"},
		{name: "standard protocol type", code: "overloaded_error", want: "overloaded_error", state: "retained"},
		{name: "operator trusted code", code: "custom:quota.exhausted", configured: true, want: "custom:quota.exhausted", state: "retained"},
		{name: "untrusted ASCII text", code: "private_account_identifier", state: "redacted"},
		{name: "credential shaped code", code: "sk-private123456", state: "redacted"},
		{name: "raw session UUID", code: "12345678-1234-1234-1234-123456789012", state: "redacted"},
		{name: "oversize configured code", code: strings.Repeat("a", 129), configured: true, state: "redacted"},
		{name: "unsafe configured text", code: "private\naccount", configured: true, state: "redacted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &Failure{Source: "stream_error", Phase: "stream", HTTPStatus: 200, ErrorStatus: 503,
				ProviderCode: tc.code, ConfiguredCode: tc.configured, ProviderRequestID: "req_fixture12345678"}
			f := original.Normalize()
			if f.ProviderCode != tc.want || f.CodeState != tc.state || f.HTTPStatus != 200 || f.ErrorStatus != 503 || f.ProviderRequestID != original.ProviderRequestID {
				t.Fatalf("normalized failure=%+v", f)
			}
			if original.ProviderCode != tc.code {
				t.Fatal("normalization mutated the caller's value")
			}
		})
	}
	f := (&Failure{Source: "private source", Phase: "/private/path", HTTPStatus: 999, ErrorStatus: 200,
		ProviderRequestID: "https://private/?key=secret", Category: "private category", Retryability: "private policy"}).Normalize()
	data, _ := json.Marshal(f)
	if strings.Contains(string(data), "private") || strings.Contains(string(data), "secret") || f.HTTPStatus != 0 || f.ErrorStatus != 0 {
		t.Fatalf("untrusted values leaked: %s", data)
	}
	f = (&Failure{ProviderCode: "1310", ProviderRequestID: "req_fixture12345678"}).Redact("1310", "fixture12345678")
	if f.ProviderCode != "" || f.ProviderRequestID != "" || f.CodeState != "redacted" {
		t.Fatalf("echoed secrets survived: %+v", f)
	}
}
