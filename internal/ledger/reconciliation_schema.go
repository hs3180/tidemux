package ledger

import (
	"context"
	"fmt"
)

// Triggers maintain reconciliation in the same transaction as each source write.
// The source views are implementation helpers for insertion and one-time backfill;
// billing reads only the materialized tables.
func (l *Ledger) initReconciliation(ctx context.Context) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
 CREATE TABLE IF NOT EXISTS token_comparisons (
 request_id TEXT PRIMARY KEY REFERENCES request_audit(id), tokenizer TEXT NOT NULL,
 tokenizer_version TEXT NOT NULL, estimated_input INTEGER NOT NULL, estimated_output INTEGER NOT NULL,
 measured_at_ms INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS balance_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, observed_at_ms INTEGER NOT NULL, currency TEXT NOT NULL,
 total REAL NOT NULL, granted REAL NOT NULL, topped_up REAL NOT NULL,
 UNIQUE(observed_at_ms,currency));
 CREATE TABLE IF NOT EXISTS supplier_statement_lines (
 id INTEGER PRIMARY KEY AUTOINCREMENT, period_start_ms INTEGER NOT NULL, period_end_ms INTEGER NOT NULL,
 currency TEXT NOT NULL, amount REAL NOT NULL, request_id TEXT, model TEXT NOT NULL,
 imported_at_ms INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS statement_period ON supplier_statement_lines(period_start_ms,period_end_ms);
 CREATE INDEX IF NOT EXISTS statement_request ON supplier_statement_lines(request_id);
 CREATE TABLE IF NOT EXISTS statement_imports (
 fingerprint TEXT PRIMARY KEY, imported_at_ms INTEGER NOT NULL, row_count INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS statement_import_sources (
 source TEXT PRIMARY KEY, fingerprint TEXT NOT NULL REFERENCES statement_imports(fingerprint));
 CREATE TABLE IF NOT EXISTS statement_sync (
 id INTEGER PRIMARY KEY CHECK(id=1), last_attempt_ms INTEGER NOT NULL, last_success_ms INTEGER,
 files_imported INTEGER NOT NULL, lines_imported INTEGER NOT NULL, failures INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS reconciliation_schema (version INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS reconciliation_requests (
 request_id TEXT PRIMARY KEY, timestamp_ms INTEGER NOT NULL, currency TEXT NOT NULL,
 model TEXT NOT NULL, estimated_cost REAL, input_tokens INTEGER, output_tokens INTEGER,
 statement_lines INTEGER NOT NULL, supplier_amount REAL NOT NULL,
 tokenizer_compared INTEGER NOT NULL, tokenizer_mismatch INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS reconciliation_requests_time ON reconciliation_requests(timestamp_ms);
 CREATE TABLE IF NOT EXISTS reconciliation_statements (
 statement_id INTEGER PRIMARY KEY, period_start_ms INTEGER NOT NULL, period_end_ms INTEGER NOT NULL,
 currency TEXT NOT NULL, amount REAL NOT NULL, request_id TEXT, model TEXT NOT NULL,
 status TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS reconciliation_statements_period ON reconciliation_statements(period_start_ms,period_end_ms);
 CREATE INDEX IF NOT EXISTS reconciliation_statements_request ON reconciliation_statements(request_id);
 CREATE TABLE IF NOT EXISTS reconciliation_balance_intervals (
 snapshot_id INTEGER PRIMARY KEY, from_ms INTEGER, to_ms INTEGER NOT NULL, currency TEXT NOT NULL,
 net_change REAL, local_estimated REAL NOT NULL, unknown_cost_requests INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS reconciliation_balances_period ON reconciliation_balance_intervals(from_ms,to_ms);
 CREATE VIEW IF NOT EXISTS reconciliation_statement_source AS
 SELECT s.id AS statement_id,s.period_start_ms,s.period_end_ms,s.currency,s.amount,s.request_id,s.model,
 CASE WHEN s.request_id IS NULL OR s.request_id='' THEN 'missing_request_id'
 WHEN a.id IS NULL THEN 'request_not_found'
 WHEN COALESCE(a.currency,'')<>s.currency THEN 'currency_mismatch'
 WHEN s.model<>'' AND s.model<>a.model THEN 'model_mismatch'
 WHEN a.timestamp_ms<s.period_start_ms OR a.timestamp_ms>=s.period_end_ms THEN 'outside_request_period'
 ELSE 'matched' END AS status
 FROM supplier_statement_lines s LEFT JOIN request_audit a ON a.id=s.request_id;
 CREATE VIEW IF NOT EXISTS reconciliation_request_source AS
 SELECT a.id AS request_id,a.timestamp_ms,COALESCE(a.currency,'') AS currency,a.model,a.estimated_cost,a.input_tokens,a.output_tokens,
 (SELECT COUNT(*) FROM reconciliation_statement_source s WHERE s.request_id=a.id AND s.status='matched') AS statement_lines,
 COALESCE((SELECT SUM(s.amount) FROM reconciliation_statement_source s WHERE s.request_id=a.id AND s.status='matched'),0) AS supplier_amount,
 CASE WHEN c.request_id IS NULL THEN 0 ELSE 1 END AS tokenizer_compared,
 CASE WHEN c.request_id IS NOT NULL AND (c.estimated_input<>COALESCE(a.input_tokens,-1) OR c.estimated_output<>COALESCE(a.output_tokens,-1)) THEN 1 ELSE 0 END AS tokenizer_mismatch
 FROM request_audit a LEFT JOIN token_comparisons c ON c.request_id=a.id;
 CREATE VIEW IF NOT EXISTS reconciliation_balance_source AS
 SELECT b.id AS snapshot_id,p.observed_at_ms AS from_ms,b.observed_at_ms AS to_ms,b.currency,
 b.total-p.total AS net_change,
 COALESCE((SELECT SUM(a.estimated_cost) FROM request_audit a WHERE a.currency=b.currency AND a.timestamp_ms>=p.observed_at_ms AND a.timestamp_ms<b.observed_at_ms),0) AS local_estimated,
 (SELECT COUNT(*) FROM request_audit a WHERE a.currency=b.currency AND a.timestamp_ms>=p.observed_at_ms AND a.timestamp_ms<b.observed_at_ms AND a.estimated_cost IS NULL) AS unknown_cost_requests
 FROM balance_snapshots b LEFT JOIN balance_snapshots p ON p.id=(SELECT prev.id FROM balance_snapshots prev WHERE prev.currency=b.currency AND prev.observed_at_ms<b.observed_at_ms ORDER BY prev.observed_at_ms DESC LIMIT 1);
 CREATE TRIGGER IF NOT EXISTS reconciliation_audit_insert AFTER INSERT ON request_audit BEGIN
 INSERT OR REPLACE INTO reconciliation_requests SELECT * FROM reconciliation_request_source WHERE request_id=NEW.id;
 INSERT OR REPLACE INTO reconciliation_statements SELECT * FROM reconciliation_statement_source WHERE request_id=NEW.id;
 UPDATE reconciliation_balance_intervals SET local_estimated=local_estimated+COALESCE(NEW.estimated_cost,0),unknown_cost_requests=unknown_cost_requests+(NEW.estimated_cost IS NULL) WHERE currency=NEW.currency AND from_ms<=NEW.timestamp_ms AND to_ms>NEW.timestamp_ms;
 END;
 CREATE TRIGGER IF NOT EXISTS reconciliation_token_insert AFTER INSERT ON token_comparisons BEGIN
 UPDATE reconciliation_requests SET tokenizer_compared=1,tokenizer_mismatch=(NEW.estimated_input<>COALESCE(input_tokens,-1) OR NEW.estimated_output<>COALESCE(output_tokens,-1)) WHERE request_id=NEW.request_id;
 END;
 CREATE TRIGGER IF NOT EXISTS reconciliation_statement_insert AFTER INSERT ON supplier_statement_lines BEGIN
 INSERT OR REPLACE INTO reconciliation_statements SELECT * FROM reconciliation_statement_source WHERE statement_id=NEW.id;
 UPDATE reconciliation_requests SET statement_lines=statement_lines+1,supplier_amount=supplier_amount+NEW.amount WHERE request_id=NEW.request_id AND (SELECT status FROM reconciliation_statements WHERE statement_id=NEW.id)='matched';
 END;
 CREATE TRIGGER IF NOT EXISTS reconciliation_balance_insert AFTER INSERT ON balance_snapshots BEGIN
 INSERT OR REPLACE INTO reconciliation_balance_intervals SELECT * FROM reconciliation_balance_source WHERE snapshot_id=NEW.id OR snapshot_id=(SELECT id FROM balance_snapshots WHERE currency=NEW.currency AND observed_at_ms>NEW.observed_at_ms ORDER BY observed_at_ms LIMIT 1);
 END;
 `)
	if err != nil {
		return fmt.Errorf("initialize automatic reconciliation: %w", err)
	}
	var initialized int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reconciliation_schema WHERE version=1`).Scan(&initialized); err != nil {
		return err
	}
	if initialized == 0 {
		if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO reconciliation_requests SELECT * FROM reconciliation_request_source;
 INSERT OR REPLACE INTO reconciliation_statements SELECT * FROM reconciliation_statement_source;
 INSERT OR REPLACE INTO reconciliation_balance_intervals SELECT * FROM reconciliation_balance_source;
 INSERT INTO reconciliation_schema(version) VALUES(1);`); err != nil {
			return fmt.Errorf("backfill automatic reconciliation: %w", err)
		}
	}
	return tx.Commit()
}
