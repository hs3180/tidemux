package gateway

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
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

func startStatementSync(c Config, l *ledger.Ledger) func() {
	directory := c.Reconciliation.StatementDir
	if directory == "" {
		directory = filepath.Join(filepath.Dir(c.LedgerPath), "statements")
	}
	interval := time.Duration(c.Reconciliation.PollIntervalSeconds) * time.Second
	if interval == 0 {
		interval = time.Minute
	}
	return runStatementSync(l, directory, interval)
}

func runStatementSync(l *ledger.Ledger, directory string, interval time.Duration) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
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
			syncStatements(scanCtx, l, directory)
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
	attemptedAt := time.Now().UnixMilli()
	files, lines, failures := 0, 0, 0
	defer func() {
		if ctx.Err() != nil {
			failures++
		}
		// A canceled worker still leaves an honest status before closing SQLite.
		statusCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.RecordStatementSync(statusCtx, attemptedAt, files, lines, failures)
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
