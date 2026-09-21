package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TokenComparison stores counts only; prompts and responses never enter the ledger.
type TokenComparison struct {
	RequestID        string
	Tokenizer        string
	TokenizerVersion string
	EstimatedInput   int64
	EstimatedOutput  int64
	MeasuredAtMS     int64
}

type BalanceSnapshot struct {
	ObservedAtMS int64
	Currency     string
	Total        float64
	Granted      float64
	ToppedUp     float64
}

// StatementLine is one normalized supplier charge. Periods are half-open.
type StatementLine struct {
	PeriodStartMS int64
	PeriodEndMS   int64
	Currency      string
	Amount        float64
	RequestID     string
	Model         string
}

// Reconciliation summarizes automatically maintained state. Difference is only
// available when all selected requests and statement lines are comparable.
// Partial statement periods are counted, but never prorated or included in money totals.
type Reconciliation struct {
	Currency                 string   `json:"currency"`
	Coverage                 string   `json:"coverage"`
	LocalEstimated           float64  `json:"local_estimated"`
	SupplierStatement        float64  `json:"supplier_statement"`
	Difference               *float64 `json:"difference"`
	StatementLines           int64    `json:"statement_lines"`
	PartialStatementLines    int64    `json:"partial_statement_lines"`
	MatchedLines             int64    `json:"matched_lines"`
	UnmatchedLines           int64    `json:"unmatched_lines"`
	MissingStatementRequests int64    `json:"missing_statement_requests"`
	UnknownCostRequests      int64    `json:"unknown_cost_requests"`
	MatchedLocalEstimated    float64  `json:"matched_local_estimated"`
	MatchedDifference        *float64 `json:"matched_difference"`
	TokenizerComparisons     int64    `json:"tokenizer_comparisons"`
	TokenizerMismatches      int64    `json:"tokenizer_mismatches"`
	BalanceNetChange         *float64 `json:"balance_net_change"`
	UnattributedBalanceDelta *float64 `json:"unattributed_balance_delta"`
}

type StatementSync struct {
	LastAttemptMS int64  `json:"last_attempt_ms"`
	LastSuccessMS *int64 `json:"last_success_ms"`
	FilesImported int    `json:"files_imported"`
	LinesImported int    `json:"lines_imported"`
	Failures      int    `json:"failures"`
}

func (l *Ledger) AppendTokenComparison(ctx context.Context, c TokenComparison) error {
	if c.RequestID == "" || strings.TrimSpace(c.Tokenizer) == "" || strings.TrimSpace(c.TokenizerVersion) == "" || c.EstimatedInput < 0 || c.EstimatedOutput < 0 || c.MeasuredAtMS <= 0 {
		return errors.New("invalid token comparison")
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO token_comparisons VALUES (?,?,?,?,?,?)`, c.RequestID, c.Tokenizer, c.TokenizerVersion, c.EstimatedInput, c.EstimatedOutput, c.MeasuredAtMS)
	return err
}

func (l *Ledger) AppendBalanceSnapshot(ctx context.Context, s BalanceSnapshot) error {
	if s.ObservedAtMS <= 0 || strings.TrimSpace(s.Currency) == "" || !validAmount(s.Total) || !validAmount(s.Granted) || !validAmount(s.ToppedUp) {
		return errors.New("invalid balance snapshot")
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO balance_snapshots (observed_at_ms,currency,total,granted,topped_up) VALUES (?,?,?,?,?)`, s.ObservedAtMS, s.Currency, s.Total, s.Granted, s.ToppedUp)
	return err
}

const MaxStatementBytes = 32 << 20
const MaxStatementRows = 100000

// ImportStatementCSV validates and imports one immutable export atomically.
// The persisted content fingerprint makes byte-identical imports no-ops, including
// after renaming or restarting. Identical rows within an export remain distinct.
func (l *Ledger) ImportStatementCSV(ctx context.Context, r io.Reader, importedAtMS int64) (int, error) {
	return l.ImportStatementFile(ctx, "", r, importedAtMS)
}

// ImportStatementFile also remembers a source path. Previously accepted sources
// are immutable: changed contents are rejected to avoid recharging appended rows.
// Separate files must represent independent exports, not overlapping snapshots.
func (l *Ledger) ImportStatementFile(ctx context.Context, source string, r io.Reader, importedAtMS int64) (int, error) {
	if importedAtMS <= 0 {
		return 0, errors.New("invalid import time")
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxStatementBytes+1))
	if err != nil {
		return 0, fmt.Errorf("read statement CSV: %w", err)
	}
	if len(data) > MaxStatementBytes {
		return 0, fmt.Errorf("statement CSV exceeds %d bytes", MaxStatementBytes)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if source != "" {
		var previous string
		err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM statement_import_sources WHERE source=?`, source).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err == nil && previous != digest {
			return 0, errors.New("previously imported statement source changed; statement files must be immutable")
		}
	}
	var duplicate int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM statement_imports WHERE fingerprint=?`, digest).Scan(&duplicate); err != nil {
		return 0, err
	}
	if duplicate > 0 {
		if source != "" {
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO statement_import_sources(source,fingerprint) VALUES (?,?)`, source, digest); err != nil {
				return 0, err
			}
		}
		return 0, tx.Commit()
	}
	reader := csv.NewReader(bytes.NewReader(data))
	header, err := reader.Read()
	if err != nil {
		return 0, fmt.Errorf("read statement CSV header: %w", err)
	}
	head := map[string]int{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		if _, exists := head[h]; exists {
			return 0, fmt.Errorf("statement CSV duplicate column %q", h)
		}
		head[h] = i
	}
	for _, name := range []string{"period_start", "period_end", "currency", "amount"} {
		if _, ok := head[name]; !ok {
			return 0, fmt.Errorf("statement CSV missing %s", name)
		}
	}
	insert, err := tx.PrepareContext(ctx, `INSERT INTO supplier_statement_lines(period_start_ms,period_end_ms,currency,amount,request_id,model,imported_at_ms) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer insert.Close()
	count := 0
	for {
		if err = ctx.Err(); err != nil {
			return 0, err
		}
		row, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, fmt.Errorf("read statement CSV row %d: %w", count+2, readErr)
		}
		count++
		if count > MaxStatementRows {
			return 0, fmt.Errorf("statement CSV exceeds %d rows", MaxStatementRows)
		}
		field := func(name string) string {
			i, ok := head[name]
			if !ok {
				return ""
			}
			return strings.TrimSpace(row[i])
		}
		start, e1 := parseStatementTime(field("period_start"))
		end, e2 := parseStatementTime(field("period_end"))
		amount, e3 := strconv.ParseFloat(field("amount"), 64)
		if e1 != nil || e2 != nil || e3 != nil || start < 0 || start >= end || field("currency") == "" || !validAmount(amount) {
			return 0, fmt.Errorf("invalid statement CSV row %d", count+1)
		}
		if _, err = insert.ExecContext(ctx, start, end, field("currency"), amount, nullIfEmpty(field("request_id")), field("model"), importedAtMS); err != nil {
			return 0, err
		}
	}
	if count == 0 {
		return 0, errors.New("statement CSV requires a header and one row")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO statement_imports(fingerprint,imported_at_ms,row_count) VALUES (?,?,?)`, digest, importedAtMS, count); err != nil {
		return 0, err
	}
	if source != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO statement_import_sources(source,fingerprint) VALUES (?,?)`, source, digest); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (l *Ledger) RecordStatementSync(ctx context.Context, attemptedAtMS int64, filesImported, linesImported, failures int) error {
	if attemptedAtMS <= 0 || filesImported < 0 || linesImported < 0 || failures < 0 {
		return errors.New("invalid statement sync status")
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO statement_sync(id,last_attempt_ms,last_success_ms,files_imported,lines_imported,failures) VALUES(1,?,CASE WHEN ?=0 THEN ? ELSE NULL END,?,?,?) ON CONFLICT(id) DO UPDATE SET last_attempt_ms=excluded.last_attempt_ms,last_success_ms=CASE WHEN excluded.failures=0 THEN excluded.last_attempt_ms ELSE statement_sync.last_success_ms END,files_imported=excluded.files_imported,lines_imported=excluded.lines_imported,failures=excluded.failures`, attemptedAtMS, failures, attemptedAtMS, filesImported, linesImported, failures)
	return err
}

func (l *Ledger) StatementSyncStatus(ctx context.Context) (StatementSync, error) {
	var s StatementSync
	err := l.db.QueryRowContext(ctx, `SELECT last_attempt_ms,last_success_ms,files_imported,lines_imported,failures FROM statement_sync WHERE id=1`).Scan(&s.LastAttemptMS, &s.LastSuccessMS, &s.FilesImported, &s.LinesImported, &s.Failures)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return s, err
}

// Reconcile is retained for callers of the old API; it performs only SELECTs.
// Source writes already updated the materialized state within their transaction.
func (l *Ledger) Reconcile(ctx context.Context, fromMS, toMS int64) ([]Reconciliation, error) {
	return l.ReconciliationStats(ctx, fromMS, toMS)
}

func (l *Ledger) ReconciliationStats(ctx context.Context, fromMS, toMS int64) ([]Reconciliation, error) {
	if err := validatePeriod(fromMS, toMS); err != nil {
		return nil, err
	}
	tx, err := l.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	data := map[string]*Reconciliation{}
	get := func(c string) *Reconciliation {
		if data[c] == nil {
			data[c] = &Reconciliation{Currency: c}
		}
		return data[c]
	}
	where, args := periodWhere("timestamp_ms", fromMS, toMS)
	rows, err := tx.QueryContext(ctx, `SELECT currency,COALESCE(SUM(estimated_cost),0),SUM(estimated_cost IS NULL),SUM(statement_lines=0),SUM(tokenizer_compared),SUM(tokenizer_mismatch) FROM reconciliation_requests`+where+` GROUP BY currency`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		var cost float64
		var unknown, missing, compared, mismatch int64
		if err = rows.Scan(&c, &cost, &unknown, &missing, &compared, &mismatch); err != nil {
			rows.Close()
			return nil, err
		}
		x := get(c)
		x.LocalEstimated = cost
		x.UnknownCostRequests = unknown
		x.MissingStatementRequests = missing
		x.TokenizerComparisons = compared
		x.TokenizerMismatches = mismatch
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	where, args = statementWhere(fromMS, toMS, false)
	rows, err = tx.QueryContext(ctx, `SELECT currency,SUM(amount),COUNT(*),SUM(status='matched'),SUM(status<>'matched') FROM reconciliation_statements`+where+` GROUP BY currency`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		var amount float64
		var total, matched, unmatched int64
		if err = rows.Scan(&c, &amount, &total, &matched, &unmatched); err != nil {
			rows.Close()
			return nil, err
		}
		x := get(c)
		x.SupplierStatement = amount
		x.StatementLines = total
		x.MatchedLines = matched
		x.UnmatchedLines = unmatched
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// A supplier period crossing a requested bound is indivisible. Surface it as
	// partial coverage instead of comparing its full amount with a shorter window.
	if fromMS > 0 || toMS > 0 {
		overlap, overlapArgs := statementWhere(fromMS, toMS, true)
		full, fullArgs := statementWhere(fromMS, toMS, false)
		query := `SELECT currency,COUNT(*) FROM reconciliation_statements` + overlap + ` AND NOT (` + strings.TrimPrefix(full, " WHERE ") + `) GROUP BY currency`
		rows, err = tx.QueryContext(ctx, query, append(overlapArgs, fullArgs...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c string
			var n int64
			if err = rows.Scan(&c, &n); err != nil {
				rows.Close()
				return nil, err
			}
			get(c).PartialStatementLines = n
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	// Count each local estimate once, even if several supplier charges match it.
	where, args = statementWhere(fromMS, toMS, false)
	rows, err = tx.QueryContext(ctx, `SELECT r.currency,COALESCE(SUM(r.estimated_cost),0),SUM(r.estimated_cost IS NULL),SUM(s.amount) FROM reconciliation_requests r JOIN (SELECT request_id,SUM(amount) amount FROM reconciliation_statements`+where+condition(where, `status='matched'`)+` GROUP BY request_id) s ON s.request_id=r.request_id GROUP BY r.currency`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		var local, supplier float64
		var unknown int64
		if err = rows.Scan(&c, &local, &unknown, &supplier); err != nil {
			rows.Close()
			return nil, err
		}
		x := get(c)
		x.MatchedLocalEstimated = local
		if unknown == 0 {
			d := supplier - local
			x.MatchedDifference = &d
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	where, args = statementWhereColumns("from_ms", "to_ms", fromMS, toMS, false)
	rows, err = tx.QueryContext(ctx, `SELECT currency,SUM(net_change),SUM(local_estimated),SUM(unknown_cost_requests) FROM reconciliation_balance_intervals`+where+condition(where, `from_ms IS NOT NULL`)+` GROUP BY currency`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		var delta, estimated float64
		var unknown int64
		if err = rows.Scan(&c, &delta, &estimated, &unknown); err != nil {
			rows.Close()
			return nil, err
		}
		x := get(c)
		x.BalanceNetChange = &delta
		if unknown == 0 {
			d := delta + estimated
			x.UnattributedBalanceDelta = &d
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// Keep a single balance observation visible as a currency with unknown delta.
	where, args = periodWhere("observed_at_ms", fromMS, toMS)
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT currency FROM balance_snapshots`+where, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		if err = rows.Scan(&c); err != nil {
			rows.Close()
			return nil, err
		}
		get(c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]Reconciliation, 0, len(data))
	for _, x := range data {
		x.Coverage = "partial"
		if x.StatementLines == 0 && x.PartialStatementLines == 0 {
			x.Coverage = "no_statement"
		}
		if x.StatementLines > 0 && x.PartialStatementLines == 0 && x.UnmatchedLines == 0 && x.MissingStatementRequests == 0 && x.UnknownCostRequests == 0 {
			x.Coverage = "complete"
			d := x.SupplierStatement - x.LocalEstimated
			x.Difference = &d
		}
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out, tx.Commit()
}

func validatePeriod(from, to int64) error {
	if from < 0 || to < 0 || (to > 0 && from >= to) {
		return errors.New("invalid reconciliation period")
	}
	return nil
}
func condition(where, clause string) string {
	if where == "" {
		return " WHERE " + clause
	}
	return " AND " + clause
}
func periodWhere(column string, from, to int64) (string, []any) {
	parts := []string{}
	args := []any{}
	if from > 0 {
		parts = append(parts, column+">=?")
		args = append(args, from)
	}
	if to > 0 {
		parts = append(parts, column+"<?")
		args = append(args, to)
	}
	if len(parts) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}
func statementWhere(from, to int64, overlap bool) (string, []any) {
	return statementWhereColumns("period_start_ms", "period_end_ms", from, to, overlap)
}
func statementWhereColumns(start, end string, from, to int64, overlap bool) (string, []any) {
	parts := []string{}
	args := []any{}
	if from > 0 {
		if overlap {
			parts = append(parts, end+">?")
		} else {
			parts = append(parts, start+">=?")
		}
		args = append(args, from)
	}
	if to > 0 {
		if overlap {
			parts = append(parts, start+"<?")
		} else {
			parts = append(parts, end+"<=?")
		}
		args = append(args, to)
	}
	if len(parts) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}
func parseStatementTime(v string) (int64, error) {
	if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= 0 {
		return n, nil
	}
	t, e := time.Parse(time.RFC3339, v)
	if e != nil {
		return 0, e
	}
	return t.UnixMilli(), nil
}
func validAmount(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
