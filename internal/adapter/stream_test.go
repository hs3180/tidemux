package adapter

import (
	"errors"
	"strings"
	"testing"
)

func openStream() string {
	return "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
}
func anthStream() string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1,\"cache_read_input_tokens\":3}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}
func TestStreamUsageAndTerminalHold(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			data := openStream()
			wantInput := int64(10)
			end := "[DONE]"
			if protocol == "anthropic" {
				data = anthStream()
				wantInput = 8
				end = "message_stop"
			}
			var emitted strings.Builder
			u, terminal, err := readStream(protocol, strings.NewReader(data), func(b []byte) error { emitted.Write(b); return nil })
			if err != nil || u.Input == nil || *u.Input != wantInput || *u.Output != 2 {
				t.Fatalf("%+v %v", u, err)
			}
			if strings.Contains(emitted.String(), end) || !strings.Contains(string(terminal), end) {
				t.Fatal("terminal was not held for audit")
			}
			if emitted.String()+string(terminal) != data {
				t.Fatal("changed SSE payload")
			}
		})
	}
}
func TestStreamFailureAndUnknownUsage(t *testing.T) {
	cases := []struct{ p, s string }{
		{"openai", strings.Replace(openStream(), "data: [DONE]\n\n", "", 1)},
		{"anthropic", strings.Replace(anthStream(), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1)},
		{"openai", "data: [DONE]\n\n"},
		{"openai", "data: {\"error\":{\"message\":\"secret-provider-error\"}}\n\n"},
		{"anthropic", "data: {\"type\":\"error\",\"error\":{\"message\":\"secret-provider-error\"}}\n\n"},
		{"openai", strings.Replace(openStream(), "\"prompt_tokens\":10", "\"prompt_tokens\":-1", 1)},
		{"openai", "data: bad-json\n\n"},
	}
	for _, c := range cases {
		var out strings.Builder
		u, _, err := readStream(c.p, strings.NewReader(c.s), func(b []byte) error { out.Write(b); return nil })
		if err == nil || u.Input != nil || u.Output != nil || strings.Contains(out.String(), "secret-provider-error") {
			t.Fatalf("%s %+v %v", c.p, u, err)
		}
	}
	data := strings.Replace(openStream(), ",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}", "", 1)
	u, _, err := readStream("openai", strings.NewReader(data), func([]byte) error { return nil })
	if err != nil || u.Input != nil || u.Output != nil {
		t.Fatalf("unknown usage %+v %v", u, err)
	}
}
func TestStreamStopsOnDownstreamError(t *testing.T) {
	want := errors.New("closed")
	_, _, err := readStream("openai", strings.NewReader(openStream()), func([]byte) error { return want })
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func TestStreamPreservesCRLFAndCountsWireBytes(t *testing.T) {
	data := strings.ReplaceAll(openStream(), "\n", "\r\n")
	for _, allowed := range []bool{true, false} {
		limit := int64(len(data))
		if !allowed {
			limit--
		}
		var emitted strings.Builder
		u, terminal, err := readStreamWithLimits("openai", strings.NewReader(data), Limits{StreamBytes: limit, EventBytes: len(data)}, func(b []byte) error { emitted.Write(b); return nil })
		if allowed {
			if err != nil || emitted.String()+string(terminal) != data || u.Input == nil {
				t.Fatalf("CRLF changed or rejected: %v", err)
			}
		} else if err == nil || terminal != nil || u.Input != nil {
			t.Fatal("wire limit bypassed by CRLF")
		}
	}
}

func TestStreamCannotSpoofTerminalEvent(t *testing.T) {
	cases := []struct{ protocol, data string }{
		{"anthropic", strings.Replace(anthStream(), "event: message_delta", "event: message_stop", 1)},
		{"openai", "event: error\n" + openStream()},
		{"openai", strings.Replace(openStream(), `"delta":{"content":"OK"}`, `"delta":null`, 1)},
		{"openai", strings.Replace(openStream(), `"delta":{"content":"OK"}`, `"delta":"bad"`, 1)},
	}
	for _, tc := range cases {
		u, terminal, err := readStream(tc.protocol, strings.NewReader(tc.data), func([]byte) error { return nil })
		if err == nil || terminal != nil || u.Input != nil {
			t.Fatal("invalid stream accepted")
		}
	}
}
