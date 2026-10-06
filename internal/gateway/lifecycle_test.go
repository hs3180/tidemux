package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/observability"
)

func streamStart(protocol string) string {
	if protocol == "anthropic" {
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"
	}
	return "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"
}

func TestShutdownInterruptsIncompleteRequestBody(t *testing.T) {
	for _, delay := range []time.Duration{0, 75 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer upstream.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL)
			listener, server, closeGateway, err := Open(c, upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeGateway()
			defer server.Close()
			h := server.Handler.(*handler)
			server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.ServeHTTP(w, r)
				// A bounded delay reproduces CI scheduling/handler cleanup that
				// misses net/http's first quiescence poll after its 500ms TCP
				// reset-avoidance delay. Application cancellation is tested below
				// independently of that connection shutdown bookkeeping.
				time.Sleep(delay)
			})
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve(listener) }()
			connection, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(connection, "POST /v1/messages HTTP/1.1\r\nHost: localhost\r\nx-api-key: local-secret\r\nContent-Length: 1000\r\n\r\n{\"model\":"); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				h.activeMu.Lock()
				active := len(h.activeCalls)
				h.activeMu.Unlock()
				if active == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("body reader was not admitted")
				}
				time.Sleep(time.Millisecond)
			}
			// Keep the application's cancellation/503 deadline at one second.
			// HTTP connection draining has a separate bounded deadline because
			// net/http sleeps to avoid truncating responses with a TCP reset,
			// then polls shutdown with intervals up to another 500ms.
			if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- server.Shutdown(ctx) }()
			response, err := http.ReadResponse(bufio.NewReader(connection), nil)
			if err != nil {
				t.Fatal("incomplete body was not interrupted promptly: ", err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "server_shutting_down") {
				t.Fatalf("incomplete-body shutdown: %d %s error=%v", response.StatusCode, body, err)
			}
			h.activeMu.Lock()
			active := len(h.activeCalls)
			h.activeMu.Unlock()
			if active != 0 {
				t.Fatalf("body cancellation left %d application calls active", active)
			}
			rows, err := h.ledger.Recent(context.Background(), 10)
			if err != nil || len(rows) != 0 || upstreamCalls.Load() != 0 {
				t.Fatalf("incomplete request reached provider: calls=%d audits=%+v error=%v", upstreamCalls.Load(), rows, err)
			}
			select {
			case err := <-shutdownDone:
				if err != nil {
					t.Fatal("HTTP connection shutdown did not complete: ", err)
				}
			case <-ctx.Done():
				t.Fatal("HTTP connection shutdown did not complete: ", ctx.Err())
			}
			select {
			case err := <-serveDone:
				if err != http.ErrServerClosed {
					t.Fatalf("server did not stop its listener: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("server did not stop its listener: ", ctx.Err())
			}
		})
	}
}

func TestServerShutdownReportsStructuredFailure(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, stream := range []bool{false, true} {
			name := protocol + "/json"
			if stream {
				name = protocol + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					calls.Add(1)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, streamStart(protocol))
						w.(http.Flusher).Flush()
					}
					close(started)
					select {
					case <-r.Context().Done():
						close(canceled)
					case <-release:
					}
				}))
				defer upstream.Close()
				defer close(release)
				c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL)
				c.Protocol = protocol
				var logs bytes.Buffer
				listener, server, closeGateway, err := OpenWithLogger(c, upstream.Client(), observability.JSONLogger(&logs))
				if err != nil {
					t.Fatal(err)
				}
				defer closeGateway()
				defer server.Close()
				go server.Serve(listener)
				body := requestBody(protocol)
				if stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				req, _ := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+clientEndpoint(protocol), strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer local-secret")
				result := make(chan *http.Response, 1)
				clientErr := make(chan error, 1)
				client := &http.Client{Timeout: 5 * time.Second}
				go func() {
					response, err := client.Do(req)
					if err != nil {
						clientErr <- err
						return
					}
					result <- response
				}()
				select {
				case <-started:
				case err := <-clientErr:
					t.Fatal(err)
				case <-time.After(3 * time.Second):
					t.Fatal("upstream did not start")
				}
				var response *http.Response
				if stream {
					select {
					case response = <-result:
					case err := <-clientErr:
						t.Fatal(err)
					case <-time.After(time.Second):
						t.Fatal("client did not receive SSE headers")
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := server.Shutdown(ctx); err != nil {
					t.Fatalf("graceful shutdown: %v", err)
				}
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("upstream was not canceled")
				}
				if !stream {
					select {
					case response = <-result:
					case err := <-clientErr:
						t.Fatal(err)
					case <-time.After(time.Second):
						t.Fatal("client did not receive a response")
					}
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				wantStatus := http.StatusServiceUnavailable
				if stream {
					wantStatus = http.StatusOK
				}
				if response.StatusCode != wantStatus || !strings.Contains(string(data), `"code":"server_shutting_down"`) || stream && (!strings.Contains(string(data), "event: error\n") || strings.Contains(string(data), "[DONE]") || strings.Contains(string(data), "message_stop")) {
					t.Fatalf("shutdown response: status=%d body=%s", response.StatusCode, data)
				}
				rows, err := server.Handler.(*handler).ledger.Recent(context.Background(), 10)
				if err != nil || len(rows) != 1 || rows[0].Status != "canceled" || rows[0].ErrorCode != "server_shutting_down" {
					t.Fatalf("terminal audit: %+v error=%v", rows, err)
				}
				// A request on an existing connection must not start new model work.
				out := httptest.NewRecorder()
				server.Handler.ServeHTTP(out, req)
				if out.Code != http.StatusServiceUnavailable || calls.Load() != 1 || !strings.Contains(out.Body.String(), "server_shutting_down") {
					t.Fatalf("draining admission: %d calls=%d %s", out.Code, calls.Load(), out.Body)
				}
				if strings.Contains(logs.String(), "local-secret") || strings.Contains(logs.String(), "provider-secret") {
					t.Fatal("credentials leaked")
				}
			})
		}
	}
}

type panicRoundTripper struct {
	protocol string
	stream   bool
}

func (p panicRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if !p.stream {
		panic("private-provider-panic-marker")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(io.MultiReader(strings.NewReader(streamStart(p.protocol)), panicReader{})), Request: r}, nil
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("private-provider-panic-marker") }

func TestPreDispatchPanicIsRedacted(t *testing.T) {
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "http://127.0.0.1:1")
	var logs bytes.Buffer
	h, closeDB, err := NewHandlerWithLogger(c, nil, observability.JSONLogger(&logs))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", panicReader{})
	req.Header.Set("x-api-key", "local-secret")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusInternalServerError || !strings.Contains(out.Body.String(), `"code":"internal_error"`) || strings.Contains(out.Body.String()+logs.String(), "private-provider-panic-marker") {
		t.Fatalf("panic response: %d %s", out.Code, out.Body)
	}
	rows, err := h.(*handler).ledger.Recent(context.Background(), 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("pre-dispatch panic created attempt: %+v %v", rows, err)
	}
	if len(h.(*handler).activeCalls) != 0 {
		t.Fatal("panic leaked active request")
	}
}

func TestShutdownReleasesQueuedBudgetReservation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL), "mock")
	provider := c.Providers["mock"]
	provider.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	provider.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 2, AlertThreshold: .8, Mode: "hard"}
	c.Providers["mock"] = provider
	h, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	done := make(chan *httptest.ResponseRecorder, 2)
	request := func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBodyFor("openai", "mock", "custom-model")))
		req.Header.Set("Authorization", "Bearer local-secret")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		done <- out
	}
	go request()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream did not start")
	}
	go request()
	handler := h.(*handler)
	deadline := time.Now().Add(time.Second)
	for {
		var reservations int
		if err := handler.ledger.QueryRow(context.Background(), `SELECT COUNT(*) FROM budget_charges WHERE state='pending'`).Scan(&reservations); err != nil {
			t.Fatal(err)
		}
		if reservations == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second request did not reserve a budget")
		}
		time.Sleep(time.Millisecond)
	}
	handler.beginShutdown()
	for i := 0; i < 2; i++ {
		select {
		case out := <-done:
			if out.Code != http.StatusServiceUnavailable || !strings.Contains(out.Body.String(), "server_shutting_down") {
				t.Fatalf("shutdown response: %d %s", out.Code, out.Body)
			}
		case <-time.After(time.Second):
			t.Fatal("request did not finish")
		}
	}
	var charges, pending int
	if err := handler.ledger.QueryRow(context.Background(), `SELECT COUNT(*),COALESCE(SUM(state='pending'),0) FROM budget_charges`).Scan(&charges, &pending); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || charges != 1 || pending != 0 {
		t.Fatalf("shutdown accounting: calls=%d charges=%d pending=%d", calls.Load(), charges, pending)
	}
}

func TestHandlerPanicProducesSafeErrorAndAudit(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, stream := range []bool{false, true} {
			name := protocol + "/json"
			if stream {
				name = protocol + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "http://127.0.0.1:1")
				c.Protocol = protocol
				var logs bytes.Buffer
				h, closeDB, err := NewHandlerWithLogger(c, &http.Client{Transport: panicRoundTripper{protocol, stream}}, observability.JSONLogger(&logs))
				if err != nil {
					t.Fatal(err)
				}
				defer closeDB()
				body := requestBody(protocol)
				if stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				req := httptest.NewRequest(http.MethodPost, clientEndpoint(protocol), strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer local-secret")
				out := httptest.NewRecorder()
				h.ServeHTTP(out, req)
				wantStatus := http.StatusInternalServerError
				if stream {
					wantStatus = http.StatusOK
				}
				if out.Code != wantStatus || !strings.Contains(out.Body.String(), `"code":"internal_error"`) || stream && !strings.Contains(out.Body.String(), "event: error\n") {
					t.Fatalf("panic response: %d %s", out.Code, out.Body)
				}
				rows, err := h.(*handler).ledger.Recent(context.Background(), 10)
				if err != nil || len(rows) != 1 || rows[0].Status != "error" || rows[0].ErrorCode != "internal_error" {
					t.Fatalf("panic audit: %+v error=%v", rows, err)
				}
				encoded, _ := json.Marshal(rows)
				if strings.Contains(out.Body.String()+logs.String()+string(encoded), "private-provider-panic-marker") {
					t.Fatal("panic value leaked")
				}
			})
		}
	}
}
