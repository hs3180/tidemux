package ledger

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/hs3180/tidemux/internal/diagnostics"
)

// Diagnostic is a local rejection, never an upstream attempt or cost record.
// Endpoint and method are categories; URLs, headers, and bodies are excluded.
type Diagnostic struct {
	ID              string                       `json:"id"`
	TimestampMS     int64                        `json:"timestamp_ms"`
	Protocol        string                       `json:"protocol"`
	Method          string                       `json:"method"`
	Endpoint        string                       `json:"endpoint"`
	Status          int                          `json:"status"`
	ErrorCode       string                       `json:"error_code"`
	RoutingDecision *diagnostics.RoutingDecision `json:"routing_decision,omitempty"`
}

func (l *Ledger) initDiagnostics(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS local_diagnostics (
 id TEXT PRIMARY KEY, timestamp_ms INTEGER NOT NULL, protocol TEXT NOT NULL,
 method TEXT NOT NULL, endpoint TEXT NOT NULL, status INTEGER NOT NULL, error_code TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS local_diagnostics_time ON local_diagnostics(timestamp_ms);
 CREATE TABLE IF NOT EXISTS local_routing_decisions (
 request_id TEXT PRIMARY KEY REFERENCES local_diagnostics(id), decision_json TEXT NOT NULL);`)
	return err
}

func (l *Ledger) AppendDiagnostic(d Diagnostic) error {
	id, err := hex.DecodeString(d.ID)
	if err != nil || len(id) != 16 || d.TimestampMS <= 0 || (d.Protocol != "openai" && d.Protocol != "anthropic") {
		return errors.New("invalid diagnostic identity")
	}
	if d.Method != "GET" && d.Method != "POST" && d.Method != "OTHER" {
		return errors.New("invalid diagnostic method")
	}
	switch d.Endpoint {
	case "models", "messages", "chat_completions", "unsupported":
	default:
		return errors.New("invalid diagnostic endpoint")
	}
	if d.Status < 400 || d.Status > 599 || len(d.ErrorCode) == 0 || len(d.ErrorCode) > 80 {
		return errors.New("invalid diagnostic error")
	}
	for _, c := range d.ErrorCode {
		if c < 'a' || c > 'z' {
			if c != '_' {
				return errors.New("invalid diagnostic code")
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	decision := d.RoutingDecision.Normalize()
	if decision == nil {
		_, err = l.db.ExecContext(ctx, `INSERT INTO local_diagnostics VALUES (?,?,?,?,?,?,?)`, d.ID, d.TimestampMS, d.Protocol, d.Method, d.Endpoint, d.Status, d.ErrorCode)
		return err
	}
	// Preserve the historical table's write shape for older binaries. Sparse
	// decision details and their rejection commit together in an additive table.
	data, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO local_diagnostics VALUES (?,?,?,?,?,?,?)`, d.ID, d.TimestampMS, d.Protocol, d.Method, d.Endpoint, d.Status, d.ErrorCode); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO local_routing_decisions VALUES (?,?)`, d.ID, string(data)); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Ledger) RecentDiagnostics(ctx context.Context, limit int) ([]Diagnostic, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("limit must be 1..1000")
	}
	var detailTable int
	if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='local_routing_decisions'`).Scan(&detailTable); err != nil {
		return nil, err
	}
	query := `SELECT d.id,d.timestamp_ms,d.protocol,d.method,d.endpoint,d.status,d.error_code,NULL FROM local_diagnostics d ORDER BY d.timestamp_ms DESC,d.id DESC LIMIT ?`
	if detailTable > 0 {
		query = `SELECT d.id,d.timestamp_ms,d.protocol,d.method,d.endpoint,d.status,d.error_code,r.decision_json FROM local_diagnostics d LEFT JOIN local_routing_decisions r ON r.request_id=d.id ORDER BY d.timestamp_ms DESC,d.id DESC LIMIT ?`
	}
	rows, err := l.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Diagnostic{}
	for rows.Next() {
		var d Diagnostic
		var details sql.NullString
		if err = rows.Scan(&d.ID, &d.TimestampMS, &d.Protocol, &d.Method, &d.Endpoint, &d.Status, &d.ErrorCode, &details); err != nil {
			return nil, err
		}
		if details.Valid {
			if err = json.Unmarshal([]byte(details.String), &d.RoutingDecision); err != nil {
				return nil, err
			}
			d.RoutingDecision = d.RoutingDecision.Normalize()
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
