package gateway

import (
	"bytes"
	"context"
	"database/sql"
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
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/observability"
)

func TestAuditFailureReplacesRetryAdviceAndTerminalOutcome(t *testing.T) {
	for _, providerProtocol := range []string{"openai", "anthropic"} {
		for _, protocol := range []string{"openai", "anthropic"} {
			for _, stream := range []bool{false, true} {
				for _, rateLimited := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/rate=%t", providerProtocol, protocol, stream, rateLimited), func(t *testing.T) {
						var calls atomic.Int32
						up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.Method == "GET" {
								io.WriteString(w, `{"data":[{"id":"custom-model"}]}`)
								return
							}
							calls.Add(1)
							if rateLimited {
								w.Header().Set("Retry-After", "3600")
							}
							if stream {
								w.Header().Set("Content-Type", "text/event-stream")
								if providerProtocol == "anthropic" {
									io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"role":"assistant","model":"custom-model","usage":{"input_tokens":3,"output_tokens":0}}}`+"\n\n")
									w.(http.Flusher).Flush()
									if rateLimited {
										io.WriteString(w, "event: error\ndata: "+`{"type":"error","error":{"type":"rate_limit_error","status":429,"message":"Bearer PRIVATE_PROVIDER_ERROR"}}`+"\n\n")
									} else {
										io.WriteString(w, "event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`+"\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
									}
									return
								}
								io.WriteString(w, "data: "+`{"id":"fixture","model":"custom-model","choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n")
								w.(http.Flusher).Flush()
								if rateLimited {
									io.WriteString(w, "event: error\ndata: "+`{"error":{"type":"rate_limit_error","status":429,"message":"Bearer PRIVATE_PROVIDER_ERROR"}}`+"\n\n")
								} else {
									io.WriteString(w, "data: "+`{"id":"fixture","model":"custom-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`+"\n\ndata: [DONE]\n\n")
								}
								return
							}
							if rateLimited {
								w.WriteHeader(429)
								io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"Bearer PRIVATE_PROVIDER_ERROR"}}`)
							} else {
								io.WriteString(w, responseBody(providerProtocol))
							}
						}))
						defer up.Close()
						c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL), "p")
						p := c.Providers["p"]
						p.Protocol = providerProtocol
						p.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "rate_limit_error", Category: adapter.ProviderErrorRateLimited}}
						c.Providers["p"] = p
						var logs bytes.Buffer
						raw, closeDB, err := NewHandlerWithLogger(c, up.Client(), observability.JSONLogger(&logs))
						if err != nil {
							t.Fatal(err)
						}
						defer closeDB()
						h := raw.(*handler)
						waitForInitialStatementSync(t, h.ledger)
						db, err := sql.Open("sqlite", c.LedgerPath)
						if err != nil {
							t.Fatal(err)
						}
						_, err = db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON request_audit BEGIN SELECT RAISE(FAIL,'PRIVATE_SQLITE_ERROR'); END`)
						db.Close()
						if err != nil {
							t.Fatal(err)
						}
						body := fmt.Sprintf(`{"model":"p/custom-model","max_tokens":16,"stream":%t,"messages":[{"role":"user","content":"fixture"}]}`, stream)
						w := affinityGatewayRequest(h, protocol, "fixture-session", body)
						payload := w.Body.String()
						wantStatus := 500
						if stream {
							wantStatus = 200
							if strings.Contains(payload, "[DONE]") || strings.Contains(payload, "message_stop") || strings.Contains(payload, `"finish_reason":"stop"`) || strings.Contains(payload, `"stop_reason":"end_turn"`) {
								t.Fatal("audit failure sent a successful stream completion")
							}
							payload = strings.SplitN(strings.SplitN(payload, "event: error\ndata: ", 2)[1], "\n\n", 2)[0]
						}
						var envelope struct {
							Error struct {
								Code, Message string
								Retry         retryAdvice
								Failure       *diagnostics.Failure
							}
						}
						if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
							t.Fatal(err)
						}
						id := w.Header().Get("X-TideMux-Request-ID")
						advice := envelope.Error.Retry
						if w.Code != wantStatus || calls.Load() != 1 || envelope.Error.Code != "audit_failed_do_not_retry_blindly" || !strings.Contains(envelope.Error.Message, id) || !strings.Contains(envelope.Error.Message, "Do not retry automatically") || advice.Strategy != "manual_check" || advice.MaxAttempts != 0 || advice.AfterSeconds != nil || advice.ReselectProvider || len(advice.Alternatives) != 0 {
							t.Fatalf("audit failure lost safe manual handling: status=%d calls=%d payload=%s", w.Code, calls.Load(), payload)
						}
						signal := "false"
						if stream {
							signal = ""
						}
						if w.Result().Header.Get("x-should-retry") != signal || w.Result().Header.Get("Retry-After") != "" || w.Result().Header.Get("retry-after-ms") != "" {
							t.Fatal("audit failure retained retry headers or claimed late SSE headers")
						}
						terminal := findRuntimeEvent(decodeRuntimeEvents(t, logs.Bytes()), "request_terminal", id)
						if terminal == nil || terminal["outcome"] != "error" || terminal["http_status"] != float64(wantStatus) || terminal["record_persisted"] != false || terminal["upstream_attempted"] != true || terminal["error_code"] != envelope.Error.Code {
							t.Fatalf("terminal event misreported the failed gateway operation: %+v", terminal)
						}
						if rateLimited {
							failure := envelope.Error.Failure
							if failure == nil || failure.Retryability != "retryable" || failure.ResponseRetryHeaders == nil || failure.ResponseRetryHeaders.ShouldRetry != signal || failure.ResponseRetryHeaders.RetryAfterSeconds != nil {
								t.Fatal("final response lost the original upstream evidence or kept its stale retry snapshot")
							}
							encoded, _ := json.Marshal(failure)
							var expected map[string]any
							json.Unmarshal(encoded, &expected)
							if !reflect.DeepEqual(terminal["failure"], expected) || h.keyPools["p"].diagnosticStatus(time.Now()).ProviderRetryAt.Before(time.Now().Add(3590*time.Second)) {
								t.Fatal("terminal metadata differs or persistence failure shortened the server pause")
							}
						} else if !h.modelRouteAvailable("p", "custom-model") || terminal["message"].(map[string]any)["usage"] == nil {
							t.Fatal("gateway persistence failure falsified upstream health or discarded known usage")
						}
						rows, err := h.ledger.Recent(context.Background(), 10)
						if err != nil || len(rows) != 0 || strings.Contains(logs.String()+payload, "PRIVATE_") {
							t.Fatal("failed persistence was hidden, retried or exposed private error text")
						}
						retry := affinityGatewayRequest(h, protocol, "fixture-session", body)
						if retry.Code != 503 || !strings.Contains(retry.Body.String(), `"code":"audit_unavailable"`) || retry.Header().Get("x-should-retry") != "false" || !strings.Contains(retry.Body.String(), `"strategy":"manual_check"`) || calls.Load() != 1 {
							t.Fatalf("later client request generated work or advertised recovery: %d %s", retry.Code, retry.Body.String())
						}
						local := findRuntimeEvent(decodeRuntimeEvents(t, logs.Bytes()), "local_rejection", retry.Header().Get("X-TideMux-Request-ID"))
						if local == nil || local["upstream_attempted"] != false || local["record_persisted"] != true {
							t.Fatal("storage guard did not use a local, unattempted diagnostic")
						}
					})
				}
			}
		}
	}
}

func TestAuditFailureGuardSurvivesReloadAndReleasesBudgetReservations(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"custom-model"}]}`)
			return
		}
		calls.Add(1)
		io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL), "p")
	p := c.Providers["p"]
	p.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	p.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .8, Mode: "hard"}
	c.Providers["p"] = p
	raw, closeDB, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { closeDB() }()
	h := raw.(*handler)
	waitForInitialStatementSync(t, h.ledger)
	db, err := sql.Open("sqlite", c.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON request_audit BEGIN SELECT RAISE(FAIL,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	first := affinityGatewayRequest(h, "openai", "first", requestBodyFor("openai", "p", "custom-model"))
	if first.Code != 500 {
		t.Fatalf("first failure: %d %s", first.Code, first.Body.String())
	}
	if _, err := db.Exec(`DROP TRIGGER fail_audit`); err != nil {
		t.Fatal(err)
	}
	// A compatible reload creates another provider client on the same ledger.
	next := c
	next.Providers = map[string]Provider{"p": p, "new": p}
	view, err := h.prepareConfigView(context.Background(), next, c, h, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"p", "new"} {
		w := affinityGatewayRequest(view, "openai", ref, requestBodyFor("openai", ref, "custom-model"))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "audit_unavailable") || calls.Load() != 1 {
			t.Fatalf("reload bypassed shared audit guard: %d %s calls=%d", w.Code, w.Body.String(), calls.Load())
		}
	}
	var pending, unknown, settled int
	if err := db.QueryRow(`SELECT COALESCE(SUM(state='pending'),0),COALESCE(SUM(state='unknown'),0),COALESCE(SUM(state='settled'),0) FROM budget_charges`).Scan(&pending, &unknown, &settled); err != nil || pending != 0 || unknown != 0 || settled != 1 {
		t.Fatalf("unattempted rejections leaked reservations or lost known cost: %d/%d/%d %v", pending, unknown, settled, err)
	}
	request := httptest.NewRequest("GET", "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer local-secret")
	models := httptest.NewRecorder()
	view.ServeHTTP(models, request)
	if models.Code != 200 || calls.Load() != 1 {
		t.Fatal("storage guard blocked read-only model discovery")
	}
	closeDB()
	closeDB = func() error { return nil }
	restarted, closeRestarted, err := NewHandler(next, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeRestarted()
	w := affinityGatewayRequest(restarted, "openai", "after-repair", requestBodyFor("openai", "new", "custom-model"))
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("repaired/restarted gateway did not recover: %d %s", w.Code, w.Body.String())
	}
}
