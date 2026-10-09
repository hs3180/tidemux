package usage

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

// Record uses the Claude log envelope. Separate TideMux metadata preserves
// accounting values and unknowns that external token reports cannot represent.
type Record struct {
	Timestamp string   `json:"timestamp"`
	RequestID string   `json:"requestId"`
	SessionID string   `json:"sessionId,omitempty"`
	Type      string   `json:"type"`
	Message   Message  `json:"message"`
	TideMux   Metadata `json:"tidemux"`
}

type Message struct {
	ID    string      `json:"id"`
	Model string      `json:"model"`
	Usage *TokenUsage `json:"usage,omitempty"`
}

type TokenUsage struct {
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheWriteTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens  *int64 `json:"cache_read_input_tokens,omitempty"`
}

type Metadata struct {
	SchemaVersion          int      `json:"schema_version"`
	Source                 string   `json:"source"`
	SourceID               string   `json:"source_id"`
	RequestID              string   `json:"request_id"`
	Revision               int64    `json:"revision"`
	SessionGroup           *string  `json:"session_group"`
	Provider               string   `json:"provider"`
	Model                  string   `json:"model"`
	Protocol               string   `json:"protocol"`
	Outcome                string   `json:"outcome"`
	UsageSource            string   `json:"usage_source"`
	InputTokens            *int64   `json:"input_tokens"`
	OutputTokens           *int64   `json:"output_tokens"`
	CacheReadTokens        *int64   `json:"cache_read_tokens"`
	CacheWriteTokens       *int64   `json:"cache_write_tokens"`
	EstimatedCost          *float64 `json:"estimated_cost"`
	Currency               *string  `json:"currency"`
	CostSource             string   `json:"cost_source"`
	PriceSource            string   `json:"price_source"`
	PriceVersion           *string  `json:"price_version"`
	SupplierAmount         *float64 `json:"supplier_amount"`
	SupplierStatementLines int64    `json:"supplier_statement_lines"`
	SupplierSource         string   `json:"supplier_source"`
}

func makeMetadata(sourceID string, row ledger.UsageAudit) Metadata {
	a := row.Audit
	r := Metadata{SchemaVersion: 2, Source: "tidemux", SourceID: sourceID, RequestID: a.ID, Revision: row.StatementRevision,
		Provider: a.ProviderRef, Model: a.Model, Protocol: a.Protocol, Outcome: a.Status,
		UsageSource: a.UsageSource, InputTokens: a.InputTokens, OutputTokens: a.OutputTokens, CacheReadTokens: a.CacheReadTokens, CacheWriteTokens: a.CacheWriteTokens,
		EstimatedCost: a.EstimatedCost, CostSource: "unknown", PriceSource: "unknown", SupplierSource: "unknown", SupplierAmount: row.SupplierAmount, SupplierStatementLines: row.StatementLines}
	if row.SupplierAmount != nil && row.StatementLines > 0 {
		r.SupplierSource = "matched_supplier_statement"
	}
	if a.SessionGroup != "" {
		group := a.SessionGroup
		r.SessionGroup = &group
	}
	if a.Currency != "" {
		currency := a.Currency
		r.Currency = &currency
	}
	if r.UsageSource == "" {
		r.UsageSource = "unknown"
		if strings.HasPrefix(a.CostSource, "local_estimated") {
			r.UsageSource = "local_estimate"
		}
		// Historical audit records cannot reliably identify token provenance.
	}
	if a.EstimatedCost != nil {
		r.CostSource = "estimated_from_provider_usage"
		if strings.HasPrefix(a.CostSource, "local_estimated") || r.UsageSource == "local_estimate" {
			r.CostSource = "local_content_estimate"
		}
		if r.UsageSource == "unknown" {
			r.CostSource = "historical_estimate"
		}
	}
	if len(a.PriceSnapshot) > 0 {
		var price struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(a.PriceSnapshot, &price) == nil {
			r.PriceSource = "audit_price_snapshot"
			if price.Version != "" {
				r.PriceVersion = &price.Version
			}
		}
	}
	return r
}

func makeRecord(sourceID string, row ledger.UsageAudit) Record {
	a := row.Audit
	id := "tidemux-" + sourceID + "-" + a.ID
	r := Record{Timestamp: time.UnixMilli(a.TimestampMS).UTC().Format("2006-01-02T15:04:05.000Z"), RequestID: id, SessionID: a.SessionGroup,
		Type: "assistant", Message: Message{ID: id, Model: a.Model}, TideMux: makeMetadata(sourceID, row)}
	// Ledger input includes cache reads/writes; Claude input is uncached input.
	// Missing required counts omit usage instead of fabricating zero usage.
	if a.InputTokens != nil && a.OutputTokens != nil && *a.InputTokens >= 0 && *a.OutputTokens >= 0 {
		input := *a.InputTokens
		for _, count := range []*int64{a.CacheReadTokens, a.CacheWriteTokens} {
			if count != nil {
				if *count < 0 || *count > input {
					return r
				}
				input -= *count
			}
		}
		r.Message.Usage = &TokenUsage{InputTokens: input, OutputTokens: *a.OutputTokens, CacheReadTokens: a.CacheReadTokens, CacheWriteTokens: a.CacheWriteTokens}
	}
	return r
}
