package adapter

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestToolRoundTrip(t *testing.T) {
	cases := map[string]string{
		"openai":    `{"model":"m","tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}},"strict":true}}],"tool_choice":"auto","messages":[{"role":"user","content":"read it"},{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"read_file","arguments":"{}"}}],"reasoning_content":"inspect file"},{"role":"tool","tool_call_id":"call1","content":"OK"}]}`,
		"anthropic": `{"model":"m","max_tokens":128,"system":[{"type":"text","text":"Read files","cache_control":{"type":"ephemeral","ttl":"5m"}}],"tools":[{"name":"read_file","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"messages":[{"role":"user","content":"read it"},{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"read_file","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":[{"type":"text","text":"OK"}],"is_error":false}]}]}`,
	}
	for protocol, body := range cases {
		t.Run(protocol, func(t *testing.T) {
			encoded, model, err := Request(protocol, []byte(body), "")
			if err != nil || model != "m" {
				t.Fatalf("%s %v", model, err)
			}
			var before, after map[string]any
			json.Unmarshal([]byte(body), &before)
			json.Unmarshal(encoded, &after)
			for k, v := range before {
				if !reflect.DeepEqual(v, after[k]) {
					t.Fatalf("changed %s", k)
				}
			}
		})
	}
}
func TestToolResponseAllowsNoText(t *testing.T) {
	for protocol, body := range map[string]string{
		"openai":    `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		"anthropic": `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"c1","name":"read_file","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":2}}`,
	} {
		u, err := ValidateResponse(protocol, []byte(body))
		if err != nil || u.Input == nil || *u.Input != 10 || *u.Output != 2 {
			t.Fatalf("%s: %+v %v", protocol, u, err)
		}
	}
}

func TestEagerInputStreamingHintIsAcceptedAndDropped(t *testing.T) {
	body := `{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"use a tool"}],"tools":[{"name":"read_file","input_schema":{"type":"object"},"eager_input_streaming":true}]}`
	encoded, model, err := Request("anthropic", []byte(body), "")
	if err != nil || model != "m" {
		t.Fatalf("model=%q err=%v", model, err)
	}
	if bytes.Contains(encoded, []byte("eager_input_streaming")) {
		t.Fatalf("client-only hint was forwarded: %s", encoded)
	}
}

func TestMalformedToolMessagesRejected(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"tool","content":"missing ID"}]}`,
		`{"messages":[{"role":"user","content":"wrong role","tool_call_id":"c1"}]}`,
		`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"","arguments":"{}"}}]}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":[]}}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"anything"}}`,
		`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","type":"array"}}}]}`,
	} {
		if _, _, err := Request("openai", []byte(body), "m"); err == nil {
			t.Fatal(body)
		}
	}
	for _, block := range []string{
		`{"type":"tool_use","id":"c1","name":"f","input":{}}`,
		`{"type":"tool_result","tool_use_id":"c1","content":[{"type":"tool_use","id":"c2","name":"f","input":{}}]}`,
		`{"type":"text","text":"hi","cache_control":{"type":"invalid"}}`,
	} {
		body := `{"max_tokens":10,"messages":[{"role":"user","content":[` + block + `]}]}`
		if _, _, err := Request("anthropic", []byte(body), "m"); err == nil {
			t.Fatal(block)
		}
	}
}

func TestAnthropicCompatibleSystemRolePreserved(t *testing.T) {
	body := `{"model":"m","max_tokens":100,"system":"top-level","messages":[{"role":"user","content":"question"},{"role":"system","content":[{"type":"text","text":"client instruction","cache_control":{"type":"ephemeral"}}]}]}`
	encoded, _, err := Request("anthropic", []byte(body), "")
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	json.Unmarshal([]byte(body), &before)
	json.Unmarshal(encoded, &after)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rewrote message roles, content, or order")
	}
}
