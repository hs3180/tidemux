package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/diagnostics"
	"github.com/hs3180/tidemux/internal/observability"
)

func zaiTestMappings() []adapter.ProviderErrorMapping {
	return []adapter.ProviderErrorMapping{
		{UpstreamCode: "1308", HTTPStatus: 429, Category: adapter.ProviderErrorQuotaExhausted},
		{UpstreamCode: "1310", HTTPStatus: 429, Category: adapter.ProviderErrorQuotaExhausted},
		{UpstreamCode: "1311", HTTPStatus: 429, Category: adapter.ProviderErrorAccessDenied},
		{UpstreamCode: "1313", HTTPStatus: 429, Category: adapter.ProviderErrorFairUseThrottled},
	}
}

func TestZaiConfiguredErrorsPauseProviderAndRecoverOnlyOnNewRequests(t *testing.T) {
	for _, cp := range []string{"openai", "anthropic"} {
		for _, pp := range []string{"openai", "anthropic"} {
			for _, stream := range []bool{false, true} {
				for _, mapping := range zaiTestMappings() {
					for _, route := range []string{"explicit", "shared", "auto"} {
						t.Run(fmt.Sprintf("%s/%s/%t/%s/%s", cp, pp, stream, mapping.UpstreamCode, route), func(t *testing.T) {
							var a, b atomic.Int32
							up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								if r.Method == "GET" {
									io.WriteString(w, `{"data":[{"id":"one"},{"id":"two"}]}`)
									return
								}
								if strings.HasPrefix(r.URL.Path, "/b/") {
									b.Add(1)
									if pp == "openai" {
										io.WriteString(w, openAIResponseForModel("one"))
									} else {
										io.WriteString(w, strings.ReplaceAll(responseBody("anthropic"), "custom-model", "one"))
									}
									return
								}
								a.Add(1)
								w.Header().Set("x-should-retry", "PRIVATE_HEADER")
								errBody := fmt.Sprintf(`{"type":"error","error":{"status":429,"code":%s,"message":"PRIVATE_PROVIDER_TEXT provider-secret local-secret"}}`, mapping.UpstreamCode)
								if !stream {
									w.WriteHeader(429)
									io.WriteString(w, errBody)
									return
								}
								w.Header().Set("Content-Type", "text/event-stream")
								if pp == "anthropic" {
									io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"fixture","role":"assistant","model":"one","usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
								} else {
									io.WriteString(w, "data: "+`{"id":"fixture","object":"chat.completion.chunk","model":"one","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
								}
								w.(http.Flusher).Flush()
								io.WriteString(w, "event: error\ndata: "+errBody+"\n\n")
							}))
							defer up.Close()
							c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
							for _, ref := range []string{"a", "b"} {
								addRoutingProvider(&c, ref, up.URL+"/"+ref, pp, "one", "two")
							}
							p := c.Providers["a"]
							p.APIKeys = []string{"provider-secret", "other-provider-secret"}
							p.APIKey = ""
							p.ForwardErrorMessages = new(false)
							p.ErrorCodeMappings = zaiTestMappings()
							c.Providers["a"] = p
							configureRouting(&c, RoutingConfig{SharedModelStrategy: "random"})
							if route == "auto" {
								c.AutoChain = []AutoChainEntry{{Provider: "a", Model: "one"}, {Provider: "b", Model: "one"}}
							}
							var logs bytes.Buffer
							raw, closeDB, err := NewHandlerWithLogger(c, up.Client(), observability.JSONLogger(&logs))
							if err != nil {
								t.Fatal(err)
							}
							defer closeDB()
							h := raw.(*handler)
							h.randomIndex = func(int) int { return 0 }
							model := "one"
							if route == "auto" {
								model = "auto"
							}
							if route == "explicit" {
								model = "a/one"
							}
							body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"stream":%t,"messages":[{"role":"user","content":"fixture"}]}`, model, stream)
							first := affinityGatewayRequest(h, cp, "same-session", body)
							wantStatus := 429
							if mapping.Category == adapter.ProviderErrorAccessDenied {
								wantStatus = 403
							}
							if stream {
								wantStatus = 200
							}
							if first.Code != wantStatus || a.Load() != 1 || b.Load() != 0 {
								t.Fatalf("first request dispatched more than once: %d/%d status=%d body=%s", a.Load(), b.Load(), first.Code, first.Body.String())
							}
							payload := first.Body.String()
							if stream {
								payload = strings.SplitN(strings.SplitN(payload, "event: error\ndata: ", 2)[1], "\n\n", 2)[0]
							}
							var envelope struct {
								Error struct {
									Type, Message string
									Failure       *diagnostics.Failure
								}
							}
							if err = json.Unmarshal([]byte(payload), &envelope); err != nil {
								t.Fatal(err)
							}
							f := envelope.Error.Failure
							if f == nil || f.Category != string(mapping.Category) || f.Retryability != "terminal" || f.CooldownScope != "provider" || f.ProviderCode != mapping.UpstreamCode || !f.ConfiguredCode {
								t.Fatalf("classification lost: %s", payload)
							}
							if mapping.Category == adapter.ProviderErrorAccessDenied && envelope.Error.Type != "permission_error" {
								t.Fatal("entitlement was treated as quota")
							}
							if mapping.Category == adapter.ProviderErrorFairUseThrottled && envelope.Error.Type != "rate_limit_error" {
								t.Fatal("fair use lost the native rate-limit type")
							}
							if mapping.Category == adapter.ProviderErrorFairUseThrottled {
								for _, text := range []string{"fair use", "if the unexpected load is not yours", "rotate", "manual removal", "unknown"} {
									if !strings.Contains(envelope.Error.Message, text) {
										t.Fatalf("conditional recovery guidance missing: %s", payload)
									}
								}
							}
							if !stream {
								signal := "true"
								if route == "explicit" {
									signal = "false"
								}
								if first.Header().Get("x-should-retry") != signal {
									t.Fatalf("retry decision=%v", first.Header())
								}
								if route == "explicit" && first.Header().Get("Retry-After") != "" {
									t.Fatal("terminal failure invented a reset wait")
								}
							}
							rows, err := h.ledger.Recent(context.Background(), 10)
							if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].Failure, f) {
								t.Fatalf("audit differs: %v", err)
							}
							terminal := findRuntimeEvent(decodeRuntimeEvents(t, logs.Bytes()), "request_terminal", rows[0].ID)
							encoded, _ := json.Marshal(f)
							var expected map[string]any
							json.Unmarshal(encoded, &expected)
							if terminal == nil || !reflect.DeepEqual(terminal["failure"], expected) {
								t.Fatal("JSONL differs")
							}
							entry := h.availability.entries[availabilityTarget{"a", "", h.providerGenerations["a"]}]
							if entry == nil || entry.reason != string(mapping.Category) || time.Until(entry.retryAt) < 299*time.Second || time.Until(entry.retryAt) > 361*time.Second {
								t.Fatalf("provider did not enter bounded request-driven recovery: %+v", entry)
							}
							other := affinityGatewayRequest(h, cp, "different-session", `{"model":"a/two","max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`)
							localStatus, localCode, localMessage, localType := mapping.Category.ClientError(cp)
							var localEnvelope struct {
								Error struct{ Code, Type, Message string }
							}
							if err := json.Unmarshal(other.Body.Bytes(), &localEnvelope); err != nil {
								t.Fatal(err)
							}
							if other.Code != localStatus || localEnvelope.Error.Code != localCode+"_cooling_down" || localEnvelope.Error.Type != localType || localEnvelope.Error.Message != localMessage || other.Header().Get("x-should-retry") != "false" || other.Header().Get("Retry-After") != "" || a.Load() != 1 {
								t.Fatalf("cooling provider or another key/model was dispatched: %s", other.Body.String())
							}
							locals, err := h.ledger.RecentDiagnostics(context.Background(), 10)
							if err != nil || len(locals) != 1 || locals[0].Status != localStatus || locals[0].ErrorCode != localCode+"_cooling_down" {
								t.Fatalf("local terminal SQLite outcome differs: %+v %v", locals, err)
							}
							localEvent := findRuntimeEvent(decodeRuntimeEvents(t, logs.Bytes()), "local_rejection", locals[0].ID)
							if localEvent == nil || localEvent["http_status"] != float64(localStatus) || localEvent["error_code"] != localCode+"_cooling_down" {
								t.Fatalf("local terminal JSONL outcome differs: %+v", localEvent)
							}
							if mapping.Category == adapter.ProviderErrorFairUseThrottled && !strings.Contains(other.Body.String(), "manual removal") {
								t.Fatal("local fair-use rejection lost recovery instructions")
							}
							if route != "explicit" {
								nextBody := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`, model)
								next := affinityGatewayRequest(h, cp, "same-session", nextBody)
								if next.Code != 200 || a.Load() != 1 || b.Load() != 1 {
									t.Fatalf("new request did not use healthy alternative: %s", next.Body.String())
								}
							}
							for _, secret := range []string{"PRIVATE_", "provider-secret", "other-provider-secret", "local-secret"} {
								if strings.Contains(first.Body.String()+other.Body.String()+logs.String(), secret) {
									t.Fatalf("strict privacy regression: %s", secret)
								}
							}
						})
					}
				}
			}
		}
	}
}

func TestFairUseWarningSurvivesEnabledMessageForwarding(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		callErr := &adapter.CallError{Category: adapter.ProviderErrorFairUseThrottled, Failure: &diagnostics.Failure{Source: "http_error", ProviderMessage: "Provider denied unusual usage. Bearer secret-shape"}}
		payload := mappedUpstreamErrorPayload(callErr, protocol)
		detail := payload["error"].(map[string]any)
		addProviderErrorSemantics(detail, callErr, protocol)
		message := detail["message"].(string)
		if !strings.Contains(message, "Provider denied unusual usage.") || !strings.Contains(message, "if the unexpected load is not yours") || strings.Contains(message, "secret-shape") {
			t.Fatalf("warning or privacy lost: %s", message)
		}
	}
}
