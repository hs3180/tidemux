package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/diagnostics"
	"github.com/hs3180/tidemux/internal/observability"
)

func TestProviderErrorPolicyAcrossProtocolsAndPersistence(t *testing.T) {
	for _, clientProtocol := range []string{"openai", "anthropic"} {
		for _, providerProtocol := range []string{"openai", "anthropic"} {
			for _, stream := range []bool{false, true} {
				for _, policy := range []string{"default", "true", "false"} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/%s", clientProtocol, providerProtocol, stream, policy), func(t *testing.T) {
						const reason = "prompt is too long: 236663 tokens > 200000 tokens."
						const keyA, keyB = "first-opaque-credential", "second-opaque-credential"
						var posts atomic.Int32
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.Method == "GET" {
								io.WriteString(w, `{"data":[{"id":"custom-model"}]}`)
								return
							}
							posts.Add(1)
							w.Header().Set("request-id", "zai-20261016.trace:fixture")
							message := reason + " " + keyA + " " + keyB + " local-secret raw-client-session sk-unrelated123456 Bearer other-credential\n\u202e"
							body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "code": "context_length_exceeded", "message": message}})
							if !stream {
								w.WriteHeader(400)
								w.Write(body)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							if providerProtocol == "anthropic" {
								io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"fixture","role":"assistant","model":"custom-model","usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
							} else {
								io.WriteString(w, "data: "+`{"id":"fixture","object":"chat.completion.chunk","model":"custom-model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
							}
							w.(http.Flusher).Flush()
							io.WriteString(w, "event: error\ndata: "+string(body)+"\n\n")
						}))
						defer upstream.Close()
						directory := t.TempDir()
						c := namedProviderConfig(testConfig(filepath.Join(directory, "ledger.db"), upstream.URL), "p")
						p := c.Providers["p"]
						p.Protocol, p.APIKey, p.APIKeys = providerProtocol, "", []string{keyA, keyB}
						if policy != "default" {
							p.ForwardErrorMessages = new(policy == "true")
						}
						c.Providers["p"] = p
						var logs bytes.Buffer
						raw, closeDB, err := NewHandlerWithLogger(c, upstream.Client(), observability.JSONLogger(&logs))
						if err != nil {
							t.Fatal(err)
						}
						defer closeDB()
						h := raw.(*handler)
						for attempt := range 2 {
							body := requestBodyFor(clientProtocol, "p", "custom-model")
							if stream {
								body = strings.TrimSuffix(body, "}") + `,"stream":true}`
							}
							response := affinityGatewayRequest(h, clientProtocol, "raw-client-session", body)
							wantStatus := 400
							if stream {
								wantStatus = 200
							}
							if response.Code != wantStatus || posts.Load() != int32(attempt+1) {
								t.Fatalf("status=%d dispatches=%d body=%s", response.Code, posts.Load(), response.Body.String())
							}
							payload := response.Body.String()
							if stream {
								payload = strings.SplitN(strings.SplitN(payload, "event: error\ndata: ", 2)[1], "\n\n", 2)[0]
							}
							var envelope struct {
								Error struct {
									Message, Type, Code string
									ActualTokens        int64 `json:"actual_tokens"`
									LimitTokens         int64 `json:"limit_tokens"`
									Failure             *diagnostics.Failure
								}
							}
							if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
								t.Fatal(err)
							}
							e := envelope.Error
							f := e.Failure
							if f == nil || e.Type != "invalid_request_error" || e.Code != "context_length_exceeded" || e.ActualTokens != 236663 || e.LimitTokens != 200000 || f.ProviderType != "invalid_request_error" || f.ActualTokens != 236663 || f.LimitTokens != 200000 || f.Retryability != "terminal" {
								t.Fatalf("semantics lost: %s", payload)
							}
							if policy == "false" {
								if f.ProviderMessage != "" || strings.Contains(e.Message, reason) {
									t.Fatal("opt-out forwarded text")
								}
							} else if !strings.HasPrefix(e.Message, reason) || !strings.HasPrefix(f.ProviderMessage, reason) {
								t.Fatalf("reason missing from consumable message: %s", payload)
							}
							if f.KeyLabel != h.keyPools["p"].labels[attempt] || f.BackendID != h.keyPools["p"].backendID || f.ProviderRequestID != "zai-20261016.trace:fixture" {
								t.Fatalf("incorrect actual key/backend correlation: %+v", f)
							}
							rows, err := h.ledger.Recent(context.Background(), 10)
							var auditFailure *diagnostics.Failure
							for _, row := range rows {
								if row.ID == response.Header().Get("X-TideMux-Request-ID") {
									auditFailure = row.Failure
									if row.KeyLabel != f.KeyLabel || row.BackendID != f.BackendID || row.Model != "custom-model" {
										t.Fatal("audit attribution lost")
									}
								}
							}
							if err != nil || !reflect.DeepEqual(auditFailure, f) {
								t.Fatalf("SQLite differs from client: %v", err)
							}
							terminal := findRuntimeEvent(decodeRuntimeEvents(t, logs.Bytes()), "request_terminal", response.Header().Get("X-TideMux-Request-ID"))
							encoded, _ := json.Marshal(f)
							var expected map[string]any
							json.Unmarshal(encoded, &expected)
							if terminal == nil || !reflect.DeepEqual(terminal["failure"], expected) || terminal["message"].(map[string]any)["model"] != "custom-model" || terminal["key_label"] != f.KeyLabel || terminal["backend_id"] != f.BackendID {
								t.Fatal("terminal correlation differs")
							}
							var persisted bytes.Buffer
							filepath.WalkDir(filepath.Join(directory, "logs"), func(path string, d os.DirEntry, walkErr error) error {
								if walkErr == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
									data, _ := os.ReadFile(path)
									persisted.Write(data)
								}
								return walkErr
							})
							fileTerminal := findRuntimeEvent(decodeRuntimeEvents(t, persisted.Bytes()), "request_terminal", response.Header().Get("X-TideMux-Request-ID"))
							if !reflect.DeepEqual(fileTerminal, terminal) {
								t.Fatal("session JSONL differs from terminal event")
							}
							all := payload + logs.String() + string(encoded) + persisted.String()
							for _, secret := range []string{keyA, keyB, "local-secret", "raw-client-session", "sk-unrelated", "other-credential", "\u202e"} {
								if strings.Contains(all, secret) {
									t.Fatalf("credential/session/control escaped: %q", secret)
								}
							}
							if stream && (strings.Contains(response.Body.String(), "[DONE]") || strings.Contains(response.Body.String(), "message_stop")) {
								t.Fatal("failed stream emitted a success completion")
							}
						}
					})
				}
			}
		}
	}
}

func TestErrorMessagePolicySerialization(t *testing.T) {
	for _, policy := range []*bool{nil, new(true), new(false)} {
		p := Provider{ForwardErrorMessages: policy}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var restored Provider
		if err = json.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p, restored) {
			t.Fatalf("policy changed: %s", data)
		}
	}
	var p Provider
	if json.Unmarshal([]byte(`{"forward_error_messages":"false"}`), &p) == nil {
		t.Fatal("accepted non-boolean policy")
	}
}

func TestErrorMessagePolicyReloadKeepsAdmittedPolicyAndIdentity(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, `{"data":[{"id":"custom-model"}]}`)
					return
				}
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"message":"prompt is too long: 236663 tokens > 200000"}}`)
			}))
			defer up.Close()
			config := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
			name := "legacy"
			if !legacy {
				name = "p"
				config = namedProviderConfig(config, name)
			}
			config.HealthDiagnostics = new(false)
			raw, closeDB, err := NewHandler(config, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			root := raw.(*handler)
			changed := config
			if legacy {
				changed.ForwardErrorMessages = new(false)
			} else {
				p := config.Providers[name]
				p.ForwardErrorMessages = new(false)
				changed.Providers = map[string]Provider{name: p}
			}
			if !reflect.DeepEqual(runtimeSettings(config), runtimeSettings(changed)) {
				t.Fatal("policy change incorrectly requires a restart")
			}
			view, err := root.prepareConfigView(context.Background(), changed, config, root, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			if root.clients[name].ForwardErrorMessages != nil || view.clients[name].ForwardErrorMessages == nil || *view.clients[name].ForwardErrorMessages || view.keyPools[name] != root.keyPools[name] || view.clients[name].BackendID != root.clients[name].BackendID {
				t.Fatal("policy reload reset identity or mutated admitted client")
			}
			body := requestBodyFor("openai", name, "custom-model")
			for i, h := range []*handler{root, view} {
				response := affinityGatewayRequest(h, "openai", "session", body)
				if response.Code != 400 || strings.Contains(response.Body.String(), "prompt is too long") != (i == 0) {
					t.Fatalf("admitted policy changed: %s", response.Body.String())
				}
			}
			rows, err := root.ledger.Recent(context.Background(), 10)
			if err != nil || len(rows) != 2 || rows[0].KeyLabel == "" || rows[1].KeyLabel != rows[0].KeyLabel || rows[0].BackendID != rows[1].BackendID {
				t.Fatalf("disabled counters lost attribution: %+v %v", rows, err)
			}
			if root.keyPools[name].diagnosticStatus(time.Now()).TelemetryEnabled {
				t.Fatal("test did not disable optional counters")
			}
		})
	}
}
