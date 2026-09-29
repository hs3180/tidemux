package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestRequestSummaryHasStableSchemaAndDropsUnsafeIdentifiers(t *testing.T) {
	var output bytes.Buffer
	logger := JSONLogger(&output)
	RequestSummary{
		Event: "request_terminal", RequestID: strings.Repeat("a", 32),
		Protocol: "openai", ProviderProtocol: "anthropic", Endpoint: "chat_completions",
		ProviderRef: "provider-main", Model: "model/with:scope",
		Outcome: "error", ErrorCode: "upstream_error", HTTPStatus: 502,
		LatencyMS: 13, QueueTimeMS: 2, UpstreamAttempted: true, RecordPersisted: true,
	}.Log(logger)
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatalf("invalid JSON log line: %v: %s", err, output.String())
	}
	want := map[string]any{
		"schema_version": float64(SchemaVersion), "event": "request_terminal",
		"request_id": strings.Repeat("a", 32), "protocol": "openai",
		"provider_protocol": "anthropic", "endpoint": "chat_completions",
		"provider_ref": "provider-main", "model": "model/with:scope",
		"outcome": "error", "error_code": "upstream_error", "http_status": float64(502),
		"latency_ms": float64(13), "queue_time_ms": float64(2),
		"upstream_attempted": true, "record_persisted": true, "level": slog.LevelWarn.String(),
	}
	for key, expected := range want {
		if event[key] != expected {
			t.Errorf("%s=%#v want %#v", key, event[key], expected)
		}
	}

	output.Reset()
	RequestSummary{
		Event: "local_rejection", RequestID: "invalid-request-id",
		Protocol: "openai", Endpoint: "unsupported", ProviderRef: "safe\nsecret",
		Model: "model?credential=private", Outcome: "rejected", ErrorCode: "bad code",
		HTTPStatus: 400,
	}.Log(logger)
	if strings.Contains(output.String(), "safe") || strings.Contains(output.String(), "private") || strings.Contains(output.String(), "invalid-request-id") {
		t.Fatalf("unsafe identifier reached logs: %s", output.String())
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatalf("invalid normalized JSON log line: %v: %s", err, output.String())
	}
	for _, key := range []string{"request_id", "provider_ref", "model", "error_code", "http_status", "latency_ms", "queue_time_ms", "outcome"} {
		if _, ok := event[key]; !ok {
			t.Errorf("stable field %q missing from event: %s", key, output.String())
		}
	}
}

func TestHTTPServerErrorLoggerDoesNotForwardUntrustedMessage(t *testing.T) {
	var output bytes.Buffer
	logger := JSONLogger(&output)
	message := "malformed request /secret?token=private-body-marker"
	n, err := HTTPServerErrorLogger(logger).Writer().Write([]byte(message))
	if err != nil || n != len(message) {
		t.Fatalf("Write()=(%d,%v)", n, err)
	}
	if strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "private-body-marker") || strings.Contains(output.String(), message) {
		t.Fatalf("server error log exposed its raw message: %s", output.String())
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatalf("invalid JSON server event: %v: %s", err, output.String())
	}
	if event["event"] != "http_server_error" || event["error_class"] != "net_http" {
		t.Fatalf("unexpected server error event: %#v", event)
	}
}
