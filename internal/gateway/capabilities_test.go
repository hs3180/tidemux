package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

func TestCatalogLimitsRejectAmbiguousAndInvalidMetadata(t *testing.T) {
	for _, test := range []struct {
		name, protocol, records string
		want                    ModelCapabilities
		warning                 bool
	}{
		{"context alias", "openai", `{"id":"m","max_model_len":262144,"max_output_tokens":8192}`, ModelCapabilities{262144, 8192}, false},
		{"anthropic fields", "anthropic", `{"id":"m","max_input_tokens":65536,"max_tokens":4096}`, ModelCapabilities{65536, 4096}, false},
		{"missing", "openai", `{"id":"m"}`, ModelCapabilities{}, false},
		{"null unknown", "openai", `{"id":"m","context_length":null,"max_output_tokens":0}`, ModelCapabilities{}, false},
		{"contradictory aliases", "openai", `{"id":"m","context_length":8192,"max_model_len":4096}`, ModelCapabilities{}, true},
		{"contradictory records", "openai", `{"id":"m","context_length":8192},{"id":"m","context_length":4096},{"id":"m","context_length":8192}`, ModelCapabilities{}, true},
		{"output exceeds context", "openai", `{"id":"m","context_length":4096,"max_output_tokens":8192}`, ModelCapabilities{}, true},
		{"string is not a limit", "openai", `{"id":"m","context_length":"262144"}`, ModelCapabilities{}, true},
		{"fraction is not a limit", "openai", `{"id":"m","context_length":4096.5}`, ModelCapabilities{}, true},
		{"overflow", "openai", `{"id":"m","context_length":18446744073709551615}`, ModelCapabilities{}, true},
		{"negative", "openai", `{"id":"m","context_length":-1}`, ModelCapabilities{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			prefix := `"object":"list"`
			if test.protocol == "anthropic" {
				prefix = `"has_more":false`
			}
			catalog := parseProviderCatalog([]byte(`{`+prefix+`,"data":[`+test.records+`]}`), test.protocol)
			if !catalog.known || catalog.capabilities["m"] != test.want || catalog.warnings["m"] != test.warning {
				t.Fatalf("catalog=%+v", catalog)
			}
		})
	}
	partial := parseProviderCatalog([]byte(`{"object":"list","has_more":true,"data":[{"id":"m","context_length":8192}]}`), "openai")
	if partial.known || len(partial.capabilities) != 0 {
		t.Fatal("learned capabilities from an incomplete catalog")
	}
}

func TestDiscoveredLimitsStayPerModelAcrossBothDiscoveryProtocols(t *testing.T) {
	for _, providerProtocol := range []string{"openai", "anthropic"} {
		t.Run(providerProtocol, func(t *testing.T) {
			var mu sync.Mutex
			gets := 0
			outputs := map[string]int64{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodGet {
					gets++
					prefix := `"object":"list"`
					if providerProtocol == "anthropic" {
						prefix = `"has_more":false`
					}
					fmt.Fprint(w, `{`+prefix+`,"data":[{"id":"small","context_length":16384,"max_output_tokens":1024},{"id":"large","max_model_len":262144,"max_output_tokens":8192}]}`)
					return
				}
				var body struct {
					Model     string `json:"model"`
					MaxTokens int64  `json:"max_tokens"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid request")
				}
				outputs[body.Model] = body.MaxTokens
				io.WriteString(w, responseBody(providerProtocol))
			}))
			defer upstream.Close()
			c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL), "p")
			p := c.Providers["p"]
			p.Protocol, p.SupportedModels = providerProtocol, []string{"small", "large"}
			c.Providers["p"] = p
			h, closeGateway, err := NewHandler(c, upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeGateway()
			for _, clientProtocol := range []string{"openai", "anthropic"} {
				for _, modelID := range []string{"small", "large"} {
					for _, path := range []string{"/v1/models", "/models/p/" + modelID} {
						req := httptest.NewRequest(http.MethodGet, path, nil)
						req.Header.Set("Authorization", "Bearer local-secret")
						if clientProtocol == "anthropic" {
							req.Header.Set("anthropic-version", defaultAnthropicAPIVersion)
						}
						w := httptest.NewRecorder()
						h.ServeHTTP(w, req)
						var result map[string]any
						if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
							t.Fatal(w.Code, w.Body.String())
						}
						if path == "/v1/models" {
							for _, item := range result["data"].([]any) {
								if item.(map[string]any)["id"] == "p/"+modelID {
									result = item.(map[string]any)
									break
								}
							}
						}
						wantContext, wantOutput := float64(16384), float64(1024)
						if modelID == "large" {
							wantContext, wantOutput = 262144, 8192
						}
						if result["context_length"] != wantContext || result["max_output_tokens"] != wantOutput {
							t.Fatalf("limits for %s: %v", modelID, result)
						}
					}
				}
			}
			if providerProtocol == "anthropic" {
				for _, model := range []string{"small", "large"} {
					req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"p/`+model+`","messages":[{"role":"user","content":"fixture"}]}`))
					req.Header.Set("Authorization", "Bearer local-secret")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					if w.Code != 200 {
						t.Fatal(w.Code, w.Body.String())
					}
				}
				mu.Lock()
				if outputs["small"] != 1024 || outputs["large"] != 4096 {
					t.Errorf("conversion did not respect a smaller limit or increased the default budget: %v", outputs)
				}
				mu.Unlock()
			}
			mu.Lock()
			defer mu.Unlock()
			if gets != 1 {
				t.Fatalf("downstream discovery caused extra upstream reads: %d", gets)
			}
		})
	}
}

func TestCapabilitiesConfigPrecedenceUnknownWarningsAndReloadSnapshot(t *testing.T) {
	var catalogMu sync.Mutex
	contextLimit, reads := 262144, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogMu.Lock()
		defer catalogMu.Unlock()
		reads++
		fmt.Fprintf(w, `{"object":"list","data":[{"id":"custom-model","context_length":%d,"max_output_tokens":8192}]}`, contextLimit)
	}))
	defer upstream.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL), "p")
	var logs bytes.Buffer
	h, closeGateway, err := NewHandlerWithLogger(c, upstream.Client(), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	root := h.(*handler)
	changed := c
	changed.Providers = map[string]Provider{"p": c.Providers["p"]}
	p := changed.Providers["p"]
	p.ModelCapabilities = ModelCapabilities{65536, 4096}
	changed.Providers["p"] = p
	catalogMu.Lock()
	contextLimit = 131072
	catalogMu.Unlock()
	view, err := root.prepareConfigView(context.Background(), changed, c, root, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	view.warnModelCapabilities("p")
	if view.capabilitiesForModel("p", "custom-model") != (ModelCapabilities{65536, 4096}) || !strings.Contains(logs.String(), "model_capabilities_conflict") {
		t.Fatal("operator precedence/conflict warning lost")
	}
	rotated := c
	rotated.Providers = map[string]Provider{"p": c.Providers["p"]}
	p = rotated.Providers["p"]
	p.APIKey = "new-provider-secret"
	rotated.Providers["p"] = p
	fresh, err := root.prepareConfigView(context.Background(), rotated, c, root, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	if root.capabilitiesForModel("p", "custom-model").ContextTokens != 262144 || fresh.capabilitiesForModel("p", "custom-model").ContextTokens != 131072 {
		t.Fatal("reload overwrote the admitted view or reused another connection's catalog")
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if reads != 2 {
		t.Fatalf("policy-only reload refreshed catalog or connection reload did not: %d", reads)
	}
	for _, secret := range []string{"provider-secret", "new-provider-secret", "local-secret", upstream.URL} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("capability warning leaked private context")
		}
	}
	unknown := &handler{providers: map[string]Provider{"p": {}}, catalogs: map[string]providerCatalog{}, logger: root.logger}
	unknown.warnModelCapabilities("p")
	if !strings.Contains(logs.String(), "model_capabilities_unknown") || unknown.capabilitiesForModel("p", "glm-5.3") != (ModelCapabilities{}) {
		t.Fatal("unknown context was silent or guessed from a model name")
	}
}
