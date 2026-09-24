package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
			json.Unmarshal([]byte(requestBody("openai")), &input)
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
