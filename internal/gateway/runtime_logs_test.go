package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/observability"
)

func decodeRuntimeEvents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid JSON Lines runtime event: %v: %s", err, scanner.Text())
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func findRuntimeEvent(events []map[string]any, eventName, requestID string) map[string]any {
	for _, event := range events {
		if event["event"] == eventName && (requestID == "" || event["request_id"] == requestID) {
			return event
		}
	}
	return nil
}

func TestRequestRuntimeEventsCorrelateAndExcludeSensitiveData(t *testing.T) {
	var logs bytes.Buffer
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/models" {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"provider_denied","message":"upstream-body-sensitive-sentinel"}}`)
	}))
	defer upstream.Close()

	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL), "provider-main")
	rawHandler, closeGateway, err := NewHandlerWithLogger(c, upstream.Client(), observability.JSONLogger(&logs))
	if err != nil {
		t.Fatal(err)
	}
	defer closeGateway()
	gatewayHandler := rawHandler.(*handler)

	body := `{"model":"provider-main/custom-model","messages":[{"role":"user","content":"request-body-sensitive-sentinel"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?private-query-sentinel", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer local-secret")
	request.Header.Set("X-TideMux-Session-ID", "session-sensitive-marker")
	response := httptest.NewRecorder()
	gatewayHandler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("upstream rejection status=%d body=%s", response.Code, response.Body.String())
	}
	requestID := response.Header().Get("X-TideMux-Request-ID")
	rows, err := gatewayHandler.ledger.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].ID != requestID || rows[0].Status != "error" {
		t.Fatalf("audit rows=%+v err=%v", rows, err)
	}

	localReject := httptest.NewRequest(http.MethodGet, "/v1/unsupported?private-query-sentinel", nil)
	localReject.Header.Set("Authorization", "Bearer invalid-local-token")
	localResponse := httptest.NewRecorder()
	gatewayHandler.ServeHTTP(localResponse, localReject)
	if localResponse.Code != http.StatusUnauthorized {
		t.Fatalf("local rejection status=%d body=%s", localResponse.Code, localResponse.Body.String())
	}
	localID := localResponse.Header().Get("X-TideMux-Request-ID")
	diagnostics, err := gatewayHandler.ledger.RecentDiagnostics(context.Background(), 10)
	if err != nil || len(diagnostics) != 1 || diagnostics[0].ID != localID {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}

	events := decodeRuntimeEvents(t, logs.Bytes())
	terminal := findRuntimeEvent(events, "request_terminal", requestID)
	if terminal == nil {
		t.Fatalf("terminal request event missing for %s: %s", requestID, logs.String())
	}
	for key, want := range map[string]any{
		"schema_version": float64(observability.SchemaVersion), "protocol": "openai",
		"provider_protocol": "openai", "endpoint": "chat_completions",
		"provider_ref": "provider-main", "model": "custom-model",
		"outcome": "error", "error_code": "upstream_error", "http_status": float64(http.StatusBadGateway),
		"upstream_attempted": true, "record_persisted": true,
	} {
		if terminal[key] != want {
			t.Errorf("terminal.%s=%#v want %#v", key, terminal[key], want)
		}
	}
	if terminal["latency_ms"].(float64) < 0 || terminal["queue_time_ms"].(float64) < 0 {
		t.Fatalf("negative timing in terminal event: %#v", terminal)
	}
	for _, field := range []string{"session_id", "estimated_cost", "price_snapshot", "input_tokens", "output_tokens"} {
		if _, ok := terminal[field]; ok {
			t.Errorf("runtime event included accounting/session field %q: %#v", field, terminal)
		}
	}
	local := findRuntimeEvent(events, "local_rejection", localID)
	if local == nil || local["outcome"] != "rejected" || local["error_code"] != "invalid_api_key" || local["http_status"] != float64(http.StatusUnauthorized) || local["record_persisted"] != true {
		t.Fatalf("local rejection event missing or incomplete: %#v; logs=%s", local, logs.String())
	}
	for _, private := range []string{
		"request-body-sensitive-sentinel", "upstream-body-sensitive-sentinel",
		"private-query-sentinel", "local-secret", "invalid-local-token", "provider-secret", "session-sensitive-marker",
	} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("runtime logs leaked %q: %s", private, logs.String())
		}
	}
}
