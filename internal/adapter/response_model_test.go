package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSetResponseModelUsesSelectedModelForBothProtocols(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			input := `{"id":"response-1","model":"provider-alias","extra":"kept"}`
			got := setResponseModel(protocol, []byte(input), "selected-model")
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatal(err)
			}
			var model, extra string
			_ = json.Unmarshal(payload["model"], &model)
			_ = json.Unmarshal(payload["extra"], &extra)
			if model != "selected-model" || extra != "kept" {
				t.Fatalf("response=%s model=%q extra=%q", got, model, extra)
			}
		})
	}
}

func TestSetStreamResponseModelPreservesNativeSSEFraming(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		frame    string
		want     string
	}{
		{
			name: "openai LF", protocol: "openai",
			frame: "data: {\"id\":\"chunk\",\"model\":\"provider-alias\",\"choices\":[]}\n\n",
			want:  "selected-model",
		},
		{
			name: "anthropic CRLF", protocol: "anthropic",
			frame: "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"provider-alias\"}}\r\n\r\n",
			want:  "selected-model",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := setStreamResponseModel(test.protocol, []byte(test.frame), "selected-model")
			if test.protocol == "anthropic" && !strings.Contains(string(got), "\r\n") {
				t.Fatalf("CRLF framing was lost: %q", got)
			}
			_, data, err := parseSSEFrame(got)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatal(err)
			}
			modelRaw := payload["model"]
			if test.protocol == "anthropic" {
				var message map[string]json.RawMessage
				if err := json.Unmarshal(payload["message"], &message); err != nil {
					t.Fatal(err)
				}
				modelRaw = message["model"]
			}
			var model string
			if err := json.Unmarshal(modelRaw, &model); err != nil || model != test.want {
				t.Fatalf("frame=%q model=%q err=%v", got, model, err)
			}
		})
	}
}
