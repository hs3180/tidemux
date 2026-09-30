package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/ledger"
)

// Verify the proxy forwards a tool result in a second request, accepts a
// tool-only first response, and audits each model attempt independently.
func TestToolConversationThroughGateway(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if protocol == "openai" {
						io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
					} else {
						io.WriteString(w, `{"data":[{"id":"custom-model","type":"model"}],"has_more":false}`)
					}
					return
				}
				calls++
				data, _ := io.ReadAll(r.Body)
				if calls == 2 && (!strings.Contains(string(data), "call1") || !strings.Contains(string(data), "fixture-result")) {
					t.Error("tool result lost")
				}
				if strings.Contains(string(data), "local-secret") {
					t.Error("gateway credential leaked")
				}
				if calls == 1 {
					if protocol == "openai" {
						io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
					} else {
						io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"call1","name":"read_file","input":{}}],"usage":{"input_tokens":3,"output_tokens":2}}`)
					}
				} else {
					io.WriteString(w, responseBody(protocol))
				}
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
			c.Protocol = protocol
			c = namedProviderConfig(c, protocol+"-main")
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			var input map[string]any
			json.Unmarshal([]byte(requestBodyFor("openai", protocol+"-main", "custom-model")), &input)
			for turn := 0; turn < 2; turn++ {
				body, _ := json.Marshal(input)
				req := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(string(body)))
				req.Header.Set("Authorization", "Bearer local-secret")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != 200 {
					t.Fatalf("turn %d: %d %s", turn, w.Code, w.Body.String())
				}
				if turn == 0 {
					var response map[string]any
					json.Unmarshal(w.Body.Bytes(), &response)
					messages := input["messages"].([]any)
					assistant := response["choices"].([]any)[0].(map[string]any)["message"]
					messages = append(messages, assistant, map[string]any{"role": "tool", "tool_call_id": "call1", "content": "fixture-result"})
					input["messages"] = messages
				}
			}
			db, err := ledger.Open(c.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rows, err := db.Recent(context.Background(), 10)
			if err != nil || len(rows) != 2 || calls != 2 {
				t.Fatalf("calls=%d rows=%d err=%v", calls, len(rows), err)
			}
			for _, r := range rows {
				if r.Status != "ok" || r.InputTokens == nil || *r.InputTokens != 3 || r.OutputTokens == nil || *r.OutputTokens != 2 {
					t.Fatalf("bad audit %+v", r)
				}
			}
		})
	}
}

func TestAnthropicToolContinuationThroughOpenAIProvider(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/provider/v1/chat/completions" {
			t.Errorf("upstream path=%q", r.URL.Path)
		}
		if calls == 1 {
			io.WriteString(w, `{"id":"chat1","object":"chat.completion","model":"custom-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"lookup","arguments":"{\"id\":\"1\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
			return
		}
		if !strings.Contains(string(data), `"tool_call_id":"call1"`) || !strings.Contains(string(data), "fixture-result") {
			t.Errorf("Anthropic tool result did not continue as an OpenAI tool message: %s", data)
		}
		io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL+"/provider/v1")
	c.Protocol = "openai"
	h, closeDB, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	bodies := []string{
		`{"model":"legacy/custom-model","max_tokens":32,"messages":[{"role":"user","content":"look up item 1"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
		`{"model":"legacy/custom-model","max_tokens":32,"messages":[{"role":"user","content":"look up item 1"},{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"lookup","input":{"id":"1"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"fixture-result"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
	}
	for turn, body := range bodies {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", "local-secret")
		req.Header.Set("anthropic-version", "2023-06-01")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if out.Code != 200 {
			t.Fatalf("turn %d: status=%d body=%s", turn, out.Code, out.Body.String())
		}
		if turn == 0 && (!strings.Contains(out.Body.String(), `"type":"tool_use"`) || !strings.Contains(out.Body.String(), `"id":"call1"`)) {
			t.Fatalf("OpenAI tool call was not converted for Anthropic client: %s", out.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("provider calls=%d want 2", calls)
	}
}

func TestNativeAnthropicServerToolIsForwardedUnchanged(t *testing.T) {
	const toolDefinition = `{"type":"web_search_20250305","name":"web_search","max_uses":3,"allowed_domains":["example.com"],"user_location":{"type":"approximate","country":"US","city":"Seattle"},"provider_extension":{"mode":"fast","limits":[1,2]}}`
	var received []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"data":[{"id":"claude-test","type":"model"}],"has_more":false}`)
			return
		}
		received, _ = io.ReadAll(r.Body)
		io.WriteString(w, responseBody("anthropic"))
	}))
	defer up.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL+"/v1"), "anthropic-main")
	provider := c.Providers["anthropic-main"]
	provider.Protocol = "anthropic"
	provider.SupportedModels = []string{"claude-test"}
	c.Providers["anthropic-main"] = provider
	h, closeDB, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"anthropic-main/claude-test","max_tokens":64,"messages":[{"role":"user","content":"search"}],"tools":[` + toolDefinition + `]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "local-secret")
	req.Header.Set("anthropic-version", "2023-06-01")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
	}
	var got struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(received, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || !jsonEqual(got.Tools[0], json.RawMessage(toolDefinition)) {
		t.Fatalf("server tool changed in upstream request: %s", received)
	}
}

func TestCrossProtocolAnthropicServerToolReturnsActionableError(t *testing.T) {
	posts := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
			return
		}
		posts++
		io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL+"/v1"), "openai-main")
	provider := c.Providers["openai-main"]
	provider.SupportedModels = []string{"custom-model"}
	c.Providers["openai-main"] = provider
	h, closeDB, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"openai-main/custom-model","max_tokens":64,"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "local-secret")
	req.Header.Set("anthropic-version", "2023-06-01")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusBadRequest || !strings.Contains(out.Body.String(), "unsupported_request_feature") || !strings.Contains(out.Body.String(), "tools cannot be represented") || posts != 0 {
		t.Fatalf("status=%d posts=%d body=%s", out.Code, posts, out.Body.String())
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}
