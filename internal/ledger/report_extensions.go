package ledger

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
)

// Optional readers never create another feature's tables. Missing sources stay
// unknown; query failures in an installed source are returned to the caller.
func (l *Ledger) reportTableExists(ctx context.Context, name string) (bool, error) {
	var count int
	err := l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
	return count > 0, err
}

func (l *Ledger) enrichDailyReport(ctx context.Context, r *DailyReport, start, end time.Time, budget ReportBudget) error {
	if math.IsNaN(budget.DailyLimit) || math.IsInf(budget.DailyLimit, 0) || budget.DailyLimit < 0 || (budget.DailyLimit > 0 && (len(budget.Currency) != 3 || strings.ToUpper(budget.Currency) != budget.Currency)) {
		return errors.New("invalid reporting budget")
	}
	for _, source := range []struct {
		name   string
		query  string
		target **int64
	}{
		{"token_comparisons", `SELECT COUNT(*) FROM token_comparisons c JOIN request_audit a ON a.id=c.request_id WHERE a.timestamp_ms>=? AND a.timestamp_ms<? AND (c.estimated_input<>COALESCE(a.input_tokens,-1) OR c.estimated_output<>COALESCE(a.output_tokens,-1))`, &r.TokenizerMismatches},
		{"supplier_statement_lines", `SELECT COUNT(*) FROM supplier_statement_lines s WHERE s.period_start_ms>=? AND s.period_start_ms<? AND (s.request_id IS NULL OR NOT EXISTS(SELECT 1 FROM request_audit a WHERE a.id=s.request_id))`, &r.UnmatchedStatements},
	} {
		exists, err := l.reportTableExists(ctx, source.name)
		if err != nil {
			return err
		}
		if exists {
			var count int64
			if err := l.db.QueryRowContext(ctx, source.query, start.UnixMilli(), end.UnixMilli()).Scan(&count); err != nil {
				return err
			}
			*source.target = &count
		}
	}
	exists, err := l.reportTableExists(ctx, "balance_snapshots")
	if err != nil {
		return err
	}
	if exists && r.Currency != "" {
		var count int
		if err := l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM balance_snapshots WHERE currency=? AND observed_at_ms>=? AND observed_at_ms<?`, r.Currency, start.UnixMilli(), end.UnixMilli()).Scan(&count); err != nil {
			return err
		}
		if count >= 2 {
			var first, last float64
			for _, q := range []struct {
				order  string
				target *float64
			}{{"ASC", &first}, {"DESC", &last}} {
				if err := l.db.QueryRowContext(ctx, `SELECT total FROM balance_snapshots WHERE currency=? AND observed_at_ms>=? AND observed_at_ms<? ORDER BY observed_at_ms `+q.order+` LIMIT 1`, r.Currency, start.UnixMilli(), end.UnixMilli()).Scan(q.target); err != nil {
					return err
				}
			}
			delta := last - first
			r.BalanceNetChange = &delta
		}
	}
	exists, err = l.reportTableExists(ctx, "budget_reservations")
	if err != nil {
		return err
	}
	if exists && budget.DailyLimit > 0 && budget.Currency == r.Currency {
		var spent float64
		if err := l.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_amount),0) FROM budget_reservations WHERE currency=? AND reserved_at_ms>=? AND reserved_at_ms<?`, budget.Currency, start.UnixMilli(), end.UnixMilli()).Scan(&spent); err != nil {
			return err
		}
		remaining := budget.DailyLimit - spent
		r.BudgetRemaining = &remaining
	}
	return nil
}
