package usage

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/ledger"
)

func TestClaudeEnvelopeTokenTotalsAndUnknownCoverage(t *testing.T) {
	count := func(value int64) *int64 { return &value }
	for _, scenario := range []struct {
		name       string
		input      *int64
		output     *int64
		read       *int64
		write      *int64
		compatible bool
	}{
		{"cache-inclusive", count(30), count(4), count(10), count(5), true},
		{"cache-unknown", count(30), count(4), nil, nil, true},
		{"known-zero", count(0), count(0), count(0), count(0), true},
		{"unknown-input", nil, count(4), nil, nil, false},
		{"unknown-output", count(30), nil, nil, nil, false},
		{"invalid-cache", count(30), count(4), count(20), count(11), false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			a := audit("request", "EUR")
			a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens = scenario.input, scenario.output, scenario.read, scenario.write
			r := makeRecord("source", ledger.UsageAudit{Audit: a})
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Timestamp string          `json:"timestamp"`
				RequestID string          `json:"requestId"`
				SessionID string          `json:"sessionId"`
				CostUSD   json.RawMessage `json:"costUSD"`
				Message   struct {
					ID    string           `json:"id"`
					Model string           `json:"model"`
					Usage map[string]int64 `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil || envelope.Timestamp == "" || envelope.RequestID == "" || envelope.Message.ID == "" || envelope.Message.Model != a.Model || len(envelope.CostUSD) != 0 || envelope.SessionID != "" {
				t.Fatalf("invalid envelope: %s %v", data, err)
			}
			if (envelope.Message.Usage != nil) != scenario.compatible {
				t.Fatalf("unknown usage presented as known: %s", data)
			}
			if !scenario.compatible {
				return
			}
			tokens := envelope.Message.Usage
			total := tokens["input_tokens"] + tokens["output_tokens"] + tokens["cache_read_input_tokens"] + tokens["cache_creation_input_tokens"]
			if total != *a.InputTokens+*a.OutputTokens {
				t.Fatalf("cache counted twice: %s", data)
			}
			if a.CacheReadTokens == nil {
				if _, exists := tokens["cache_read_input_tokens"]; exists || r.TideMux.CacheReadTokens != nil {
					t.Fatal("unknown cache count changed to zero")
				}
			}
		})
	}
}

func TestSnapshotIdentityAndAccountingMetadata(t *testing.T) {
	a := audit("request", "EUR")
	a.SessionGroup = strings.Repeat("a", 64)
	a.UsageSource = "provider"
	cost, supplier := 1.5, 2.0
	a.EstimatedCost = &cost
	a.PriceSnapshot = json.RawMessage(`{"source":"https://private-price-source","version":"test-version"}`)
	row := ledger.UsageAudit{Audit: a, StatementRevision: 1, StatementLines: 1, SupplierAmount: &supplier}
	r := makeRecord("first-ledger", row)
	row.StatementRevision = 2
	revised := makeRecord("first-ledger", row)
	other := makeRecord("other-ledger", row)
	if r.RequestID != revised.RequestID || r.Message.ID != revised.Message.ID || r.SessionID != a.SessionGroup || r.Message.ID == other.Message.ID || r.RequestID == other.RequestID {
		t.Fatal("replay or cross-ledger identities collide")
	}
	data, err := json.Marshal(revised)
	if err != nil || strings.Contains(string(data), "private-price-source") || strings.Contains(string(data), "costUSD") {
		t.Fatalf("private prices or billing amounts exposed as consumer costs: %s %v", data, err)
	}
	m := revised.TideMux
	if m.Revision != 2 || *m.EstimatedCost != cost || *m.SupplierAmount != supplier || *m.Currency != "EUR" || m.CostSource != "estimated_from_provider_usage" || *m.PriceVersion != "test-version" {
		t.Fatalf("accounting metadata lost: %+v", m)
	}
}

func TestClaudeTimestampKeepsThreeDigitMilliseconds(t *testing.T) {
	for _, scenario := range []struct {
		millis int64
		want   string
	}{
		{0, "1970-01-01T00:00:00.000Z"},
		{100, "1970-01-01T00:00:00.100Z"},
		{120, "1970-01-01T00:00:00.120Z"},
		{123, "1970-01-01T00:00:00.123Z"},
	} {
		a := audit("request", "")
		a.TimestampMS = scenario.millis
		r := makeRecord("source", ledger.UsageAudit{Audit: a})
		if r.Timestamp != scenario.want {
			t.Fatalf("timestamp %d: %s, want %s", scenario.millis, r.Timestamp, scenario.want)
		}
	}
}
