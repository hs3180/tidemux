// Package ledger owns TideMux's local, append-only runtime cost ledger.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Request is the legacy request format, retained for database compatibility.
// Its currency-specific fields describe historical data, not the active ledger.
// New gateway requests use Audit with an explicit currency and price snapshot.
type Request struct {
	TimestampMS         int64
	Kind                string
	Model               string
	Status              string
	PromptTokens        int64
	CompletionTokens    int64
	TotalTokens         int64
	CacheHitTokens      *int64
	CacheMissTokens     *int64
	PriceInPerMTok      float64
	PriceOutPerMTok     float64
	PriceHitPerMTok     float64
	PriceMissPerMTok    float64
	EstimatedCostCNY    float64
	Shaping             *string
	ErrorCode           *string
	ErrorReason         *string
	FirstTokenLatencyMS *int64
	TotalLatencyMS      *int64
}

// Event records an explainable shaping or rate-limit decision related to a
// request. RequestID is optional because a rejected request may have no row.
type Event struct {
	TimestampMS int64
	Type        string
	RequestID   *int64
	Reason      string
	Detail      *string
	RetryAfterS *float64
}

// Ledger is a local SQLite-backed append-only ledger.
type Ledger struct{ db *sql.DB }

// Open creates the SQLite schema when necessary and enables settings suitable
// for concurrent local reads and serialized request writes.
func Open(path string) (*Ledger, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	l := &Ledger{db: db}
	if err := l.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := l.initAudit(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := l.initDiagnostics(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := l.initReconciliation(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := l.initBudget(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := l.initReports(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Ledger) initBudget(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS budget_reservations (
 request_id TEXT PRIMARY KEY, audit_id TEXT, reserved_at_ms INTEGER NOT NULL,
 currency TEXT NOT NULL, reserved_amount REAL NOT NULL, charged_amount REAL NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('reserved','settled','unknown')));
 CREATE INDEX IF NOT EXISTS budget_reservation_period ON budget_reservations(currency,reserved_at_ms);`)
	return err
}

// Close releases the local database handle.
func (l *Ledger) Close() error { return l.db.Close() }

// QueryRow exposes read-only SQL access for derived views and diagnostics.
// Callers must not mutate ledger tables. Current writes use AppendAudit or
// AppendDiagnostic; AppendRequest and AppendEvent are legacy compatibility APIs.
func (l *Ledger) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return l.db.QueryRowContext(ctx, query, args...)
}

// AppendRequest inserts one immutable request row and returns its ledger ID.
func (l *Ledger) AppendRequest(ctx context.Context, r Request) (int64, error) {
	if err := validateRequest(r); err != nil {
		return 0, err
	}
	result, err := l.db.ExecContext(ctx, `INSERT INTO ledger_requests (
		req_ts, req_kind, model, status, prompt_tokens, completion_tokens,
		total_tokens, cache_hit_tokens, cache_miss_tokens, price_in_per_Mtok,
		price_out_per_Mtok, price_hit_per_Mtok, price_miss_per_Mtok,
		est_cost_cny, shaping, err_code, err_reason, latency_first_token_ms,
		latency_total_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TimestampMS, requestKind(r.Kind), r.Model, r.Status, r.PromptTokens,
		r.CompletionTokens, r.TotalTokens, nullableInt(r.CacheHitTokens),
		nullableInt(r.CacheMissTokens), r.PriceInPerMTok, r.PriceOutPerMTok,
		r.PriceHitPerMTok, r.PriceMissPerMTok, r.EstimatedCostCNY,
		nullableString(r.Shaping), nullableString(r.ErrorCode),
		nullableString(r.ErrorReason), nullableInt(r.FirstTokenLatencyMS),
		nullableInt(r.TotalLatencyMS),
	)
	if err != nil {
		return 0, fmt.Errorf("append ledger request: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read ledger request ID: %w", err)
	}
	return id, nil
}

// AppendEvent inserts an immutable, user-explainable control-plane event.
func (l *Ledger) AppendEvent(ctx context.Context, e Event) (int64, error) {
	if e.TimestampMS <= 0 || strings.TrimSpace(e.Type) == "" || strings.TrimSpace(e.Reason) == "" {
		return 0, errors.New("ledger event requires timestamp, type, and reason")
	}
	result, err := l.db.ExecContext(ctx, `INSERT INTO ledger_events
		(ts, type, req_id, reason, detail, retry_after_s) VALUES (?, ?, ?, ?, ?, ?)`,
		e.TimestampMS, e.Type, nullableInt(e.RequestID), e.Reason,
		nullableString(e.Detail), nullableFloat(e.RetryAfterS))
	if err != nil {
		return 0, fmt.Errorf("append ledger event: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read ledger event ID: %w", err)
	}
	return id, nil
}

func (l *Ledger) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS ledger_requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			req_ts INTEGER NOT NULL,
			req_kind TEXT NOT NULL CHECK (req_kind = 'chat'),
			model TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('ok', 'rate_limited', 'queued_ok', 'throttled_ok', 'budget_capped', 'error')),
			prompt_tokens INTEGER NOT NULL CHECK (prompt_tokens >= 0),
			completion_tokens INTEGER NOT NULL CHECK (completion_tokens >= 0),
			total_tokens INTEGER NOT NULL CHECK (total_tokens >= 0),
			cache_hit_tokens INTEGER,
			cache_miss_tokens INTEGER,
			price_in_per_Mtok REAL NOT NULL CHECK (price_in_per_Mtok >= 0),
			price_out_per_Mtok REAL NOT NULL CHECK (price_out_per_Mtok >= 0),
			price_hit_per_Mtok REAL NOT NULL CHECK (price_hit_per_Mtok >= 0),
			price_miss_per_Mtok REAL NOT NULL CHECK (price_miss_per_Mtok >= 0),
			est_cost_cny REAL NOT NULL CHECK (est_cost_cny >= 0),
			shaping TEXT,
			err_code TEXT,
			err_reason TEXT,
			latency_first_token_ms INTEGER,
			latency_total_ms INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS ledger_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			type TEXT NOT NULL CHECK (type IN ('queue_wait', 'rate_limit_429', 'peak_throttle', 'budget_capped', 'backoff_retry')),
			req_id INTEGER REFERENCES ledger_requests(id),
			reason TEXT NOT NULL,
			detail TEXT,
			retry_after_s REAL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ledger_req_ts ON ledger_requests(req_ts)`,
		`CREATE INDEX IF NOT EXISTS idx_ledger_req_status ON ledger_requests(status)`,
		`CREATE INDEX IF NOT EXISTS idx_ledger_events_req ON ledger_events(req_id)`,
	}
	for _, statement := range statements {
		if _, err := l.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate ledger: %w", err)
		}
	}
	return nil
}

func validateRequest(r Request) error {
	if r.TimestampMS <= 0 || strings.TrimSpace(r.Model) == "" {
		return errors.New("ledger request requires timestamp and model")
	}
	if requestKind(r.Kind) != "chat" {
		return fmt.Errorf("unsupported request kind %q", r.Kind)
	}
	validStatus := map[string]bool{"ok": true, "rate_limited": true, "queued_ok": true, "throttled_ok": true, "budget_capped": true, "error": true}
	if !validStatus[r.Status] {
		return fmt.Errorf("unsupported ledger status %q", r.Status)
	}
	if r.PromptTokens < 0 || r.CompletionTokens < 0 || r.TotalTokens != r.PromptTokens+r.CompletionTokens {
		return errors.New("ledger request token totals are invalid")
	}
	if r.PriceInPerMTok < 0 || r.PriceOutPerMTok < 0 || r.PriceHitPerMTok < 0 || r.PriceMissPerMTok < 0 || r.EstimatedCostCNY < 0 {
		return errors.New("ledger request prices and cost must be non-negative")
	}
	return nil
}

func requestKind(kind string) string {
	if kind == "" {
		return "chat"
	}
	return kind
}
func nullableInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
func nullableString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
