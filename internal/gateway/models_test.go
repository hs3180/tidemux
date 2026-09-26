package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestModelsListsConfiguredModelAndRequiresAuthentication(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "http://127.0.0.1:1")
		c.Protocol = protocol
		c = namedProviderConfig(c, "test")
		provider := c.Providers["test"]
		provider.SupportedModels = []string{"custom-model"}
		c.Providers["test"] = provider
		h, closeDB, err := NewHandler(c, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer closeDB()
		for _, path := range []string{"/v1/models", "/models"} {
			for _, authorized := range []bool{false, true} {
				req := httptest.NewRequest("GET", path, nil)
				if authorized && protocol == "anthropic" {
					req.Header.Set("x-api-key", "local-secret")
					req.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
				} else if authorized {
					req.Header.Set("Authorization", "Bearer local-secret")
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if !authorized {
					if w.Code != 401 {
						t.Fatal(w.Code)
					}
					continue
				}
				if w.Code != 200 {
					t.Fatal(w.Code)
				}
				var result struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Data) != 1 || result.Data[0].ID != "test/custom-model" {
					t.Fatal(w.Body.String())
				}
			}
		}
	}
}

func TestModelDetailUsesConfiguredIdentity(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "http://127.0.0.1:1")
		c.Protocol = protocol
		c = namedProviderConfig(c, "test")
		provider := c.Providers["test"]
		provider.SupportedModels = []string{"custom-model"}
		c.Providers["test"] = provider
		h, closeDB, err := NewHandler(c, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer closeDB()
		for _, path := range []string{"/v1/models/", "/models/"} {
			for _, name := range []string{"test/custom-model", "missing/model"} {
				r := httptest.NewRequest("GET", path+name, nil)
				if protocol == "anthropic" {
					r.Header.Set("x-api-key", "local-secret")
					r.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
				} else {
					r.Header.Set("Authorization", "Bearer local-secret")
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if name != "test/custom-model" {
					if w.Code != 404 {
						t.Fatal(w.Code)
					}
					continue
				}
				var body map[string]any
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["id"] != "test/custom-model" {
					t.Fatal(w.Code, w.Body.String())
				}
				if body["context_length"] != nil || body["pricing"] != nil {
					t.Fatal("invented model capabilities")
				}
			}
		}
	}
}
