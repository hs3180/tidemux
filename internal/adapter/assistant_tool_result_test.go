package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Sanitized reproduction of #128's reported GLM server-tool/text/tool_result
// structure. This is a local fixture, not a production payload or live API call.
func glmDualOutputFixture(t *testing.T, streaming bool) []byte {
	t.Helper()
	name := "testdata/glm_dual_output.json"
	if streaming {
		name = "testdata/glm_dual_output.sse"
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGLMDualOutputCompletesWithoutReplayAndPreservesUsage(t *testing.T) {
	for _, protocol := range []string{"anthropic", "openai"} {
		for _, streaming := range []bool{false, true} {
			for _, policy := range []string{"reject", StripRedundantToolResults} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, streaming, policy), func(t *testing.T) {
					posts := 0
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						posts++
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						_, _ = w.Write(glmDualOutputFixture(t, streaming))
					}))
					defer upstream.Close()
					client, l := newCandidateTestClient(t, upstream.Client())
					defer l.Close()
					client.Protocol, client.BaseURL, client.AssistantToolResultPolicy = "anthropic", upstream.URL+"/v1", policy
					body := []byte(fmt.Sprintf(`{"model":"claude-haiku-4-5","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, streaming))
					var emitted strings.Builder
					var sink StreamSink
					if streaming {
						sink = func(_ string, frame []byte) error { emitted.Write(frame); return nil }
					}
					response, id, err := client.CallFrom(protocol, context.Background(), body, "claude-haiku-4-5", sink, CallOptions{})
					if posts != 1 || id == "" {
						t.Fatalf("replayed request or absent audit: posts=%d id=%s", posts, id)
					}
					rows, auditErr := l.Recent(context.Background(), 10)
					if auditErr != nil || len(rows) != 1 {
						t.Fatalf("audit mismatch: rows=%d err=%v", len(rows), auditErr)
					}
					if policy == "reject" {
						var callErr *CallError
						if !errors.As(err, &callErr) || callErr.Code != "invalid_upstream_tool_history" {
							t.Fatalf("strict default changed: %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					output := emitted.String() + string(response)
					if !strings.Contains(output, "GLM_WEB_READER_OUTPUT") || !strings.Contains(output, "GLM_DUAL_OUTPUT_OK") || strings.Contains(output, `"type":"tool_result"`) {
						t.Fatal("duplicate result escaped or available output was lost")
					}
					if rows[0].Status != "ok" || rows[0].InputTokens == nil || *rows[0].InputTokens != 10 || rows[0].OutputTokens == nil || *rows[0].OutputTokens != 10 {
						t.Fatalf("usage lost: %+v", rows[0])
					}
					if protocol == "anthropic" && !streaming {
						var message map[string]json.RawMessage
						_ = json.Unmarshal(response, &message)
						continuation := []byte(`{"model":"claude-haiku-4-5","max_tokens":32,"messages":[{"role":"assistant","content":` + string(message["content"]) + `},{"role":"user","content":"continue"}]}`)
						if _, _, err := Request("anthropic", continuation, "claude-haiku-4-5"); err != nil {
							t.Fatalf("poisoned continuation: %v", err)
						}
					}
					if protocol == "anthropic" && streaming && (!strings.Contains(output, `"index":2`) || strings.Contains(output, `"index":3`)) {
						t.Fatal("suppressed block left holes in client history")
					}
				})
			}
		}
	}
}

func TestAssistantToolResultCompatibilityStillRejectsUniqueOrOpaqueResults(t *testing.T) {
	for _, content := range []string{
		`"unique-data"`, `[{"type":"image","source":{"data":"opaque"}}]`, `[{"type":"text","text":"duplicate","citations":[1]}]`, `null`, `""`,
	} {
		filter := &assistantToolResultFilter{}
		data := []byte(`{"content":[{"type":"text","text":"duplicate"},{"type":"tool_result","tool_use_id":"id","content":` + content + `}]}`)
		if _, err := filter.response(data); err == nil {
			t.Fatalf("discarded unique or unrecognized result: %s", content)
		}
	}
	valid := []byte(`{"content":[{"type":"server_tool_use","id":"server","name":"web_search","input":{}},{"type":"web_search_tool_result","tool_use_id":"server","content":[{"type":"web_search_result","url":"https://example.invalid/","encrypted_content":"opaque"}]}]}`)
	if out, err := (&assistantToolResultFilter{}).response(valid); err != nil || string(out) != string(valid) {
		t.Fatal("valid native server-tool content was modified")
	}
	duplicate := []byte(`{"content":[{"type":"text","text":"duplicate"},{"type":"tool_result","tool_use_id":"id","content":"unique","content":"duplicate"}]}`)
	if _, err := (&assistantToolResultFilter{}).response(duplicate); err == nil {
		t.Fatal("duplicate JSON keys hid unique output")
	}
}

func TestAssistantToolResultStreamNeverDropsAdditionalDeltaContent(t *testing.T) {
	for _, extra := range []string{
		`{"type":"text_delta","text":"unique"}`, `{"type":"input_json_delta","partial_json":"private"}`, `{"type":"text_delta","text":"GLM_WEB_READER_OUTPUT","extra":"private"}`,
	} {
		stream := string(glmDualOutputFixture(t, true))
		stop := "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n"
		stream = strings.Replace(stream, stop, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":"+extra+"}\n\n"+stop, 1)
		var emitted strings.Builder
		_, terminal, err := readStreamWithLimits("anthropic", strings.NewReader(stream), Limits{}.Effective(), func(b []byte) error { emitted.Write(b); return nil }, streamErrorContext{toolResults: &assistantToolResultFilter{}})
		if err == nil || terminal != nil || strings.Contains(emitted.String(), "private") || strings.Contains(emitted.String(), "unique") {
			t.Fatal("additional malformed output was dropped or leaked instead of failing closed")
		}
	}
}
