package usage

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

type Record struct {
	SchemaVersion          int      `json:"schema_version"`
	Source                 string   `json:"source"`
	SourceID               string   `json:"source_id"`
	RequestID              string   `json:"request_id"`
	Revision               int64    `json:"revision"`
	Timestamp              string   `json:"timestamp"`
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

func makeRecord(sourceID string, row ledger.UsageAudit) Record {
	a := row.Audit
	r := Record{SchemaVersion: 1, Source: "tidemux", SourceID: sourceID, RequestID: a.ID, Revision: row.StatementRevision,
		Timestamp: time.UnixMilli(a.TimestampMS).UTC().Format(time.RFC3339Nano), Provider: a.ProviderRef, Model: a.Model, Protocol: a.Protocol, Outcome: a.Status,
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
