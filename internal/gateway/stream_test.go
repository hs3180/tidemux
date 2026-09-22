package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

func TestStreamingGatewayAudit(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, scenario := range []string{"ok", "truncated", "error", "audit-failure"} {
			t.Run(protocol+"/"+scenario, func(t *testing.T) {
				start := `data: {"choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\n"
				end := "data: [DONE]\n\n"
				if protocol == "anthropic" {
					start = `event: message_start` + "\n" + `data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n" + `event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n"
					end = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, start)
					w.(http.Flusher).Flush()
					if scenario == "error" {
						io.WriteString(w, "event: error\ndata: {\"error\":{\"message\":\"secret upstream body\"}}\n\n")
						return
					}
					if scenario != "truncated" {
						io.WriteString(w, end)
					}
				}))
				defer up.Close()
				c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
				c.Protocol = protocol
				h, closeDB, err := NewHandler(c, up.Client())
				if err != nil {
					t.Fatal(err)
				}
				defer closeDB()
				if scenario == "audit-failure" {
					closeDB()
				}
				body := strings.TrimSuffix(requestBody(protocol), "}") + `,"stream":true}`
				req := httptest.NewRequest("POST", endpoint(protocol), strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer local-secret")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" || !w.Flushed {
					t.Fatalf("%d %v", w.Code, w.Header())
				}
				if strings.Contains(w.Body.String(), "secret upstream body") {
					t.Fatal("leaked error")
				}
				if scenario == "ok" {
					if protocol == "openai" {
						if w.Body.String() != start+end {
							t.Fatalf("SSE modified %s", w.Body.String())
						}
					} else if body := w.Body.String(); !strings.Contains(body, `"chat.completion.chunk"`) || !strings.Contains(body, "data: [DONE]") || strings.Contains(body, "event:") || strings.Contains(body, "end_turn") {
						t.Fatalf("Anthropic stream was not translated: %s", body)
					}
				} else if strings.Contains(w.Body.String(), end) || !strings.Contains(w.Body.String(), "event: error") {
					t.Fatal("failed stream completed")
				}
				if scenario == "audit-failure" {
					if !strings.Contains(w.Body.String(), "audit_failed_do_not_retry_blindly") {
						t.Fatal(w.Body.String())
					}
					return
				}
				db, err := ledger.Open(c.LedgerPath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				rows, err := db.Recent(context.Background(), 10)
				if err != nil || len(rows) != 1 {
					t.Fatalf("%v %v", rows, err)
				}
				a := rows[0]
				if a.ID != w.Header().Get("X-TideMux-Request-ID") {
					t.Fatal("missing audit correlation")
				}
				if scenario == "ok" {
					if a.Status != "ok" || a.InputTokens == nil || *a.InputTokens != 3 || *a.OutputTokens != 2 {
						t.Fatalf("%+v", a)
					}
				} else if a.Status != "error" || a.InputTokens != nil || a.OutputTokens != nil {
					t.Fatalf("failed stream false usage %+v", a)
				}
			})
		}
	}
}

func TestAnthropicClientStreamingWithOpenAIProvider(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Fatalf("unexpected upstream request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"chat1","object":"chat.completion.chunk","created":1,"model":"custom-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"chat1","object":"chat.completion.chunk","created":1,"model":"custom-model","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"chat1","object":"chat.completion.chunk","created":1,"model":"custom-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
	c.Protocol = "openai"
	h, closeDB, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := strings.TrimSuffix(requestBody("anthropic"), "}") + `,"stream":true}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "local-secret")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != 200 || out.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(out.Body.String(), "event: message_start") || !strings.Contains(out.Body.String(), `"text":"hi"`) || !strings.Contains(out.Body.String(), "event: message_stop") {
		t.Fatalf("status=%d headers=%v body=%s", out.Code, out.Header(), out.Body.String())
	}
	db, err := ledger.Open(c.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].InputTokens == nil || *rows[0].InputTokens != 3 || rows[0].OutputTokens == nil || *rows[0].OutputTokens != 2 {
		t.Fatalf("audit=%+v err=%v", rows, err)
	}
}

func TestStreamingCancellationRetainsConcurrencyUntilClose(t *testing.T) {
	entered := make(chan struct{}, 2)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
	h, closeDB, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	gate := httptest.NewServer(h)
	defer gate.Close()
	makeReq := func(ctx context.Context) *http.Request {
		req, _ := http.NewRequestWithContext(ctx, "POST", gate.URL+endpoint("openai"), strings.NewReader(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer local-secret")
		return req
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := gate.Client().Do(makeReq(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	<-entered
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	secondDone := make(chan error, 1)
	go func() {
		r, e := gate.Client().Do(makeReq(ctx2))
		if e == nil {
			r.Body.Close()
		}
		secondDone <- e
	}()
	select {
	case <-entered:
		t.Fatal("second call admitted during active stream")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	first.Body.Close()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("slot was not released after cancellation")
	}
	cancel2()
	<-secondDone
	// Terminal audit is asynchronous relative to the client socket closing.
	db, err := ledger.Open(c.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, e := db.Recent(context.Background(), 10)
		if e != nil {
			t.Fatal(e)
		}
		if len(rows) == 2 {
			for _, a := range rows {
				if a.Status != "canceled" || a.InputTokens != nil || a.OutputTokens != nil {
					t.Fatalf("%+v", a)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled audits missing")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
