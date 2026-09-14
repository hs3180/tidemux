package ledger

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TokenComparison preserves an optional local tokenizer measurement alongside
// the provider's response usage already retained in request_audit.  It stores
// counts only: prompts and responses never enter the reconciliation database.
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

// StatementLine is a normalized supplier export row. RequestID is optional;
// without it a line is deliberately left unmatched rather than guessed.
type StatementLine struct {
	PeriodStartMS int64
	PeriodEndMS   int64
	Currency      string
	Amount        float64
	RequestID     string
	Model         string
}

type Reconciliation struct {
	Currency                 string   `json:"currency"`
	LocalEstimated           float64  `json:"local_estimated"`
	SupplierStatement        float64  `json:"supplier_statement"`
	Difference               float64  `json:"difference"`
	MatchedLines             int64    `json:"matched_lines"`
	UnmatchedLines           int64    `json:"unmatched_lines"`
	UnknownCostRequests      int64    `json:"unknown_cost_requests"`
	TokenizerComparisons     int64    `json:"tokenizer_comparisons"`
	TokenizerMismatches      int64    `json:"tokenizer_mismatches"`
	BalanceNetChange         *float64 `json:"balance_net_change"`
	UnattributedBalanceDelta *float64 `json:"unattributed_balance_delta"`
}

func (l *Ledger) initReconciliation(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS token_comparisons (
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
 CREATE INDEX IF NOT EXISTS statement_request ON supplier_statement_lines(request_id);`)
	return err
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

func (l *Ledger) ImportStatementCSV(ctx context.Context, r io.Reader, importedAtMS int64) (int, error) {
	if importedAtMS <= 0 {
		return 0, errors.New("invalid import time")
	}
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return 0, fmt.Errorf("read statement CSV: %w", err)
	}
	if len(rows) < 2 {
		return 0, errors.New("statement CSV requires a header and one row")
	}
	head := map[string]int{}
	for i, h := range rows[0] {
		head[strings.TrimSpace(h)] = i
	}
	for _, name := range []string{"period_start", "period_end", "currency", "amount"} {
		if _, ok := head[name]; !ok {
			return 0, fmt.Errorf("statement CSV missing %s", name)
		}
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for n, row := range rows[1:] {
		field := func(name string) string {
			i, ok := head[name]
			if !ok || i >= len(row) {
				return ""
			}
			return strings.TrimSpace(row[i])
		}
		start, e1 := parseStatementTime(field("period_start"))
		end, e2 := parseStatementTime(field("period_end"))
		amount, e3 := strconv.ParseFloat(field("amount"), 64)
		if e1 != nil || e2 != nil || e3 != nil || start >= end || field("currency") == "" || !validAmount(amount) {
			return 0, fmt.Errorf("invalid statement CSV row %d", n+2)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO supplier_statement_lines (period_start_ms,period_end_ms,currency,amount,request_id,model,imported_at_ms) VALUES (?,?,?,?,?,?,?)`, start, end, field("currency"), amount, nullIfEmpty(field("request_id")), field("model"), importedAtMS); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(rows) - 1, nil
}

func (l *Ledger) Reconcile(ctx context.Context, fromMS, toMS int64) ([]Reconciliation, error) {
	if fromMS < 0 || toMS < 0 || (toMS > 0 && fromMS >= toMS) {
		return nil, errors.New("invalid reconciliation period")
	}
	where, args := periodWhere("timestamp_ms", fromMS, toMS)
	data := map[string]*Reconciliation{}
	get := func(c string) *Reconciliation {
		if data[c] == nil {
			data[c] = &Reconciliation{Currency: c}
		}
		return data[c]
	}
	rows, err := l.db.QueryContext(ctx, `SELECT COALESCE(currency,''), COALESCE(SUM(estimated_cost),0), SUM(CASE WHEN estimated_cost IS NULL THEN 1 ELSE 0 END) FROM request_audit`+where+` GROUP BY currency`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		var cost float64
		var unknown int64
		if err := rows.Scan(&c, &cost, &unknown); err != nil {
			rows.Close()
			return nil, err
		}
		x := get(c)
		x.LocalEstimated = cost
		x.UnknownCostRequests = unknown
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	where, args = periodWhere("period_start_ms", fromMS, toMS)
	rows, err = l.db.QueryContext(ctx, `SELECT currency, COALESCE(SUM(amount),0), SUM(CASE WHEN request_id IS NULL OR request_id='' OR NOT EXISTS(SELECT 1 FROM request_audit r WHERE r.id=s.request_id) THEN 1 ELSE 0 END), SUM(CASE WHEN request_id IS NOT NULL AND request_id<>'' AND EXISTS(SELECT 1 FROM request_audit r WHERE r.id=s.request_id) THEN 1 ELSE 0 END) FROM supplier_statement_lines s`+where+` GROUP BY currency`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		var amount float64
		var unmatched, matched int64
		if err := rows.Scan(&c, &amount, &unmatched, &matched); err != nil {
			rows.Close()
			return nil, err
		}
		x := get(c)
		x.SupplierStatement = amount
		x.UnmatchedLines = unmatched
		x.MatchedLines = matched
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	where, args = periodWhere("a.timestamp_ms", fromMS, toMS)
	rows, err = l.db.QueryContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN c.estimated_input<>COALESCE(a.input_tokens,-1) OR c.estimated_output<>COALESCE(a.output_tokens,-1) THEN 1 ELSE 0 END),0) FROM token_comparisons c JOIN request_audit a ON a.id=c.request_id`+where, args...)
	if err != nil {
		return nil, err
	}
	var compares, mismatches int64
	if rows.Next() {
		if err = rows.Scan(&compares, &mismatches); err != nil {
			rows.Close()
			return nil, err
		}
	}
	rows.Close()
	for _, x := range data {
		x.TokenizerComparisons = compares
		x.TokenizerMismatches = mismatches
	}
	for currency, x := range data {
		x.Difference = x.SupplierStatement - x.LocalEstimated
		var first, last float64
		err := l.db.QueryRowContext(ctx, `SELECT total FROM balance_snapshots WHERE currency=? ORDER BY observed_at_ms ASC LIMIT 1`, currency).Scan(&first)
		if err == nil {
			if l.db.QueryRowContext(ctx, `SELECT total FROM balance_snapshots WHERE currency=? ORDER BY observed_at_ms DESC LIMIT 1`, currency).Scan(&last) == nil {
				delta := last - first
				x.BalanceNetChange = &delta
				unattributed := delta + x.LocalEstimated
				x.UnattributedBalanceDelta = &unattributed
			}
		}
	}
	out := make([]Reconciliation, 0, len(data))
	for _, x := range data {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out, nil
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
func parseStatementTime(v string) (int64, error) {
	if n, e := strconv.ParseInt(v, 10, 64); e == nil && n > 0 {
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
