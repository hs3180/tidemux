package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/contentlog"
	"github.com/hs3180/tidemux/internal/observability"
)

func readContent(t *testing.T, directory string) []contentlog.Event {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(directory, "content-*.jsonl"))
	var events []contentlog.Event
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := bufio.NewScanner(bytes.NewReader(data))
		s.Buffer(make([]byte, contentlog.MaxEventBytes), contentlog.MaxEventBytes)
		for s.Scan() {
			var e contentlog.Event
			if err := json.Unmarshal(s.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			events = append(events, e)
		}
		if err := s.Err(); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	return events
}

func TestContentOutputClientViewsAcrossProtocolsAndStreaming(t *testing.T) {
	for _, clientProtocol := range []string{"openai", "anthropic"} {
		for _, providerProtocol := range []string{"openai", "anthropic"} {
			for _, stream := range []bool{false, true} {
				name := clientProtocol + "-to-" + providerProtocol
				if stream {
					name += "-stream"
				}
				t.Run(name, func(t *testing.T) {
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							io.WriteString(w, aliasStream(providerProtocol, "custom-model"))
						} else {
							io.WriteString(w, responseBody(providerProtocol))
						}
					}))
					defer up.Close()
					root := t.TempDir()
					c := testConfig(filepath.Join(root, "ledger.db"), up.URL)
					c.Protocol = providerProtocol
					c.ContentOutput = contentlog.Config{Enabled: true, Directory: filepath.Join(root, "content")}
					var logs bytes.Buffer
					h, closeGateway, err := NewHandlerWithLogger(c, up.Client(), observability.JSONLogger(&logs))
					if err != nil {
						t.Fatal(err)
					}
					body := strings.ReplaceAll(requestBody(clientProtocol), "hello", "body-private-marker")
					// The exact client body, including an ignored extension and its
					// whitespace, must survive conversion/model rewriting unchanged.
					body = strings.TrimSuffix(body, "}") + `, "capture_fixture":"original"}`
					if stream {
						body = strings.TrimSuffix(body, "}") + `,"stream":true}`
					}
					path := "/v1/chat/completions"
					if clientProtocol == "anthropic" {
						path = "/v1/messages"
					}
					r := httptest.NewRequest("POST", path, strings.NewReader(body))
					r.Header.Set("Authorization", "Bearer local-secret")
					if clientProtocol == "anthropic" {
						r.Header.Set("anthropic-version", "2023-06-01")
					}
					r.Header.Set("X-TideMux-Session-ID", "private-session-header")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					rows, err := h.(*handler).ledger.Recent(context.Background(), 1)
					if err != nil || len(rows) != 1 || rows[0].ID != w.Header().Get("X-TideMux-Request-ID") || w.Code != 200 {
						t.Fatalf("request/audit: %v %v %s", rows, err, w.Body.String())
					}
					closeGateway()
					events := readContent(t, c.ContentOutput.Directory)
					if len(events) < 3 {
						t.Fatalf("missing content: %+v", events)
					}
					var input, output []byte
					for i, e := range events {
						if e.RequestID != rows[0].ID || e.Protocol != clientProtocol || e.ProviderProtocol != providerProtocol || e.Model != "custom-model" || e.Provider != "legacy" || e.SessionID == "" || e.SessionID == "private-session-header" || e.Seq != uint64(i) {
							t.Fatalf("metadata: %+v", e)
						}
						if e.Direction == "input" {
							input = append(input, e.Data...)
						}
						if e.Direction == "output" {
							output = append(output, e.Data...)
						}
					}
					end := events[len(events)-1]
					sum := sha256.Sum256(output)
					if string(input) != body || string(output) != w.Body.String() || end.Complete == nil || !*end.Complete || end.OutputSHA256 != hex.EncodeToString(sum[:]) {
						t.Fatalf("client view/completeness: %+v", end)
					}
					if strings.Contains(string(input)+string(output), "local-secret") || strings.Contains(logs.String(), "body-private-marker") {
						t.Fatal("content/header leaked across output boundaries")
					}
				})
			}
		}
	}
}

type failedContentWriter struct{ *httptest.ResponseRecorder }

func (w failedContentWriter) Write(data []byte) (int, error) {
	w.ResponseRecorder.Write(data[:3])
	return 3, errors.New("disconnected")
}

// A nonstreaming HTTP writer is allowed to omit optional flushing support.
type contentWriterWithoutFlush struct{ recorder *httptest.ResponseRecorder }

func (w contentWriterWithoutFlush) Header() http.Header            { return w.recorder.Header() }
func (w contentWriterWithoutFlush) WriteHeader(status int)         { w.recorder.WriteHeader(status) }
func (w contentWriterWithoutFlush) Write(data []byte) (int, error) { return w.recorder.Write(data) }

func TestContentOutputDefaultDisableFailuresAndPartialClientWrites(t *testing.T) {
	for _, scenario := range []string{"default", "disabled", "truncated", "write-failure", "no-flusher", "canceled", "invalid-request"} {
		t.Run(scenario, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "truncated" {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
					return
				}
				io.WriteString(w, responseBody("openai"))
			}))
			defer up.Close()
			root := t.TempDir()
			c := testConfig(filepath.Join(root, "ledger.db"), up.URL)
			directory := filepath.Join(root, "content")
			if scenario != "default" {
				c.ContentOutput = contentlog.Config{Enabled: scenario != "disabled", Directory: directory}
			}
			h, closeGateway, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			body := requestBody("openai")
			if scenario == "truncated" {
				body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			}
			if scenario == "invalid-request" {
				body = "{broken"
			}
			r := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer local-secret")
			if scenario == "canceled" {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			var writer http.ResponseWriter = w
			if scenario == "write-failure" {
				writer = failedContentWriter{w}
			}
			if scenario == "no-flusher" {
				writer = contentWriterWithoutFlush{w}
			}
			h.ServeHTTP(writer, r)
			closeGateway()
			events := readContent(t, directory)
			if scenario == "default" || scenario == "disabled" {
				if len(events) != 0 {
					t.Fatal("disabled output wrote bodies")
				}
				if scenario == "default" {
					if _, err := os.Stat(directory); !os.IsNotExist(err) {
						t.Fatal("default created content directory")
					}
				}
				return
			}
			if scenario == "no-flusher" {
				if len(events) == 0 || events[len(events)-1].Complete == nil || !*events[len(events)-1].Complete {
					t.Fatal("optional flushing incorrectly makes content partial")
				}
				return
			}
			if len(events) == 0 || events[len(events)-1].Complete == nil || *events[len(events)-1].Complete || events[len(events)-1].Outcome == "success" {
				t.Fatalf("partial output claimed success: %+v", events)
			}
		})
	}
}

func TestContentOutputEnabledReloadAndRuntimeSettings(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if strings.Contains(string(data), `"stream":true`) {
			close(entered)
			<-release
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, aliasStream("openai", "custom-model"))
			return
		}
		io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	defer releaseUpstream()
	root := t.TempDir()
	c := testConfig(filepath.Join(root, "ledger.db"), up.URL)
	c.ContentOutput = contentlog.Config{Directory: filepath.Join(root, "content")}
	h, closeGateway, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	path := filepath.Join(root, "config.json")
	stop := WatchConfig(h, path, c, testSecrets{"test.provider": "provider-secret", "test.gateway": "local-secret"}, up.Client())
	defer stop()
	save := func(enabled bool) {
		c.ContentOutput.Enabled = enabled
		data, _ := json.Marshal(c)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if h.(*handler).reload.active.Load().config.ContentOutput.Enabled == enabled {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("content setting did not apply")
	}
	save(true)
	body := strings.TrimSuffix(requestBody("openai"), "}") + `,"stream":true}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer local-secret")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(w, r) }()
	select {
	case <-entered:
	case <-time.After(4 * time.Second):
		t.Fatal("request did not dispatch")
	}
	// Input has already been enqueued before any upstream response arrives.
	deadline := time.Now().Add(time.Second)
	for h.(*handler).contentOutput.Status()["written_events"] == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.(*handler).contentOutput.Status()["written_events"] == 0 {
		t.Fatal("input waits for response")
	}
	save(false)
	releaseUpstream()
	<-done
	r = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(requestBody("openai")))
	r.Header.Set("Authorization", "Bearer local-secret")
	h.ServeHTTP(httptest.NewRecorder(), r)
	stop()
	closeGateway()
	events := readContent(t, c.ContentOutput.Directory)
	if len(events) == 0 {
		t.Fatal("admitted capture was lost on disable")
	}
	for _, e := range events {
		if e.RequestID != w.Header().Get("X-TideMux-Request-ID") {
			t.Fatal("disabled view captured a new request")
		}
	}
}

func TestContentOutputToolsReasoningAndAuthenticatedStatus(t *testing.T) {
	response := `{"id":"m","type":"message","role":"assistant","model":"custom-model","content":[{"type":"thinking","thinking":"visible reasoning","signature":"fixture"},{"type":"tool_use","id":"call2","name":"read_file","input":{"path":"fixture"}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":2}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, response) }))
	defer up.Close()
	root := t.TempDir()
	c := testConfig(filepath.Join(root, "ledger.db"), up.URL)
	c.Protocol = "anthropic"
	c.ContentOutput = contentlog.Config{Enabled: true, Directory: filepath.Join(root, "content")}
	h, closeGateway, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	for _, authenticated := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/tidemux/content-output-status", nil)
		if authenticated {
			r.Header.Set("Authorization", "Bearer local-secret")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if authenticated && (w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":true`)) || !authenticated && w.Code != 401 {
			t.Fatal("status authentication failed")
		}
	}
	body := `{"model":"legacy/custom-model","max_tokens":2048,"system":"system context","tools":[{"name":"read_file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"read fixture"},{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"read_file","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"tool result"}]}]}`
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer local-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("native tool request failed: %s", w.Body.String())
	}
	closeGateway()
	var input, output []byte
	for _, e := range readContent(t, c.ContentOutput.Directory) {
		if e.Direction == "input" {
			input = append(input, e.Data...)
		}
		if e.Direction == "output" {
			output = append(output, e.Data...)
		}
	}
	if string(input) != body || string(output) != response {
		t.Fatal("tools/reasoning were modified or omitted in capture")
	}
}

type cancelContentWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w cancelContentWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	w.cancel()
	return n, err
}

func TestContentOutputCancellationAfterFirstStreamContent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	root := t.TempDir()
	c := testConfig(filepath.Join(root, "ledger.db"), up.URL)
	c.ContentOutput = contentlog.Config{Enabled: true, Directory: filepath.Join(root, "content")}
	h, closeGateway, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	body := strings.TrimSuffix(requestBody("openai"), "}") + `,"stream":true}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer local-secret")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(cancelContentWriter{w, cancel}, r.WithContext(ctx))
	closeGateway()
	events := readContent(t, c.ContentOutput.Directory)
	if len(events) < 3 {
		t.Fatal("partial stream missing")
	}
	end := events[len(events)-1]
	if end.Complete == nil || *end.Complete || end.Outcome != "canceled" || end.OutputBytes != int64(w.Body.Len()) || !strings.Contains(w.Body.String(), "partial") {
		t.Fatalf("cancellation integrity: %+v", end)
	}
}

func TestContentOutputDiskFailureDoesNotChangeInferenceOrAccounting(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	root := t.TempDir()
	c := testConfig(filepath.Join(root, "ledger.db"), up.URL)
	c.ContentOutput = contentlog.Config{Enabled: true, Directory: filepath.Join(root, "private-output")}
	var logs bytes.Buffer
	h, closeGateway, err := NewHandlerWithLogger(c, up.Client(), observability.JSONLogger(&logs))
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	// After startup, make the output pathname unusable without touching the
	// gateway's ledger or any other directory.
	if err := os.Rename(c.ContentOutput.Directory, c.ContentOutput.Directory+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.ContentOutput.Directory, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(requestBody("openai"), "hello", "private-body-sentinel")
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer local-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	rows, err := h.(*handler).ledger.Recent(context.Background(), 1)
	if err != nil || len(rows) != 1 || rows[0].Status != "ok" || calls.Load() != 1 || w.Code != 200 {
		t.Fatalf("storage changed inference/accounting: %v %v %d", rows, err, calls.Load())
	}
	closeGateway()
	if h.(*handler).contentOutput.Status()["write_failures"] == 0 || !strings.Contains(logs.String(), "content_output_gap") {
		t.Fatal("output loss was not observable")
	}
	for _, secret := range []string{root, "private-body-sentinel", "local-secret", "provider-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("content failure leaked a body, path or credential")
		}
	}
}
