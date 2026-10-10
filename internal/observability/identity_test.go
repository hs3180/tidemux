package observability

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type boundIdentityValue struct{ evaluations atomic.Int64 }

func (v *boundIdentityValue) LogValue() slog.Value { return slog.Int64Value(v.evaluations.Add(1)) }

func TestRuntimeEnvelopePreservesBoundLogValues(t *testing.T) {
	var output bytes.Buffer
	value := &boundIdentityValue{}
	logger := JSONLogger(&output).WithGroup("worker").With("bound", value)
	logger.Info("first")
	logger.Info("second")
	if value.evaluations.Load() != 1 || bytes.Count(output.Bytes(), []byte(`"bound":1`)) != 2 {
		t.Fatal("bound LogValuer was reevaluated")
	}
}

func TestRuntimeIdentityConcurrentEventFamiliesAndLoggerGroups(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewJSONLogger(&output, "0.3.3")
	if err != nil {
		t.Fatal(err)
	}
	families := []string{"gateway_start", "gateway_shutdown", "request_terminal", "local_rejection",
		"protocol_conversion_omitted_fields", "request_panic", "request_audit_write_failure",
		"budget_settlement_failure", "http_server_error", "statement_sync_failure", "usage_log_write_failure"}
	var wg sync.WaitGroup
	for n := 0; n < 128; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, family := range families {
				logger.With("component", "gateway").Warn("event", "event", family, "requestId", strings.Repeat("a", 32))
			}
		}()
	}
	wg.Wait()
	logger.With("service", "forged", "event_id", "forged").WithGroup("worker").With("bound", 1).
		WithGroup("task").Info("grouped", "live", 2)
	logger.Info("inline group", slog.Group("", "service", "forged", "event_id", "forged", "kept", 3))
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	seen := map[string]bool{}
	instance := ""
	for _, line := range lines {
		var event struct {
			Service serviceMetadata `json:"service"`
			ID      string          `json:"event_id"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if instance == "" {
			instance = event.Service.Instance.ID
		}
		if event.Service.Name != "tidemux" || event.Service.Version != "0.3.3" || len(instance) != 32 ||
			event.Service.Instance.ID != instance || len(event.ID) != 49 || !strings.HasPrefix(event.ID, instance+"-") || seen[event.ID] {
			t.Fatalf("missing or colliding envelope: %s", line)
		}
		seen[event.ID] = true
	}
	if len(lines) != 128*len(families)+2 {
		t.Fatalf("lost concurrent events: %d", len(lines))
	}
	var grouped map[string]any
	if err := json.Unmarshal(lines[len(lines)-2], &grouped); err != nil {
		t.Fatal(err)
	}
	worker := grouped["worker"].(map[string]any)
	if worker["bound"] != float64(1) || worker["task"].(map[string]any)["live"] != float64(2) || grouped["msg"] != "grouped" {
		t.Fatalf("logger grouping lost fields: %s", lines[len(lines)-2])
	}
	if bytes.Contains(output.Bytes(), []byte("forged")) {
		t.Fatal("reserved identity attributes overwritten")
	}
}

func TestRuntimeIdentitySharedWithinProcessAndNewNamespaces(t *testing.T) {
	first, err := processRuntimeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	second, err := processRuntimeIdentity()
	if err != nil || first != second {
		t.Fatal("logger construction replaced process identity")
	}
	other, err := newRuntimeIdentity(rand.Reader)
	if err != nil || first.instance == other.instance {
		t.Fatal("new process namespace collided")
	}
	var output bytes.Buffer
	newJSONLogger(&output, "", first).Info("missing build metadata")
	var event struct {
		Service serviceMetadata `json:"service"`
	}
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Service.Version != "unknown" {
		t.Fatal("missing version did not use documented value")
	}
}

func TestRuntimeIdentityFailureAndCounterExhaustionNeverReuseIDs(t *testing.T) {
	identity, err := newRuntimeIdentity(io.LimitReader(strings.NewReader("private-entropy-error"), 15))
	if identity != nil || !errors.Is(err, ErrRuntimeIdentity) || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe entropy failure: %v", err)
	}
	identity = &runtimeIdentity{instance: strings.Repeat("a", 32)}
	identity.sequence.Store(^uint64(0) - 1)
	if _, err := identity.eventID(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := newJSONLogger(&output, "0.3.3", identity)
	for n := 0; n < 3; n++ {
		err := logger.Handler().Handle(context.Background(), slog.Record{})
		if !errors.Is(err, ErrEventIdentityExhausted) {
			t.Fatalf("exhausted ID reused: %v", err)
		}
	}
	if output.Len() != 0 {
		t.Fatal("exhausted counter emitted a colliding event")
	}
}
