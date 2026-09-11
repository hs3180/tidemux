package ledger

import (
	"context"
	"encoding/hex"
	"errors"
	"time"
)

// Diagnostic is a local rejection, never an upstream attempt or cost record.
// Endpoint and method are categories; URLs, headers, and bodies are excluded.
type Diagnostic struct {
	ID          string `json:"id"`
	TimestampMS int64  `json:"timestamp_ms"`
	Protocol    string `json:"protocol"`
	Method      string `json:"method"`
	Endpoint    string `json:"endpoint"`
	Status      int    `json:"status"`
	ErrorCode   string `json:"error_code"`
}

func (l *Ledger) initDiagnostics(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS local_diagnostics (
 id TEXT PRIMARY KEY, timestamp_ms INTEGER NOT NULL, protocol TEXT NOT NULL,
 method TEXT NOT NULL, endpoint TEXT NOT NULL, status INTEGER NOT NULL, error_code TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS local_diagnostics_time ON local_diagnostics(timestamp_ms);`)
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
	_, err = l.db.ExecContext(ctx, `INSERT INTO local_diagnostics VALUES (?,?,?,?,?,?,?)`, d.ID, d.TimestampMS, d.Protocol, d.Method, d.Endpoint, d.Status, d.ErrorCode)
	return err
}

func (l *Ledger) RecentDiagnostics(ctx context.Context, limit int) ([]Diagnostic, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("limit must be 1..1000")
	}
	rows, err := l.db.QueryContext(ctx, `SELECT id,timestamp_ms,protocol,method,endpoint,status,error_code FROM local_diagnostics ORDER BY timestamp_ms DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Diagnostic{}
	for rows.Next() {
		var d Diagnostic
		if err = rows.Scan(&d.ID, &d.TimestampMS, &d.Protocol, &d.Method, &d.Endpoint, &d.Status, &d.ErrorCode); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
