// Package observability defines the stable, privacy-bounded runtime event schema.
package observability

import (
	"context"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strings"
)

const SchemaVersion = 1

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	errorCodePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)
	requestIDPattern  = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// JSONLogger creates the JSON Lines logger used for serve runtime events.
func JSONLogger(output io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// LoggerOrDiscard prevents tests and non-serve callers from falling back to the
// process-global text logger. Runtime logs are always emitted through slog.
func LoggerOrDiscard(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// RequestSummary is the versioned event shape shared by terminal requests and
// local rejections. Values are normalized before they are written.
type RequestSummary struct {
	Event             string
	RequestID         string
	Protocol          string
	ProviderProtocol  string
	Endpoint          string
	ProviderRef       string
	Model             string
	Outcome           string
	ErrorCode         string
	HTTPStatus        int
	LatencyMS         int64
	QueueTimeMS       int64
	UpstreamAttempted bool
	RecordPersisted   bool
	TimestampMS       int64
	SessionID         string // Pseudonymous log identity, never a raw client ID.
	InputTokens       *int64
	OutputTokens      *int64
	CacheReadTokens   *int64
	CacheWriteTokens  *int64
	UsageLog          *UsageLog
}

func (e RequestSummary) Log(logger *slog.Logger) {
	logger = LoggerOrDiscard(logger)
	level := slog.LevelInfo
	if e.Outcome != "success" {
		level = slog.LevelWarn
	}
	event := normalizedEnum(e.Event, "request_terminal", "local_rejection")
	protocol := normalizedEnum(e.Protocol, "openai", "anthropic")
	providerProtocol := normalizedEnum(e.ProviderProtocol, "openai", "anthropic")
	endpoint := normalizedEnum(e.Endpoint, "messages", "chat_completions", "models", "unsupported")
	outcome := normalizedEnum(e.Outcome, "success", "error", "canceled", "rejected")
	requestID := ""
	if requestIDPattern.MatchString(e.RequestID) {
		requestID = e.RequestID
	}
	providerRef := normalizedIdentifier(e.ProviderRef)
	model := normalizedIdentifier(e.Model)
	errorCode := ""
	if errorCodePattern.MatchString(e.ErrorCode) {
		errorCode = e.ErrorCode
	}
	if e.HTTPStatus < 100 || e.HTTPStatus > 599 {
		e.HTTPStatus = 0
	}
	if e.LatencyMS < 0 {
		e.LatencyMS = 0
	}
	if e.QueueTimeMS < 0 {
		e.QueueTimeMS = 0
	}
	attrs := []slog.Attr{
		slog.Int("schema_version", SchemaVersion),
		slog.String("event", event),
		slog.String("request_id", requestID),
		slog.String("protocol", protocol),
		slog.String("provider_protocol", providerProtocol),
		slog.String("endpoint", endpoint),
		slog.String("provider_ref", providerRef),
		slog.String("model", model),
		slog.String("outcome", outcome),
		slog.String("error_code", errorCode),
		slog.Int("http_status", e.HTTPStatus),
		slog.Int64("latency_ms", e.LatencyMS),
		slog.Int64("queue_time_ms", e.QueueTimeMS),
		slog.Bool("upstream_attempted", e.UpstreamAttempted),
		slog.Bool("record_persisted", e.RecordPersisted),
	}
	if event == "request_terminal" {
		record := e.usageRecord(requestID, model)
		attrs = append(attrs, slog.String("timestamp", record.Timestamp),
			slog.String("requestId", record.RequestID), slog.String("type", record.Type),
			slog.Any("message", record.Message))
		if record.SessionID != "" {
			attrs = append(attrs, slog.String("sessionId", record.SessionID))
		}
		if e.UsageLog != nil {
			e.UsageLog.append(record, logger)
		}
	}
	logger.LogAttrs(context.Background(), level, "request summary", attrs...)
}

func normalizedEnum(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return "unknown"
}

func normalizedIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if !identifierPattern.MatchString(value) {
		return ""
	}
	return value
}

// HTTPServerErrorLogger adapts net/http's legacy logger while intentionally
// discarding its message, which can contain remote addresses or request data.
func HTTPServerErrorLogger(logger *slog.Logger) *log.Logger {
	return log.New(httpServerErrorWriter{logger: LoggerOrDiscard(logger)}, "", 0)
}

type httpServerErrorWriter struct {
	logger *slog.Logger
}

func (w httpServerErrorWriter) Write(message []byte) (int, error) {
	w.logger.Error("HTTP server internal error",
		slog.Int("schema_version", SchemaVersion),
		slog.String("event", "http_server_error"),
		slog.String("error_class", "net_http"),
	)
	return len(message), nil
}
