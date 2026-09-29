package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/observability"
)

// ReconciliationConfig controls automatic intake of immutable supplier exports.
// Request-to-statement matching itself is maintained by the ledger on writes.
type ReconciliationConfig struct {
	StatementDir        string `json:"statement_dir,omitempty"`
	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty"`
}

func (c ReconciliationConfig) Validate() error {
	if c.StatementDir != "" && (!filepath.IsAbs(c.StatementDir) || strings.TrimSpace(c.StatementDir) == "") {
		return errors.New("reconciliation.statement_dir must be an absolute path")
	}
	if c.PollIntervalSeconds < 0 || c.PollIntervalSeconds > 86400 {
		return errors.New("reconciliation.poll_interval_seconds must be 1..86400 or omitted")
	}
	return nil
}

func startStatementSync(c Config, l *ledger.Ledger, logger *slog.Logger) func() {
	directory := c.Reconciliation.StatementDir
	if directory == "" {
		directory = filepath.Join(filepath.Dir(c.LedgerPath), "statements")
	}
	interval := time.Duration(c.Reconciliation.PollIntervalSeconds) * time.Second
	if interval == 0 {
		interval = time.Minute
	}
	return runStatementSyncWithLogger(l, directory, interval, logger)
}

func runStatementSync(l *ledger.Ledger, directory string, interval time.Duration) func() {
	return runStatementSyncWithLogger(l, directory, interval, nil)
}

func runStatementSyncWithLogger(l *ledger.Ledger, directory string, interval time.Duration, logger *slog.Logger) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	reporter := &statementSyncReporter{logger: observability.LoggerOrDiscard(logger)}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			// File/provider availability must never prevent serving model requests.
			// The bounded scan records its result for the read-only billing command.
			scanCtx, scanCancel := context.WithTimeout(ctx, 30*time.Second)
			syncStatementsWithReporter(scanCtx, l, directory, reporter)
			scanCancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

func syncStatements(ctx context.Context, l *ledger.Ledger, directory string) {
	syncStatementsWithReporter(ctx, l, directory, nil)
}

type statementSyncSummary struct {
	files, lines, failures int
	statusWriteFailed      bool
}

type statementSyncReporter struct {
	logger        *slog.Logger
	mu            sync.Mutex
	lastFailureAt time.Time
	suppressed    int
}

func (r *statementSyncReporter) report(summary statementSyncSummary, canceled bool) {
	if r == nil || summary.failures == 0 || canceled {
		return
	}
	now := time.Now()
	r.mu.Lock()
	if !r.lastFailureAt.IsZero() && now.Sub(r.lastFailureAt) < time.Minute {
		r.suppressed++
		r.mu.Unlock()
		return
	}
	suppressed := r.suppressed
	r.suppressed = 0
	r.lastFailureAt = now
	r.mu.Unlock()
	observability.LoggerOrDiscard(r.logger).Warn("statement synchronization encountered failures",
		slog.Int("schema_version", observability.SchemaVersion),
		slog.String("event", "statement_sync_failure"),
		slog.Int("failure_count", summary.failures),
		slog.Int("files_imported", summary.files),
		slog.Int("lines_imported", summary.lines),
		slog.Bool("status_write_failed", summary.statusWriteFailed),
		slog.Int("suppressed_scans", suppressed),
	)
}

func syncStatementsWithReporter(ctx context.Context, l *ledger.Ledger, directory string, reporter *statementSyncReporter) {
	attemptedAt := time.Now().UnixMilli()
	files, lines, failures := 0, 0, 0
	defer func() {
		if ctx.Err() != nil {
			failures++
		}
		// A canceled worker still leaves an honest status before closing SQLite.
		statusCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		recordErr := l.RecordStatementSync(statusCtx, attemptedAt, files, lines, failures)
		summary := statementSyncSummary{files: files, lines: lines, failures: failures}
		if recordErr != nil {
			summary.failures++
			summary.statusWriteFailed = true
		}
		reporter.report(summary, ctx.Err() != nil)
	}()
	if err := os.MkdirAll(directory, 0700); err != nil {
		failures++
		return
	}
	dir, err := os.Open(directory)
	if err != nil {
		failures++
		return
	}
	defer dir.Close()
	for ctx.Err() == nil {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if ctx.Err() != nil {
				return
			}
			if !strings.EqualFold(filepath.Ext(entry.Name()), ".csv") || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || info.Size() > ledger.MaxStatementBytes {
				failures++
				continue
			}
			path, err := filepath.Abs(filepath.Join(directory, entry.Name()))
			if err != nil {
				failures++
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				failures++
				continue
			}
			opened, err := f.Stat()
			if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
				f.Close()
				failures++
				continue
			}
			// Leave time for terminal request auditing (five-second deadline).
			// Large or slow exports roll back and retry on a later scan.
			importCtx, importCancel := context.WithTimeout(ctx, 2*time.Second)
			n, importErr := l.ImportStatementFile(importCtx, path, f, attemptedAt)
			importCancel()
			closeErr := f.Close()
			if importErr != nil || closeErr != nil {
				failures++
				continue
			}
			if n > 0 {
				files++
				lines += n
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				failures++
			}
			return
		}
	}
}
