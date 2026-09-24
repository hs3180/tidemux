package adapter

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestRequestIgnoresUnknownFieldsAndReportsIgnoredPaths(t *testing.T) {
	body := []byte(`{"model":"m","store":true,"messages":[{"role":"user","content":"use a tool","client_extension":{"trace":true}}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}},"eager_input_streaming":true}]}`)
	encoded, model, ignored, err := RequestWithWarnings("openai", body, "")
	if err != nil || model != "m" {
		t.Fatalf("model=%q err=%v", model, err)
	}
	want := []string{"messages[0].client_extension", "store", "tools[0].eager_input_streaming"}
	if !reflect.DeepEqual(ignored, want) {
		t.Fatalf("ignored=%v want=%v", ignored, want)
	}
	if bytes.Contains(encoded, []byte("store")) || bytes.Contains(encoded, []byte("eager_input_streaming")) || bytes.Contains(encoded, []byte("client_extension")) {
		t.Fatalf("ignored fields survived normalization: %s", encoded)
	}
}

func TestRequestTypeErrorIncludesParameter(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":"not-a-number"}`)
	if _, _, err := Request("openai", body, ""); err == nil || ValidationParameter(err) != "temperature" {
		t.Fatalf("err=%v param=%q", err, ValidationParameter(err))
	}
}

func TestRequestDropsKnownFieldsFromOtherProtocol(t *testing.T) {
	tests := []struct {
		protocol string
		body     string
		want     []string
	}{
		{
			protocol: "anthropic",
			body:     `{"model":"m","max_tokens":20,"messages":[{"role":"user","content":"hi","reasoning_content":{"invalid":"type"},"tool_calls":"invalid"}],"reasoning_effort":123,"response_format":"invalid","stop":{"invalid":"type"},"stream_options":false,"parallel_tool_calls":"invalid","user":{"invalid":"type"},"dsh_plugin_packages":["example"]}`,
			want:     []string{"dsh_plugin_packages", "messages[0].reasoning_content", "messages[0].tool_calls", "parallel_tool_calls", "reasoning_effort", "response_format", "stop", "stream_options", "user"},
		},
		{
			protocol: "openai",
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"system":123,"stop_sequences":"invalid","metadata":false,"output_config":"invalid","context_management":[]}`,
			want:     []string{"context_management", "metadata", "output_config", "stop_sequences", "system"},
		},
	}
	for _, test := range tests {
		t.Run(test.protocol, func(t *testing.T) {
			encoded, _, ignored, err := RequestWithWarnings(test.protocol, []byte(test.body), "")
			if err != nil {
				t.Fatalf("request rejected: %v", err)
			}
			if !reflect.DeepEqual(ignored, test.want) {
				t.Fatalf("ignored=%v want=%v", ignored, test.want)
			}
			for _, field := range test.want {
				fieldName := field[strings.LastIndex(field, ".")+1:]
				if bytes.Contains(encoded, []byte(`"`+fieldName+`":`)) {
					t.Fatalf("foreign field %q survived normalization: %s", field, encoded)
				}
			}
		})
	}
}

func TestRequestDropsUnknownContentBlockFieldsRecursively(t *testing.T) {
	tests := []struct {
		protocol string
		body     string
		want     []string
	}{
		{
			protocol: "openai",
			body:     `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"},"client_extension":{"trace":true}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"lookup","client_option":1},"future_option":true}}`,
			want:     []string{"messages[0].content[0].cache_control", "messages[0].content[0].client_extension", "tool_choice.function.client_option", "tool_choice.future_option"},
		},
		{
			protocol: "anthropic",
			body:     `{"model":"m","max_tokens":16,"system":[{"type":"text","text":"rules","future_option":true}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"lookup","input":{"n":9007199254740993},"future_option":true}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":[{"type":"text","text":"result","future_option":true}]}]}],"tool_choice":{"type":"auto","future_option":true}}`,
			want:     []string{"messages[0].content[0].future_option", "messages[1].content[0].content[0].future_option", "system[0].future_option", "tool_choice.future_option"},
		},
	}
	for _, test := range tests {
		t.Run(test.protocol, func(t *testing.T) {
			encoded, _, ignored, err := RequestWithWarnings(test.protocol, []byte(test.body), "")
			if err != nil {
				t.Fatalf("request rejected: %v", err)
			}
			if !reflect.DeepEqual(ignored, test.want) {
				t.Fatalf("ignored=%v want=%v", ignored, test.want)
			}
			if bytes.Contains(encoded, []byte("cache_control")) || bytes.Contains(encoded, []byte("client_extension")) || bytes.Contains(encoded, []byte("client_option")) || bytes.Contains(encoded, []byte("future_option")) {
				t.Fatalf("unknown content block fields survived normalization: %s", encoded)
			}
			if test.protocol == "anthropic" && !bytes.Contains(encoded, []byte(`9007199254740993`)) {
				t.Fatalf("opaque tool input number changed during normalization: %s", encoded)
			}
		})
	}
}
