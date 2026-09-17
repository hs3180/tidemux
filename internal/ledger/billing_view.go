package ledger

import (
	"context"
	"encoding/json"
	"fmt"
)

// UsageSummary counts current gateway requests and known token usage. Unknown
// counts remain explicit: a known token subtotal must never imply free usage.
type UsageSummary struct {
	RequestCount          int64 `json:"request_count"`
	SuccessfulRequests    int64 `json:"successful_requests"`
	ErrorRequests         int64 `json:"error_requests"`
	CanceledRequests      int64 `json:"canceled_requests"`
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	UnknownInputRequests  int64 `json:"unknown_input_requests"`
	UnknownOutputRequests int64 `json:"unknown_output_requests"`
}

// BillingUsage returns a read-only aggregate of request audits in [fromMS, toMS).
// A zero bound is open-ended. It excludes legacy requests and local rejections.
func (l *Ledger) BillingUsage(ctx context.Context, fromMS, toMS int64) (UsageSummary, error) {
	if err := validatePeriod(fromMS, toMS); err != nil {
		return UsageSummary{}, err
	}
	where, args := periodWhere("timestamp_ms", fromMS, toMS)
	var usage UsageSummary
	err := l.db.QueryRowContext(ctx, `SELECT
	 COUNT(*),
	 COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN status='canceled' THEN 1 ELSE 0 END),0),
	 COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
	 COUNT(*)-COUNT(input_tokens), COUNT(*)-COUNT(output_tokens)
	 FROM request_audit`+where, args...).Scan(
		&usage.RequestCount, &usage.SuccessfulRequests, &usage.ErrorRequests,
		&usage.CanceledRequests, &usage.InputTokens, &usage.OutputTokens,
		&usage.UnknownInputRequests, &usage.UnknownOutputRequests,
	)
	if err != nil {
		return UsageSummary{}, fmt.Errorf("read billing usage: %w", err)
	}
	return usage, nil
}

// RequestDetails returns every request audit in [fromMS, toMS), newest first.
// A zero bound is open-ended. Nullable usage and cost are preserved, and this
// view has no implicit row limit that could hide requests from a billing period.
func (l *Ledger) RequestDetails(ctx context.Context, fromMS, toMS int64) ([]Audit, error) {
	if err := validatePeriod(fromMS, toMS); err != nil {
		return nil, err
	}
	where, args := periodWhere("timestamp_ms", fromMS, toMS)
	rows, err := l.db.QueryContext(ctx, `SELECT record_json FROM request_audit`+where+` ORDER BY timestamp_ms DESC,id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("read billing request details: %w", err)
	}
	defer rows.Close()
	result := []Audit{}
	for rows.Next() {
		var encoded string
		var audit Audit
		if err = rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan billing request detail: %w", err)
		}
		if err = json.Unmarshal([]byte(encoded), &audit); err != nil {
			return nil, fmt.Errorf("decode billing request detail: %w", err)
		}
		result = append(result, audit)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read billing request details: %w", err)
	}
	return result, nil
}
