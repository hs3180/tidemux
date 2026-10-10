package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNativeAnthropicBlocksRemainOpaqueAcrossResponseAndHistory(t *testing.T) {
	for _, content := range []string{
		`[{"type":"tool_result","tool_use_id":"unpaired","content":"unique output"}]`,
		`[{"type":"tool_result","content":{"provider_result":[1,2,3]},"caller":{"provider":"extension"}}]`,
		`[{"type":"server_tool_use","id":"other","name":"webReader","input":null},{"type":"tool_result","tool_use_id":"unpaired","content":"unique output"}]`,
		`[{"type":"tool_result","tool_use_id":"later","content":"unique output"},{"type":"server_tool_use","id":"later","name":"webReader","input":{}}]`,
		`[{"type":"text","text":{"provider_text":"opaque"}},{"type":"future_block","data":{"large_number":9007199254740993}}]`,
		`[{"type":"tool_use","id":17,"name":["provider extension"],"input":[1,2],"cache_control":{"type":"future"}}]`,
	} {
		response := []byte(`{"type":"message","role":"assistant","content":` + content + `,"usage":{"input_tokens":2,"output_tokens":1},"choices":{"provider_extension":true}}`)
		usage, err := ValidateResponse("anthropic", response)
		if err != nil || usage.Input == nil || *usage.Input != 2 || usage.Output == nil || *usage.Output != 1 {
			t.Fatalf("native content rejected or usage lost: %v", err)
		}
		if output, _, err := TranslateResponseWithWarnings("anthropic", "anthropic", response, "m"); err != nil || !bytes.Equal(output, response) {
			t.Fatalf("native response rewritten: %v", err)
		}
		for _, compacted := range []bool{false, true} {
			prefix := `{"role":"assistant","content":[{"type":"server_tool_use","id":"prior-message","input":{}}]},`
			if compacted {
				prefix = `{"role":"assistant","content":[{"type":"compaction","content":"summary"}]},`
			}
			request := []byte(`{"model":"m","max_tokens":16,"messages":[` + prefix + `{"role":"assistant","content":` + content + `},{"role":"user","content":"continue"}]}`)
			encoded, _, warnings, err := RequestWithWarnings("anthropic", request, "m")
			if err != nil || len(warnings) != 0 {
				t.Fatalf("native history rejected or reported as dropped: warnings=%v err=%v", warnings, err)
			}
			var before, after any
			_ = json.Unmarshal(request, &before)
			_ = json.Unmarshal(encoded, &after)
			if !reflect.DeepEqual(before, after) || !bytes.Contains(encoded, []byte(content)) {
				t.Fatal("native history content was changed")
			}
		}
		var blocks []json.RawMessage
		_ = json.Unmarshal([]byte(content), &blocks)
		separate := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":2}}}\n\n"
		for index, block := range blocks {
			separate += fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", index+4, block, index+4)
		}
		start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" + string(response) + "}\n\n"
		for _, stream := range []string{start, separate} {
			stream += "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			var emitted bytes.Buffer
			usage, terminal, err := readStream("anthropic", strings.NewReader(stream), func(frame []byte) error { emitted.Write(frame); return nil })
			if err != nil || emitted.String()+string(terminal) != stream || usage.Input == nil || *usage.Input != 2 || usage.Output == nil || *usage.Output != 1 {
				t.Fatalf("native stream changed or usage lost: %v", err)
			}
		}
	}
}

func TestNativeAnthropicRejectsInvalidContentEnvelopes(t *testing.T) {
	for _, content := range []string{`null`, `{}`, `[null]`, `[{}]`, `[{"type":17}]`, `[{"type":"text","text":"one","text":"two"}]`} {
		request := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":` + content + `}]}`)
		if _, _, err := Request("anthropic", request, "m"); err == nil {
			t.Fatalf("accepted invalid request envelope %s", content)
		}
		response := []byte(`{"type":"message","role":"assistant","content":` + content + `}`)
		if _, err := ValidateResponse("anthropic", response); err == nil {
			t.Fatalf("accepted invalid response envelope %s", content)
		}
		stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" + string(response) + "}\n\n"
		var emitted bytes.Buffer
		_, terminal, err := readStream("anthropic", strings.NewReader(stream), func(frame []byte) error { emitted.Write(frame); return nil })
		if err == nil || emitted.Len() != 0 || terminal != nil {
			t.Fatalf("invalid message_start escaped: %v", err)
		}
	}
	for _, block := range []string{`null`, `[]`, `{"content":"private-result"}`, `{"type":17,"content":"private-result"}`, `{"type":"text","text":"one","text":"private-result"}`} {
		stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":4,\"content_block\":" + block + "}\n\n"
		var emitted bytes.Buffer
		_, terminal, err := readStream("anthropic", strings.NewReader(stream), func(frame []byte) error { emitted.Write(frame); return nil })
		if err == nil || terminal != nil || strings.Contains(emitted.String(), "private-result") {
			t.Fatalf("invalid content_block_start escaped: %v", err)
		}
	}
}

func TestAnthropicConversionValidatesRequiredToolFields(t *testing.T) {
	for _, block := range []string{
		`{"type":"tool_result","content":"output"}`,
		`{"type":"tool_use","name":"read_file","input":{}}`,
		`{"type":"tool_use","id":"call","name":"read_file","input":[]}`,
	} {
		role := "assistant"
		if strings.Contains(block, "tool_result") {
			role = "user"
		}
		request := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"` + role + `","content":[` + block + `]}]}`)
		if _, _, err := Request("anthropic", request, "m"); err != nil {
			t.Fatalf("native semantics were enforced locally: %v", err)
		}
		if _, _, err := PrepareRequest("anthropic", "openai", request, "m", 16); err == nil {
			t.Fatal("conversion accepted missing or invalid required tool fields")
		}
	}
	request := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"unpaired","content":"output"}]}]}`)
	if _, _, err := PrepareRequest("anthropic", "openai", request, "m", 16); err == nil || err.Error() != "unsupported_request_feature" {
		t.Fatalf("assistant result was misrepresented during conversion: %v", err)
	}
	for _, block := range []string{
		`{"type":"tool_use","id":"call","name":"read_file","input":[]}`,
		`{"type":"tool_use","name":"read_file","input":{}}`,
		`{"type":"text"}`,
	} {
		response := []byte(`{"type":"message","role":"assistant","content":[` + block + `],"stop_reason":"end_turn"}`)
		if _, err := ValidateResponse("anthropic", response); err != nil {
			t.Fatalf("native block semantics were enforced: %v", err)
		}
		if _, _, err := TranslateResponseWithWarnings("anthropic", "openai", response, "m"); err == nil {
			t.Fatal("response conversion accepted missing or invalid required fields")
		}
	}
	translator := newAnthropicStreamTranslator("m")
	frame := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"read_file\",\"input\":{}}}\n\n")
	if _, err := translator.frame(frame); err == nil {
		t.Fatal("stream conversion emitted a client tool call without an ID")
	}
}
