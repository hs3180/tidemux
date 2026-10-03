package adapter

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPoisonedToolHistoryHasExactPath(t *testing.T) {
	for _, compacted := range []bool{false, true} {
		prefix := `{"role":"user","content":"read a page"},`
		if compacted {
			prefix = `{"role":"assistant","content":[{"type":"compaction","content":"summary"}]},`
		}
		body := []byte(`{"model":"m","max_tokens":16,"messages":[` + prefix + `{"role":"assistant","content":[{"type":"text","text":"earlier"},{"type":"tool_result","tool_use_id":"private-id","content":[{"type":"text","text":"private-web-or-image-result"}]}]},{"role":"user","content":"continue"}]}`)
		for _, provider := range []string{"anthropic", "openai"} {
			_, _, err := PrepareRequest("anthropic", provider, body, "m", 16)
			// Same-protocol PrepareRequest assumes a gateway-validated request.
			if provider == "anthropic" {
				_, _, err = Request("anthropic", body, "m")
			}
			if err == nil || err.Error() != "invalid_tool_history" || ValidationParameter(err) != "messages[1].content[1].type" {
				t.Fatalf("compacted=%t provider=%s err=%v param=%s", compacted, provider, err, ValidationParameter(err))
			}
		}
	}
}

func TestMalformedToolResponseHasExactPath(t *testing.T) {
	data := []byte(`{"type":"message","role":"assistant","content":[{"type":"tool_result","tool_use_id":"private-id","content":"private-web-result"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	_, err := ValidateResponse("anthropic", data)
	if err == nil || err.Error() != "invalid_upstream_tool_history" || ValidationParameter(err) != "content[0].type" {
		t.Fatalf("err=%v", err)
	}
}

func TestStreamWithholdsMalformedToolBlock(t *testing.T) {
	for _, location := range []string{"start", "block"} {
		message := `{"role":"assistant","usage":{"input_tokens":2}}`
		block := ""
		if location == "start" {
			message = `{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"private-id","content":"private-web-result"}],"usage":{"input_tokens":2}}`
		}
		if location == "block" {
			block = "event: content_block_start\ndata: " + `{"type":"content_block_start","index":4,"content_block":{"type":"tool_result","tool_use_id":"private-id","content":"private-image-result"}}` + "\n\n"
		}
		stream := "event: message_start\ndata: " + fmt.Sprintf(`{"type":"message_start","message":%s}`, message) + "\n\n" + block + "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		var emitted strings.Builder
		_, terminal, err := readStream("anthropic", strings.NewReader(stream), func(frame []byte) error { emitted.Write(frame); return nil })
		var failure *CallError
		if !errors.As(err, &failure) || failure.Code != "invalid_upstream_tool_history" || failure.Param == "" || terminal != nil || strings.Contains(emitted.String(), "private-") {
			t.Fatalf("location=%s err=%v malformed frame escaped=%t", location, err, strings.Contains(emitted.String(), "private-"))
		}
	}
}
