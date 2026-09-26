package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestModelCapabilitiesAreExplicitAndValidated(t *testing.T) {
	for _, bad := range []ModelCapabilities{{ContextTokens: -1}, {MaxOutputTokens: -1}, {ContextTokens: 100, MaxOutputTokens: 101}} {
		if bad.Validate() == nil {
			t.Fatal("invalid model metadata accepted")
		}
	}
	for _, protocol := range []string{"openai", "anthropic"} {
		c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "http://127.0.0.1:1")
		c.Protocol = protocol
		c.ModelCapabilities = ModelCapabilities{ContextTokens: 65536, MaxOutputTokens: 8192}
		c = namedProviderConfig(c, "test")
		provider := c.Providers["test"]
		provider.SupportedModels = []string{"custom-model"}
		c.Providers["test"] = provider
		h, closeDB, err := NewHandler(c, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer closeDB()
		for _, path := range []string{"/v1/models", "/v1/models/test/custom-model"} {
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("Authorization", "Bearer local-secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var body map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
				t.Fatal(w.Code)
			}
			if path == "/v1/models" {
				body = body["data"].([]any)[0].(map[string]any)
			}
			if body["context_length"] != float64(65536) || body["max_output_tokens"] != float64(8192) {
				t.Fatal("configured capabilities lost")
			}
		}
	}
}
