package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hs3180/tidemux/internal/observability"
	"github.com/hs3180/tidemux/internal/usage"
)

func startUsageExport(c Config, signer *usage.Signer, logger *slog.Logger) func() {
	var config usage.Config
	if c.UsageLog != nil {
		config = *c.UsageLog
	}
	config = config.Effective(c.LedgerPath)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Duration(config.PollSeconds) * time.Second)
		defer ticker.Stop()
		var lastWarning time.Time
		var status usage.Status
		for ctx.Err() == nil {
			exporter, err := usage.New(config, c.LedgerPath, signer)
			if err == nil {
				scanCtx, scanCancel := context.WithTimeout(ctx, 2*time.Second)
				var result usage.Status
				result, err = exporter.Sync(scanCtx, false)
				scanCancel()
				exporter.Close()
				if err == nil {
					status = result
				}
			}
			status.ErrorCode = ""
			if err != nil {
				// Classifications only: errors may contain paths or private data.
				status.ErrorCode = usage.FailureCode(err)
				if signer == nil {
					status.ErrorCode = "usage_identity_unavailable"
				}
				if ctx.Err() == nil && (lastWarning.IsZero() || time.Since(lastWarning) >= time.Minute) {
					observability.LoggerOrDiscard(logger).Warn("usage export failed", slog.Int("schema_version", observability.SchemaVersion), slog.String("event", "usage_export_failure"), slog.String("failure_code", status.ErrorCode))
					lastWarning = time.Now()
				}
			}
			_ = usage.WriteStatus(c.LedgerPath, status)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}
