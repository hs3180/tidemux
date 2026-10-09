package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
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
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, streaming), func(t *testing.T) {
				posts := 0
				fixture := glmDualOutputFixture(t, streaming)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					posts++
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					_, _ = w.Write(fixture)
				}))
				defer upstream.Close()
				client, l := newCandidateTestClient(t, upstream.Client())
				defer l.Close()
				client.Protocol, client.BaseURL = "anthropic", upstream.URL+"/v1"
				body := []byte(fmt.Sprintf(`{"model":"claude-haiku-4-5","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, streaming))
				var emitted bytes.Buffer
				var sink StreamSink
				if streaming {
					sink = func(_ string, frame []byte) error { emitted.Write(frame); return nil }
				}
				response, id, err := client.CallFrom(protocol, context.Background(), body, "claude-haiku-4-5", sink, CallOptions{})
				if err != nil || posts != 1 || id == "" {
					t.Fatalf("failed or replayed request: posts=%d id=%s err=%v", posts, id, err)
				}
				output := append(emitted.Bytes(), response...)
				if !bytes.Contains(output, []byte("GLM_WEB_READER_OUTPUT")) || !bytes.Contains(output, []byte("GLM_DUAL_OUTPUT_OK")) {
					t.Fatal("available assistant text was lost")
				}
				if protocol == "anthropic" && !bytes.Equal(output, fixture) {
					t.Fatal("native response content or stream indices were rewritten")
				}
				if protocol == "openai" && (bytes.Contains(output, []byte(`"type":"tool_result"`)) || bytes.Contains(output, []byte("event: error"))) {
					t.Fatal("server tool was exposed as a client tool or conversion aborted")
				}
				rows, auditErr := l.Recent(context.Background(), 10)
				if auditErr != nil || len(rows) != 1 || rows[0].Status != "ok" || rows[0].InputTokens == nil || *rows[0].InputTokens != 10 || rows[0].OutputTokens == nil || *rows[0].OutputTokens != 10 {
					t.Fatalf("usage lost: %+v err=%v", rows, auditErr)
				}
				if protocol == "anthropic" && !streaming {
					var message map[string]json.RawMessage
					_ = json.Unmarshal(response, &message)
					continuation := []byte(`{"model":"claude-haiku-4-5","max_tokens":32,"messages":[{"role":"assistant","content":` + string(message["content"]) + `},{"role":"user","content":"continue"}]}`)
					encoded, _, err := Request("anthropic", continuation, "claude-haiku-4-5")
					if err != nil {
						t.Fatalf("native tool history could not continue: %v", err)
					}
					var before, after any
					_ = json.Unmarshal(continuation, &before)
					_ = json.Unmarshal(encoded, &after)
					if !reflect.DeepEqual(before, after) {
						t.Fatal("native continuation was pruned")
					}
				}
			})
		}
	}
}

func TestNativeServerToolResultsPreserveUniqueContent(t *testing.T) {
	for _, result := range []string{
		`{"type":"tool_result","tool_use_id":"server","content":"unique output"}`,
		`{"type":"tool_result","tool_use_id":"server","content":[{"type":"text","text":"unique output"}],"is_error":true}`,
		`{"type":"tool_result","tool_use_id":"server","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}`,
	} {
		content := `[{"type":"server_tool_use","id":"server","name":"webReader","input":{}},` + result + `]`
		body := []byte(`{"type":"message","role":"assistant","content":` + content + `,"usage":{"input_tokens":1,"output_tokens":2}}`)
		if _, err := ValidateResponse("anthropic", body); err != nil {
			t.Fatalf("native server tool result rejected: %v", err)
		}
		if output, _, err := TranslateResponseWithWarnings("anthropic", "anthropic", body, "m"); err != nil || !bytes.Equal(output, body) {
			t.Fatal("unique native server-tool content was modified")
		}
	}
}

func TestAssistantServerToolPairingCannotUseClientToolsOrOtherMessages(t *testing.T) {
	for _, before := range []string{
		``,
		`{"type":"tool_use","id":"server","name":"read_file","input":{}},`,
		`{"type":"server_tool_use","id":"other-server","name":"webReader","input":{}},`,
		`{"type":"server_tool_use","id":"server","name":"webReader","input":null},`,
	} {
		content := `[` + before + `{"type":"tool_result","tool_use_id":"server","content":"private-output"}]`
		body := []byte(`{"type":"message","role":"assistant","content":` + content + `}`)
		if _, err := ValidateResponse("anthropic", body); err == nil || err.Error() != "invalid_upstream_tool_history" {
			t.Fatalf("accepted an unpaired assistant result: %v", err)
		}
	}
	body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"server","name":"webReader","input":{}}]},{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"server","content":"private-output"}]}]}`)
	if _, _, err := Request("anthropic", body, "m"); err == nil || ValidationParameter(err) != "messages[1].content[0].type" {
		t.Fatalf("paired a result with a server tool from another message: %v", err)
	}
}

func TestStreamServerToolResultsPreserveOriginalFrames(t *testing.T) {
	var fixture bytes.Buffer
	if err := json.Compact(&fixture, glmDualOutputFixture(t, false)); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []string{
		string(glmDualOutputFixture(t, true)),
		"event: message_start\ndata: " + `{"type":"message_start","message":` + fixture.String() + "}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	} {
		var emitted bytes.Buffer
		_, terminal, err := readStream("anthropic", strings.NewReader(stream), func(frame []byte) error { emitted.Write(frame); return nil })
		if err != nil || emitted.String()+string(terminal) != stream {
			t.Fatalf("native stream was changed: %v", err)
		}
	}
}

func TestMalformedPairedServerToolResultFailsClosed(t *testing.T) {
	server := `{"type":"server_tool_use","id":"server","name":"webReader","input":{}}`
	for _, result := range []string{
		`{"type":"tool_result","tool_use_id":"other-server","content":"private-result"}`,
		`{"type":"tool_result","tool_use_id":"server","content":[{"type":"tool_use","id":"private-result","name":"f","input":{}}]}`,
		`{"type":"tool_result","tool_use_id":"server","content":"private-result","text":"wrong-shape"}`,
	} {
		body := []byte(`{"type":"message","role":"assistant","content":[` + server + `,` + result + `]}`)
		if _, err := ValidateResponse("anthropic", body); err == nil {
			t.Fatal("malformed paired result was accepted")
		}
		stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n\n" +
			"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":` + server + "}\n\n" +
			"event: content_block_start\ndata: " + `{"type":"content_block_start","index":1,"content_block":` + result + "}\n\n"
		var emitted bytes.Buffer
		_, terminal, err := readStream("anthropic", strings.NewReader(stream), func(frame []byte) error { emitted.Write(frame); return nil })
		var failure *CallError
		if !errors.As(err, &failure) || failure.Code != "invalid_upstream_tool_history" || failure.Param != "content[1].type" || terminal != nil || strings.Contains(emitted.String(), "private-result") {
			t.Fatalf("invalid result escaped: %v", err)
		}
	}
	duplicate := []byte(`{"type":"message","role":"assistant","content":[` + server + `,{"type":"tool_result","tool_use_id":"server","content":"unique","content":"duplicate"}]}`)
	if _, err := ValidateResponse("anthropic", duplicate); err == nil {
		t.Fatal("duplicate JSON keys were accepted")
	}
}
