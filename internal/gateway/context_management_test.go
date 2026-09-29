package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAnthropicContextManagementToOpenAIIsForwardedToUpstream(t *testing.T) {
	calls := 0
	var forwarded json.RawMessage
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			calls++
			if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer provider-secret" {
				t.Errorf("upstream request path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upstream request: %v", err)
			} else {
				forwarded = body["context_management"]
			}
		}
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL), "openai-main")
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"openai-main/custom-model","max_tokens":64,"messages":[{"role":"user","content":"continue"}],"context_management":{"edits":[{"type":"compact_20260112","trigger":{"type":"input_tokens","value":50000},"future_option":{"retain":true}}]}}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "local-secret")
	req.Header.Set("anthropic-version", "2023-06-01")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status=%d upstream_calls=%d body=%s", out.Code, calls, out.Body.String())
	}
	var got, want any
	if err := json.Unmarshal(forwarded, &got); err != nil {
		t.Fatalf("upstream did not receive context_management: %q (%v)", forwarded, err)
	}
	if err := json.Unmarshal([]byte(`{"edits":[{"type":"compact_20260112","trigger":{"type":"input_tokens","value":50000},"future_option":{"retain":true}}]}`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream context_management=%#v want=%#v", got, want)
	}
}

func TestAnthropicContextManagementUpstreamRejectionIsRelayed(t *testing.T) {
	calls := 0
	var forwarded json.RawMessage
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model","object":"model"}]}`)
			return
		}
		calls++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		} else {
			forwarded = body["context_management"]
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"unsupported context_management","param":"context_management","code":"invalid_parameter"}}`)
	}))
	defer up.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL), "openai-main")
	h, closeDB, err := NewHandler(c, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"openai-main/custom-model","max_tokens":64,"messages":[{"role":"user","content":"continue"}],"context_management":{"edits":[{"type":"compact_20260112","future_option":{"retain":true}}]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "local-secret")
	req.Header.Set("anthropic-version", "2023-06-01")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if calls != 1 || len(forwarded) == 0 || out.Code != http.StatusBadRequest {
		t.Fatalf("status=%d upstream_calls=%d forwarded=%q body=%s", out.Code, calls, forwarded, out.Body.String())
	}
	if !strings.Contains(out.Body.String(), `"type":"error"`) || strings.Contains(out.Body.String(), "unsupported_request_feature") {
		t.Fatalf("upstream error was not relayed through the Anthropic envelope: %s", out.Body.String())
	}
}

func TestNativeAnthropicContextManagementCompactionRoundTrips(t *testing.T) {
	const contextManagement = `{"edits":[{"type":"compact_20260112","trigger":{"type":"input_tokens","value":50000},"future_option":{"opaque":"context_management_sensitive_sentinel"}}]}`
	const compactResponse = `{"id":"msg1","type":"message","role":"assistant","model":"custom-model","content":[{"type":"compaction","content":"response_body_sensitive_sentinel"}],"stop_reason":"compaction","usage":{"input_tokens":3,"output_tokens":2}}`
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"custom-model","type":"model"}],"has_more":false}`)
			return
		}
		calls++
		if r.URL.Path != "/messages" || r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "compact-2026-01-12" {
			t.Errorf("upstream request path=%q x-api-key=%q version=%q beta=%q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.Header.Get("anthropic-beta"))
		}
		var body struct {
			ContextManagement json.RawMessage `json:"context_management"`
			Messages          []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		} else {
			var got, want any
			if err := json.Unmarshal(body.ContextManagement, &got); err != nil {
				t.Errorf("context_management not forwarded: %q", body.ContextManagement)
			} else if err := json.Unmarshal([]byte(contextManagement), &want); err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("context_management=%#v want=%#v err=%v", got, want, err)
			}
			if calls == 2 {
				foundCompaction := false
				for _, message := range body.Messages {
					if message.Role != "assistant" {
						continue
					}
					var blocks []map[string]json.RawMessage
					if json.Unmarshal(message.Content, &blocks) != nil {
						continue
					}
					for _, block := range blocks {
						var kind, content string
						_ = json.Unmarshal(block["type"], &kind)
						_ = json.Unmarshal(block["content"], &content)
						foundCompaction = foundCompaction || kind == "compaction" && content == "response_body_sensitive_sentinel"
					}
				}
				if !foundCompaction {
					t.Errorf("follow-up request lost compaction block: %+v", body.Messages)
				}
			}
		}
		if calls == 1 {
			_, _ = io.WriteString(w, compactResponse)
		} else {
			_, _ = io.WriteString(w, responseBody("anthropic"))
		}
	}))
	defer up.Close()
	ledgerDir := t.TempDir()
	c := testConfig(filepath.Join(ledgerDir, "ledger.db"), up.URL)
	c.Protocol = "anthropic"
	h, closeDB, err := NewHandler(namedProviderConfig(c, "anthropic-main"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	first := `{"model":"anthropic-main/custom-model","max_tokens":64,"messages":[{"role":"user","content":"request_body_sensitive_sentinel"}],"context_management":{"edits":[{"type":"compact_20260112","trigger":{"type":"input_tokens","value":50000},"future_option":{"opaque":"context_management_sensitive_sentinel"}}]}}`
	second := `{"model":"anthropic-main/custom-model","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"compaction","content":"response_body_sensitive_sentinel"}]},{"role":"user","content":"continue"}],"context_management":{"edits":[{"type":"compact_20260112","trigger":{"type":"input_tokens","value":50000},"future_option":{"opaque":"context_management_sensitive_sentinel"}}]}}`
	for i, body := range []string{first, second} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", "local-secret")
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "compact-2026-01-12")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if out.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i+1, out.Code, out.Body.String())
		}
		if i == 0 && (!strings.Contains(out.Body.String(), `"type":"compaction"`) || !strings.Contains(out.Body.String(), `"stop_reason":"compaction"`) || !strings.Contains(out.Body.String(), "response_body_sensitive_sentinel")) {
			t.Fatalf("compaction response was not preserved: %s", out.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d want=2", calls)
	}
	entries, err := os.ReadDir(ledgerDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		stored, err := os.ReadFile(filepath.Join(ledgerDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, sentinel := range []string{"request_body_sensitive_sentinel", "context_management_sensitive_sentinel", "response_body_sensitive_sentinel"} {
			if bytes.Contains(stored, []byte(sentinel)) {
				t.Fatalf("request/response body sentinel %q was persisted in %s", sentinel, entry.Name())
			}
		}
	}
}

func TestNativeAnthropicContextManagementCompactionSSEIsForwarded(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"custom-model","type":"model"}],"has_more":false}`)
			return
		}
		calls++
		if r.Header.Get("anthropic-beta") != "compact-2026-01-12" {
			t.Errorf("anthropic-beta=%q", r.Header.Get("anthropic-beta"))
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body["context_management"]) == 0 {
			t.Errorf("context_management missing: %q err=%v", body["context_management"], err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"custom-model\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"compaction\",\"content\":null}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"compaction_delta\",\"content\":\"compact summary\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"compaction\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer up.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
	c.Protocol = "anthropic"
	h, closeDB, err := NewHandler(namedProviderConfig(c, "anthropic-main"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"anthropic-main/custom-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"continue"}],"context_management":{"edits":[{"type":"compact_20260112"}]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "local-secret")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "compact-2026-01-12")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", out.Code, calls, out.Body.String())
	}
	for _, fragment := range []string{`"type":"compaction"`, `"type":"compaction_delta"`, `"stop_reason":"compaction"`} {
		if !strings.Contains(out.Body.String(), fragment) {
			t.Fatalf("stream lost %s: %s", fragment, out.Body.String())
		}
	}
}
