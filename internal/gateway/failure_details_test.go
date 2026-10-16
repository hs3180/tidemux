package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hs3180/tidemux/internal/diagnostics"
	"github.com/hs3180/tidemux/internal/observability"
)

func TestUpstreamFailureDetailsJoinClientLogAndAudit(t *testing.T) {
	for _, clientProtocol := range []string{"openai", "anthropic"} {
		for _, providerProtocol := range []string{"openai", "anthropic"} {
			for _, mode := range []string{"http_400", "stream_503", "stream_redacted"} {
				t.Run(clientProtocol+"/"+providerProtocol+"/"+mode, func(t *testing.T) {
					var posts atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == "GET" {
							io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
							return
						}
						posts.Add(1)
						w.Header().Set("request-id", "req_fixture12345678")
						if mode == "http_400" {
							w.WriteHeader(400)
							io.WriteString(w, `{"error":{"code":1210,"param":"/private/parameter","message":"PRIVATE_UPSTREAM_DETAILS"}}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						if providerProtocol == "anthropic" {
							io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"fixture-msg","role":"assistant","model":"custom-model","usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
						} else {
							io.WriteString(w, "data: "+`{"id":"fixture-msg","object":"chat.completion.chunk","created":1,"model":"custom-model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
						}
						w.(http.Flusher).Flush()
						code := "1308"
						if mode == "stream_redacted" {
							code = "private_account_identifier"
						}
						data, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"code": code, "status": 503, "message": "PRIVATE_UPSTREAM_DETAILS"}})
						io.WriteString(w, "event: error\ndata: "+string(data)+"\n\n")
					}))
					defer upstream.Close()
					c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL+"/v1"), "p")
					p := c.Providers["p"]
					p.Protocol = providerProtocol
					c.Providers["p"] = p
					var logs bytes.Buffer
					raw, closeDB, err := NewHandlerWithLogger(c, upstream.Client(), observability.JSONLogger(&logs))
					if err != nil {
						t.Fatal(err)
					}
					defer closeDB()
					body := requestBodyFor(clientProtocol, "p", "custom-model")
					if mode != "http_400" {
						body = strings.TrimSuffix(body, "}") + `,"stream":true}`
					}
					w := affinityGatewayRequest(raw, clientProtocol, "private-session-marker", body)
					wantStatus := 200
					if mode == "http_400" {
						wantStatus = 400
					}
					if w.Code != wantStatus || posts.Load() != 1 {
						t.Fatalf("status=%d posts=%d body=%s", w.Code, posts.Load(), w.Body.String())
					}
					payload := w.Body.String()
					if mode != "http_400" {
						payload = strings.SplitN(strings.SplitN(payload, "event: error\ndata: ", 2)[1], "\n\n", 2)[0]
					}
					var envelope struct {
						RequestID string `json:"request_id"`
						Error     struct {
							Failure map[string]any `json:"failure"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
						t.Fatal(err)
					}
					id := w.Header().Get("X-TideMux-Request-ID")
					if id == "" || mode != "http_400" && envelope.RequestID != id {
						t.Fatal("client lost its gateway request ID")
					}
					h := raw.(*handler)
					rows, err := h.ledger.Recent(context.Background(), 10)
					if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Failure == nil {
						t.Fatalf("audit=%+v err=%v", rows, err)
					}
					encoded, _ := json.Marshal(rows[0].Failure)
					var audit map[string]any
					json.Unmarshal(encoded, &audit)
					terminal := findRuntimeEvent(decodeRuntimeEvents(t, logs.Bytes()), "request_terminal", id)
					if terminal == nil || !reflect.DeepEqual(audit, terminal["failure"]) || !reflect.DeepEqual(audit, envelope.Error.Failure) {
						t.Fatalf("failure differs: audit=%s client=%+v terminal=%+v", encoded, envelope.Error.Failure, terminal)
					}
					f := rows[0].Failure
					if f.ProviderRequestID != "req_fixture12345678" {
						t.Fatal("provider correlation ID was lost")
					}
					if mode == "http_400" {
						if f.Source != "http_error" || f.HTTPStatus != 400 || f.ProviderCode != "1210" || f.Retryability != "terminal" {
							t.Fatalf("HTTP failure=%+v", f)
						}
					} else {
						if f.Source != "stream_error" || f.HTTPStatus != 200 || f.ErrorStatus != 503 || f.Phase != "stream" || f.Retryability != "retryable" || terminal["outcome"] != "error" {
							t.Fatalf("stream failure=%+v", f)
						}
						if mode == "stream_503" && f.ProviderCode != "1308" || mode == "stream_redacted" && (f.ProviderCode != "" || f.CodeState != "redacted") {
							t.Fatalf("code retention=%+v", f)
						}
						if f.ResponseRetryHeaders == nil || !reflect.DeepEqual(f.ResponseRetryHeaders, &diagnostics.RetryHeaders{}) {
							t.Fatal("open stream claimed retry headers that were never sent")
						}
					}
					all := logs.String() + string(encoded) + w.Body.String()
					for _, secret := range []string{"PRIVATE_UPSTREAM_DETAILS", "/private/parameter", "private_account_identifier", "private-session-marker", "provider-secret", "local-secret"} {
						if strings.Contains(all, secret) {
							t.Fatalf("sensitive value leaked: %s", secret)
						}
					}
				})
			}
		}
	}
}
