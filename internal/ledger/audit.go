package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// Audit is one terminal attempt. Nil counts and cost mean unknown, never free.
// Request and event data are committed atomically. No content or credentials belong here.
type Audit struct {
	ID               string          `json:"id"`
	TimestampMS      int64           `json:"timestamp_ms"`
	Protocol         string          `json:"protocol"`
	Upstream         string          `json:"upstream"`
	Model            string          `json:"model"`
	Status           string          `json:"status"`
	ErrorCode        string          `json:"error_code,omitempty"`
	InputTokens      *int64          `json:"input_tokens"`
	OutputTokens     *int64          `json:"output_tokens"`
	CacheReadTokens  *int64          `json:"cache_read_tokens"`
	CacheWriteTokens *int64          `json:"cache_write_tokens"`
	EstimatedCost    *float64        `json:"estimated_cost"`
	CostSource       string          `json:"cost_source,omitempty"`
	Currency         string          `json:"currency,omitempty"`
	PriceSnapshot    json.RawMessage `json:"price_snapshot,omitempty"`
	LatencyMS        int64           `json:"latency_ms"`
	QueueMS          int64           `json:"queue_ms"`
	Events           []string        `json:"events"`
}

func (l *Ledger) initAudit(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS request_audit (
 id TEXT PRIMARY KEY, timestamp_ms INTEGER NOT NULL, protocol TEXT NOT NULL,
 upstream TEXT NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL,
 input_tokens INTEGER, output_tokens INTEGER, estimated_cost REAL, currency TEXT,
 record_json TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS audit_events (
 request_id TEXT NOT NULL REFERENCES request_audit(id), ordinal INTEGER NOT NULL,
 type TEXT NOT NULL, PRIMARY KEY(request_id,ordinal));
 CREATE INDEX IF NOT EXISTS audit_time ON request_audit(timestamp_ms);`)
	return err
}

func (l *Ledger) AppendAudit(a Audit) error {
	if a.ID == "" || a.TimestampMS <= 0 || (a.Protocol != "openai" && a.Protocol != "anthropic") || a.Upstream == "" || a.Model == "" {
		return errors.New("invalid audit identity")
	}
	if a.Status != "ok" && a.Status != "error" && a.Status != "canceled" {
		return errors.New("invalid audit status")
	}
	for _, n := range []*int64{a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens} {
		if n != nil && *n < 0 {
			return errors.New("invalid audit usage")
		}
	}
	if a.EstimatedCost != nil && (*a.EstimatedCost < 0 || math.IsNaN(*a.EstimatedCost) || math.IsInf(*a.EstimatedCost, 0) || a.Currency == "") {
		return errors.New("invalid audit cost")
	}
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	// A canceled HTTP context must not cancel the terminal audit write.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO request_audit
 (id,timestamp_ms,protocol,upstream,model,status,input_tokens,output_tokens,estimated_cost,currency,record_json)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`, a.ID, a.TimestampMS, a.Protocol, a.Upstream, a.Model, a.Status, a.InputTokens, a.OutputTokens, a.EstimatedCost, a.Currency, string(data))
	if err != nil {
		return err
	}
	for i, event := range a.Events {
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events VALUES (?,?,?)`, a.ID, i, event); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (l *Ledger) Recent(ctx context.Context, limit int) ([]Audit, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("limit must be 1..1000")
	}
	rows, err := l.db.QueryContext(ctx, `SELECT record_json FROM request_audit ORDER BY timestamp_ms DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Audit{}
	for rows.Next() {
		var s string
		var a Audit
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(s), &a); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
