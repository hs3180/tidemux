package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

var (
	ErrBudgetAuditMissing = errors.New("budget_audit_missing")
)

type BudgetWindow string

const (
	BudgetWindowFiveHour BudgetWindow = "5h"
	BudgetWindowSevenDay BudgetWindow = "7d"
)

// BudgetPolicy is deliberately currency-scoped. TideMux never converts an
// estimate between currencies in order to make an admission decision.
type BudgetPolicy struct {
	Currency       string  `json:"currency"`
	FiveHourLimit  float64 `json:"five_hour_limit"`
	WeeklyLimit    float64 `json:"weekly_limit"`
	AlertThreshold float64 `json:"alert_threshold"`
	Mode           string  `json:"mode"` // alert, soft, hard
}

func (p BudgetPolicy) Validate() error {
	if p == (BudgetPolicy{}) {
		return nil
	}
	if len(p.Currency) != 3 || strings.ToUpper(p.Currency) != p.Currency {
		return errors.New("budget currency must be a three-letter uppercase code")
	}
	if p.FiveHourLimit < 0 || p.WeeklyLimit < 0 || (p.FiveHourLimit == 0 && p.WeeklyLimit == 0) || math.IsNaN(p.FiveHourLimit) || math.IsNaN(p.WeeklyLimit) || math.IsInf(p.FiveHourLimit, 0) || math.IsInf(p.WeeklyLimit, 0) {
		return errors.New("budget requires a positive five_hour_limit or weekly_limit")
	}
	if p.AlertThreshold <= 0 || p.AlertThreshold > 1 || (p.Mode != "alert" && p.Mode != "soft" && p.Mode != "hard") {
		return errors.New("invalid budget threshold or mode")
	}
	return nil
}

type BudgetDecision struct {
	Warning bool
}

// CheckBudget checks rolling five-hour and seven-day windows using settled
// charges only. It persists a zero-value pending attempt so a process restart
// cannot lose an in-flight request; pending attempts do not act as a reserve.
func (l *Ledger) CheckBudget(ctx context.Context, requestID, provider string, p BudgetPolicy, confirmed bool, now time.Time) (BudgetDecision, error) {
	if requestID == "" || strings.TrimSpace(provider) == "" || p.Validate() != nil {
		return BudgetDecision{}, errors.New("invalid budget policy")
	}
	nowMS := now.UnixMilli()
	fiveHourStart := now.Add(-5 * time.Hour).UnixMilli()
	weeklyStart := now.Add(-7 * 24 * time.Hour).UnixMilli()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return BudgetDecision{}, err
	}
	defer tx.Rollback()
	var fiveHourReset, weeklyReset sql.NullInt64
	for _, item := range []struct {
		window string
		target *sql.NullInt64
	}{{"5h", &fiveHourReset}, {"7d", &weeklyReset}} {
		err = tx.QueryRowContext(ctx, `SELECT reset_at_ms FROM budget_resets WHERE provider_scope=? AND window_key=?`, provider, item.window).Scan(item.target)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return BudgetDecision{}, err
		}
	}
	if fiveHourReset.Valid && fiveHourReset.Int64 >= fiveHourStart {
		fiveHourStart = fiveHourReset.Int64 + 1
	}
	if weeklyReset.Valid && weeklyReset.Int64 >= weeklyStart {
		weeklyStart = weeklyReset.Int64 + 1
	}
	var unknown int
	unknownWindows := []string{}
	unknownArgs := []any{provider, p.Currency}
	if p.FiveHourLimit > 0 {
		unknownWindows = append(unknownWindows, `(charged_at_ms>=? AND charged_at_ms<=?)`)
		unknownArgs = append(unknownArgs, fiveHourStart, nowMS)
	}
	if p.WeeklyLimit > 0 {
		unknownWindows = append(unknownWindows, `(charged_at_ms>=? AND charged_at_ms<=?)`)
		unknownArgs = append(unknownArgs, weeklyStart, nowMS)
	}
	if len(unknownWindows) > 0 {
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM budget_charges WHERE (provider_scope=? OR provider_scope='') AND currency=? AND state='unknown' AND (`+strings.Join(unknownWindows, ` OR `)+`)`, unknownArgs...).Scan(&unknown); err != nil {
			return BudgetDecision{}, err
		}
	}
	if unknown > 0 {
		return BudgetDecision{}, errors.New("budget_usage_unknown")
	}
	var fiveHour, weekly float64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_amount),0) FROM budget_charges WHERE (provider_scope=? OR provider_scope='') AND currency=? AND charged_at_ms>=? AND charged_at_ms<=?`, provider, p.Currency, fiveHourStart, nowMS).Scan(&fiveHour); err != nil {
		return BudgetDecision{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_amount),0) FROM budget_charges WHERE (provider_scope=? OR provider_scope='') AND currency=? AND charged_at_ms>=? AND charged_at_ms<=?`, provider, p.Currency, weeklyStart, nowMS).Scan(&weekly); err != nil {
		return BudgetDecision{}, err
	}
	fiveRatio, weekRatio := ratio(fiveHour, p.FiveHourLimit), ratio(weekly, p.WeeklyLimit)
	over := (p.FiveHourLimit > 0 && fiveHour >= p.FiveHourLimit) || (p.WeeklyLimit > 0 && weekly >= p.WeeklyLimit)
	warning := fiveRatio >= p.AlertThreshold || weekRatio >= p.AlertThreshold
	if p.Mode == "hard" && over {
		return BudgetDecision{}, errors.New("budget_hard_limit")
	}
	if p.Mode == "soft" && (over || warning) && !confirmed {
		return BudgetDecision{}, errors.New("budget_confirmation_required")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_charges (request_id,audit_id,charged_at_ms,provider_scope,currency,charged_amount,state) VALUES (?,?,?,?,?,?,?)`, requestID, "", nowMS, provider, p.Currency, 0, "pending"); err != nil {
		return BudgetDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return BudgetDecision{}, err
	}
	return BudgetDecision{Warning: warning}, nil
}

// RecordBudgetCharge records actual usage. Unknown usage is never replaced by
// a guessed amount; it blocks later budget requests in the active window.
func (l *Ledger) RecordBudgetCharge(ctx context.Context, requestID, auditID, provider, currency string, chargedAt time.Time) error {
	var cost *float64
	err := l.db.QueryRowContext(ctx, `SELECT estimated_cost FROM request_audit WHERE id=?`, auditID).Scan(&cost)
	auditMissing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !auditMissing {
		return err
	}
	state, amount := "unknown", 0.0
	if cost != nil {
		state = "settled"
		amount = *cost
	}
	return l.recordBudgetCharge(ctx, requestID, auditID, provider, currency, chargedAt, amount, state, auditMissing)
}

// RecordBudgetChargeWithCost records an already known provider charge when an
// audit append failed after the adapter calculated its cost. It avoids turning
// a missing audit row into unknown usage when the amount is still available.
func (l *Ledger) RecordBudgetChargeWithCost(ctx context.Context, requestID, auditID, provider, currency string, chargedAt time.Time, cost float64) error {
	if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return errors.New("invalid budget charge cost")
	}
	return l.recordBudgetCharge(ctx, requestID, auditID, provider, currency, chargedAt, cost, "settled", false)
}

func (l *Ledger) recordBudgetCharge(ctx context.Context, requestID, auditID, provider, currency string, chargedAt time.Time, amount float64, state string, auditMissing bool) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record budget charge begin: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE budget_charges SET audit_id=?,charged_at_ms=?,provider_scope=?,currency=?,charged_amount=?,state=? WHERE request_id=?`, auditID, chargedAt.UnixMilli(), provider, currency, amount, state, requestID)
	if err != nil {
		return fmt.Errorf("record budget charge: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("record budget charge result: %w", err)
	} else if affected == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO budget_charges (request_id,audit_id,charged_at_ms,provider_scope,currency,charged_amount,state) VALUES (?,?,?,?,?,?,?)`, requestID, auditID, chargedAt.UnixMilli(), provider, currency, amount, state); err != nil {
			return fmt.Errorf("record budget charge insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record budget charge commit: %w", err)
	}
	if auditMissing {
		return ErrBudgetAuditMissing
	}
	return nil
}

// ResetProviderBudget starts a fresh rolling window for one provider.
func (l *Ledger) ResetProviderBudget(ctx context.Context, provider string, window BudgetWindow, resetAt time.Time) error {
	if strings.TrimSpace(provider) == "" || (window != BudgetWindowFiveHour && window != BudgetWindowSevenDay) {
		return errors.New("provider and budget window (5h or 7d) are required")
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO budget_resets (provider_scope,window_key,reset_at_ms) VALUES (?,?,?)
		ON CONFLICT(provider_scope,window_key) DO UPDATE SET reset_at_ms=excluded.reset_at_ms`, provider, string(window), resetAt.UnixMilli())
	return err
}

func ratio(value, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return value / limit
}
