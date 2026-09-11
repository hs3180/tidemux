package ledger

import (
	"context"
	"fmt"
)

// Summary is a read-only, derived view over the append-only ledger. Costs and
// ratios are calculated from ledger_requests; no snapshot or second source of
// truth is persisted.
type Summary struct {
	RequestCount           int64
	SuccessfulRequestCount int64
	ErrorCount             int64
	TotalTokens            int64
	SpentCNY               float64
	CacheHitTokens         int64
	CacheMissTokens        int64
	CacheHitRatio          float64
	AvgFirstTokenMS        float64
	AvgTotalLatencyMS      float64
	QueueWaitCount         int64
	RateLimitCount         int64
	PeakThrottleCount      int64
	BudgetCappedCount      int64
}

// Summarize returns metrics for [fromMS, toMS). A zero bound is open-ended;
// callers can therefore use it for all-time or one-sided diagnostics.
func (l *Ledger) Summarize(ctx context.Context, fromMS, toMS int64) (Summary, error) {
	if l == nil {
		return Summary{}, fmt.Errorf("ledger view requires a ledger")
	}
	where := ""
	args := make([]any, 0, 2)
	if fromMS > 0 {
		where += "req_ts >= ?"
		args = append(args, fromMS)
	}
	if toMS > 0 {
		if where != "" {
			where += " AND "
		}
		where += "req_ts < ?"
		args = append(args, toMS)
	}
	if where != "" {
		where = " WHERE " + where
	}

	var s Summary
	query := `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status <> 'error' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(CASE WHEN status <> 'error' THEN est_cost_cny ELSE 0 END), 0),
		COALESCE(SUM(cache_hit_tokens), 0),
		COALESCE(SUM(cache_miss_tokens), 0),
		COALESCE(AVG(latency_first_token_ms), 0),
		COALESCE(AVG(latency_total_ms), 0)
		FROM ledger_requests` + where
	if err := l.db.QueryRowContext(ctx, query, args...).Scan(
		&s.RequestCount, &s.SuccessfulRequestCount, &s.ErrorCount,
		&s.TotalTokens, &s.SpentCNY, &s.CacheHitTokens, &s.CacheMissTokens,
		&s.AvgFirstTokenMS, &s.AvgTotalLatencyMS,
	); err != nil {
		return Summary{}, fmt.Errorf("summarize ledger requests: %w", err)
	}
	cacheTotal := s.CacheHitTokens + s.CacheMissTokens
	if cacheTotal > 0 {
		s.CacheHitRatio = float64(s.CacheHitTokens) / float64(cacheTotal)
	}

	eventQuery := `SELECT type, COUNT(*) FROM ledger_events e
		JOIN ledger_requests r ON r.id = e.req_id` + where + ` GROUP BY type`
	rows, err := l.db.QueryContext(ctx, eventQuery, args...)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize ledger events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventType string
		var count int64
		if err := rows.Scan(&eventType, &count); err != nil {
			return Summary{}, fmt.Errorf("scan ledger event summary: %w", err)
		}
		switch eventType {
		case "queue_wait":
			s.QueueWaitCount = count
		case "rate_limit_429":
			s.RateLimitCount = count
		case "peak_throttle":
			s.PeakThrottleCount = count
		case "budget_capped":
			s.BudgetCappedCount = count
		}
	}
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("read ledger event summary: %w", err)
	}
	return s, nil
}
