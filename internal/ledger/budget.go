package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// BudgetPolicy is deliberately currency-scoped. TideMux never converts an
// estimate between currencies in order to make an admission decision.
type BudgetPolicy struct {
	Currency       string
	Timezone       string
	DailyLimit     float64
	MonthlyLimit   float64
	AlertThreshold float64
	Mode           string // alert, soft, hard
	ReserveAmount  float64
}

func (p BudgetPolicy) Validate() error {
	if p == (BudgetPolicy{}) {
		return nil
	}
	if len(p.Currency) != 3 || strings.ToUpper(p.Currency) != p.Currency {
		return errors.New("budget currency must be a three-letter uppercase code")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return errors.New("budget timezone must be an IANA timezone")
	}
	if p.DailyLimit < 0 || p.MonthlyLimit < 0 || (p.DailyLimit == 0 && p.MonthlyLimit == 0) || p.ReserveAmount <= 0 || math.IsNaN(p.ReserveAmount) || math.IsInf(p.ReserveAmount, 0) {
		return errors.New("budget requires a positive limit and conservative reserve_amount")
	}
	if p.AlertThreshold <= 0 || p.AlertThreshold > 1 || (p.Mode != "alert" && p.Mode != "soft" && p.Mode != "hard") {
		return errors.New("invalid budget threshold or mode")
	}
	return nil
}

type BudgetDecision struct {
	Warning bool
}

// ReserveBudget atomically checks the configured daily and monthly periods and
// reserves the configured worst-case amount. The reservation is committed
// before the upstream request is sent, so concurrent hard-limit requests cannot
// overspend the local budget.
func (l *Ledger) ReserveBudget(ctx context.Context, requestID string, p BudgetPolicy, confirmed bool, now time.Time) (BudgetDecision, error) {
	if requestID == "" || p.Validate() != nil {
		return BudgetDecision{}, errors.New("invalid budget reservation")
	}
	loc, _ := time.LoadLocation(p.Timezone)
	dayStart := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc).UnixMilli()
	monthStart := time.Date(now.In(loc).Year(), now.In(loc).Month(), 1, 0, 0, 0, 0, loc).UnixMilli()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return BudgetDecision{}, err
	}
	defer tx.Rollback()
	var daily, monthly float64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_amount),0) FROM budget_reservations WHERE currency=? AND reserved_at_ms>=?`, p.Currency, dayStart).Scan(&daily); err != nil {
		return BudgetDecision{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_amount),0) FROM budget_reservations WHERE currency=? AND reserved_at_ms>=?`, p.Currency, monthStart).Scan(&monthly); err != nil {
		return BudgetDecision{}, err
	}
	dayRatio, monthRatio := ratio(daily+p.ReserveAmount, p.DailyLimit), ratio(monthly+p.ReserveAmount, p.MonthlyLimit)
	over := (p.DailyLimit > 0 && daily+p.ReserveAmount > p.DailyLimit) || (p.MonthlyLimit > 0 && monthly+p.ReserveAmount > p.MonthlyLimit)
	warning := dayRatio >= p.AlertThreshold || monthRatio >= p.AlertThreshold
	if p.Mode == "hard" && over {
		return BudgetDecision{}, errors.New("budget_hard_limit")
	}
	if p.Mode == "soft" && (over || warning) && !confirmed {
		return BudgetDecision{}, errors.New("budget_confirmation_required")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO budget_reservations (request_id,reserved_at_ms,currency,reserved_amount,charged_amount,state) VALUES (?,?,?,?,?,?)`, requestID, now.UnixMilli(), p.Currency, p.ReserveAmount, p.ReserveAmount, "reserved"); err != nil {
		return BudgetDecision{}, err
	}
	if err = tx.Commit(); err != nil {
		return BudgetDecision{}, err
	}
	return BudgetDecision{Warning: warning}, nil
}

// SettleBudget replaces the conservative reservation with the API-usage-based
// local estimate. Missing usage or price deliberately keeps the reserve as an
// unknown charge instead of pretending that the request was free.
func (l *Ledger) SettleBudget(ctx context.Context, reservationID, auditID string) error {
	var cost *float64
	err := l.db.QueryRowContext(ctx, `SELECT estimated_cost FROM request_audit WHERE id=?`, auditID).Scan(&cost)
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		return err
	}
	state := "unknown"
	if cost != nil {
		state = "settled"
	}
	_, err = l.db.ExecContext(ctx, `UPDATE budget_reservations SET charged_amount=COALESCE(?,charged_amount), state=?, audit_id=? WHERE request_id=?`, cost, state, auditID, reservationID)
	if err != nil {
		return fmt.Errorf("settle budget: %w", err)
	}
	return nil
}

func ratio(value, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return value / limit
}
