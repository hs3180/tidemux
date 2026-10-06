package ledger

import (
	"context"
	"encoding/json"
)

// UsageAudit is a committed audit and its matched statement snapshot. Revision
// is the largest matched immutable statement ID, not a cost or timestamp.
type UsageAudit struct {
	RowID             int64
	Audit             Audit
	StatementRevision int64
	StatementLines    int64
	SupplierAmount    *float64
}

// UsageAudits reads a bounded page using SQLite's append row identity. It never
// changes audit, reconciliation or budget state. The caller uses a separate
// read-only connection so optional export I/O cannot hold the request writer.
func (l *Ledger) UsageAudits(ctx context.Context, afterRowID int64, limit int) ([]UsageAudit, error) {
	return l.usageAudits(ctx, `a.rowid>? ORDER BY a.rowid LIMIT ?`, afterRowID, limit)
}

func (l *Ledger) usageAudits(ctx context.Context, where string, args ...any) ([]UsageAudit, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT a.rowid,a.record_json,
 COALESCE((SELECT MAX(s.statement_id) FROM reconciliation_statements s WHERE s.request_id=a.id AND s.status='matched'),0),
 COALESCE((SELECT COUNT(*) FROM reconciliation_statements s WHERE s.request_id=a.id AND s.status='matched'),0),
 (SELECT SUM(s.amount) FROM reconciliation_statements s WHERE s.request_id=a.id AND s.status='matched')
 FROM request_audit a WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []UsageAudit{}
	bytesRead := 0
	for rows.Next() {
		var row UsageAudit
		var data string
		if err := rows.Scan(&row.RowID, &data, &row.StatementRevision, &row.StatementLines, &row.SupplierAmount); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &row.Audit); err != nil {
			return nil, err
		}
		result = append(result, row)
		bytesRead += len(data)
		if bytesRead >= 1<<20 {
			break // One oversized audit at most; do not load an unbounded page.
		}
	}
	return result, rows.Err()
}

type UsageStatement struct {
	ID        int64
	RequestID string
}

// UsageStatements also advances past unmatched charges: they never become
// fabricated request charges. A subsequently inserted audit reads its full
// matched snapshot, including statements imported before the request existed.
func (l *Ledger) UsageStatements(ctx context.Context, afterID int64, limit int) ([]UsageStatement, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT statement_id,COALESCE(request_id,'') FROM reconciliation_statements WHERE statement_id>? ORDER BY statement_id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []UsageStatement{}
	for rows.Next() {
		var s UsageStatement
		if err := rows.Scan(&s.ID, &s.RequestID); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func (l *Ledger) UsageAuditByID(ctx context.Context, id string) ([]UsageAudit, error) {
	return l.usageAudits(ctx, `a.id=?`, id)
}
