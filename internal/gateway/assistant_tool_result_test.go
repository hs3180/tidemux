package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/observability"
)

func TestAssistantToolResultPolicyValidationAndRoundTrip(t *testing.T) {
	c := routingTestConfig(filepath.Join(t.TempDir(), "audit.db"))
	addRoutingProvider(&c, "glm", "https://example.invalid", "anthropic", "glm-5.3")
	for _, policy := range []string{"", "reject", "strip_redundant", "strip", "arbitrary"} {
		provider := c.Providers["glm"]
		provider.AssistantToolResultPolicy = policy
		c.Providers["glm"] = provider
		err := c.Validate()
		if (policy == "strip" || policy == "arbitrary") != (err != nil) {
			t.Fatalf("policy=%s err=%v", policy, err)
		}
		if err == nil {
			encoded, _ := json.Marshal(c)
			var decoded Config
			_ = json.Unmarshal(encoded, &decoded)
			if decoded.Providers["glm"].AssistantToolResultPolicy != policy {
				t.Fatal("policy lost during config write")
			}
		}
	}
	provider := c.Providers["glm"]
	provider.AssistantToolResultPolicy, provider.Protocol = "strip_redundant", "openai"
	c.Providers["glm"] = provider
	if c.Validate() == nil {
		t.Fatal("Anthropic compatibility policy applied to OpenAI upstream")
	}
}

func TestMalformedToolStreamErrorCarriesAuditedRequestID(t *testing.T) {
	for _, protocol := range []string{"anthropic", "openai"} {
		t.Run(protocol, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					_, _ = io.WriteString(w, `{"data":[{"id":"model","type":"model"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"prefix\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_result\",\"tool_use_id\":\"private-id\",\"content\":\"private-result\"}}\n\n")
			}))
			defer upstream.Close()
			c := routingTestConfig(filepath.Join(t.TempDir(), "audit.db"))
			addRoutingProvider(&c, "glm", upstream.URL, "anthropic", "model")
			var logs bytes.Buffer
			h, closeHandler, err := NewHandlerWithLogger(c, upstream.Client(), observability.JSONLogger(&logs))
			if err != nil {
				t.Fatal(err)
			}
			defer closeHandler()
			w := affinityGatewayRequest(h, protocol, "session", `{"model":"glm/model","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
			id := w.Header().Get("X-TideMux-Request-ID")
			if id == "" || w.Code != 200 || !strings.Contains(w.Body.String(), `"request_id":"`+id+`"`) || !strings.Contains(logs.String(), `"requestId":"`+id+`"`) {
				t.Fatalf("uncorrelated SSE error: status=%d id=%s", w.Code, id)
			}
			if strings.Contains(w.Body.String()+logs.String(), "private-") {
				t.Fatal("malformed tool payload leaked")
			}
		})
	}
}
