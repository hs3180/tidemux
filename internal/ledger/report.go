package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// DailyReport is a content-free local read model. It intentionally has no
// prompt, completion, key, endpoint or request header fields.
type DailyReport struct {
	ID                  int64    `json:"id"`
	Day                 string   `json:"day"`
	Timezone            string   `json:"timezone"`
	RequestCount        int64    `json:"request_count"`
	FailureCount        int64    `json:"failure_count"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	CacheWriteTokens    int64    `json:"cache_write_tokens"`
	EstimatedCost       *float64 `json:"estimated_cost"`
	Currency            string   `json:"currency,omitempty"`
	UnknownCostRequests int64    `json:"unknown_cost_requests"`
	TokenizerMismatches *int64   `json:"tokenizer_mismatches"`
	UnmatchedStatements *int64   `json:"unmatched_statement_lines"`
	BalanceNetChange    *float64 `json:"balance_net_change"`
	BudgetRemaining     *float64 `json:"budget_remaining"`
}

// ReportBudget is a reporting input, independent of gateway admission policy.
type ReportBudget struct {
	Currency   string
	DailyLimit float64
}

type ReportDelivery struct {
	ID        int64  `json:"id"`
	ReportID  int64  `json:"report_id"`
	Channel   string `json:"channel"`
	Status    string `json:"status"`
	Attempts  int64  `json:"attempts"`
	ErrorCode string `json:"error_code,omitempty"`
}

func (l *Ledger) initReports(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS daily_reports (
 id INTEGER PRIMARY KEY AUTOINCREMENT, day TEXT NOT NULL, timezone TEXT NOT NULL,
 report_json TEXT NOT NULL, created_at_ms INTEGER NOT NULL, UNIQUE(day,timezone));
 CREATE TABLE IF NOT EXISTS report_deliveries (
 id INTEGER PRIMARY KEY AUTOINCREMENT, report_id INTEGER NOT NULL REFERENCES daily_reports(id),
 channel TEXT NOT NULL CHECK(channel IN ('macos','smtp')), status TEXT NOT NULL CHECK(status IN ('pending','sent','failed')),
 attempts INTEGER NOT NULL, error_code TEXT NOT NULL, updated_at_ms INTEGER NOT NULL,
 UNIQUE(report_id,channel));`)
	return err
}

func (l *Ledger) GenerateDailyReport(ctx context.Context, day time.Time, timezone string, budget ReportBudget) (DailyReport, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return DailyReport{}, errors.New("invalid report timezone")
	}
	local := time.Date(day.In(loc).Year(), day.In(loc).Month(), day.In(loc).Day(), 0, 0, 0, 0, loc)
	next := local.AddDate(0, 0, 1)
	r := DailyReport{Day: local.Format("2006-01-02"), Timezone: timezone}
	var estimated float64
	var currency string
	err = l.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN status<>'ok' THEN 1 ELSE 0 END),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
 COALESCE(SUM(CASE WHEN estimated_cost IS NULL THEN 1 ELSE 0 END),0), COALESCE(SUM(estimated_cost),0), COALESCE(MAX(currency),'') FROM request_audit WHERE timestamp_ms>=? AND timestamp_ms<?`, local.UnixMilli(), next.UnixMilli()).Scan(&r.RequestCount, &r.FailureCount, &r.InputTokens, &r.OutputTokens, &r.UnknownCostRequests, &estimated, &currency)
	if err != nil {
		return r, err
	}
	if currency != "" {
		r.EstimatedCost = &estimated
		r.Currency = currency
	}
	if err := l.enrichDailyReport(ctx, &r, local, next, budget); err != nil {
		return r, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	_, err = l.db.ExecContext(ctx, `INSERT INTO daily_reports(day,timezone,report_json,created_at_ms) VALUES (?,?,?,?) ON CONFLICT(day,timezone) DO UPDATE SET report_json=excluded.report_json,created_at_ms=excluded.created_at_ms`, r.Day, r.Timezone, string(b), time.Now().UnixMilli())
	if err != nil {
		return r, err
	}
	if err := l.db.QueryRowContext(ctx, `SELECT id FROM daily_reports WHERE day=? AND timezone=?`, r.Day, r.Timezone).Scan(&r.ID); err != nil {
		return r, err
	}
	return r, nil
}

func (l *Ledger) ListDailyReports(ctx context.Context, limit int) ([]DailyReport, error) {
	if limit < 1 || limit > 3660 {
		return nil, errors.New("report limit must be 1..3660")
	}
	rows, e := l.db.QueryContext(ctx, `SELECT id,report_json FROM daily_reports ORDER BY day DESC LIMIT ?`, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []DailyReport{}
	for rows.Next() {
		var r DailyReport
		var b string
		var id int64
		if e = rows.Scan(&id, &b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &r); e != nil {
			return nil, e
		}
		r.ID = id
		out = append(out, r)
	}
	return out, rows.Err()
}

func (l *Ledger) RecordDelivery(ctx context.Context, reportID int64, channel, status, code string) error {
	if reportID < 1 || (channel != "macos" && channel != "smtp") || (status != "sent" && status != "failed") {
		return errors.New("invalid report delivery")
	}
	_, e := l.db.ExecContext(ctx, `INSERT INTO report_deliveries(report_id,channel,status,attempts,error_code,updated_at_ms) VALUES (?,?,?,?,?,?) ON CONFLICT(report_id,channel) DO UPDATE SET status=excluded.status,attempts=report_deliveries.attempts+1,error_code=excluded.error_code,updated_at_ms=excluded.updated_at_ms`, reportID, channel, status, 1, code, time.Now().UnixMilli())
	return e
}

func (l *Ledger) ReportDeliveries(ctx context.Context, reportID int64) ([]ReportDelivery, error) {
	rows, e := l.db.QueryContext(ctx, `SELECT id,report_id,channel,status,attempts,error_code FROM report_deliveries WHERE report_id=? ORDER BY id`, reportID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []ReportDelivery{}
	for rows.Next() {
		var x ReportDelivery
		if e = rows.Scan(&x.ID, &x.ReportID, &x.Channel, &x.Status, &x.Attempts, &x.ErrorCode); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
