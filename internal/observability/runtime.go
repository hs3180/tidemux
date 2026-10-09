// Package observability defines the stable, privacy-bounded runtime event schema.
package observability

import (
	"context"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

const SchemaVersion = 2

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	errorCodePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)
	requestIDPattern  = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// JSONLogger creates the JSON Lines logger used for serve runtime events.
func JSONLogger(output io.Writer) *slog.Logger {
	w := &jsonOutput{output: output}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: canonicalAttribute})
	return slog.New(&jsonHandler{Handler: h, output: w})
}

func canonicalAttribute(groups []string, a slog.Attr) slog.Attr {
	if len(groups) != 0 {
		return a
	}
	if a.Key == slog.TimeKey {
		return slog.String("timestamp", a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	return a
}

type usageLogContextKey struct{}

type usageLogDestination struct {
	log     *UsageLog
	session string
}

// jsonHandler serializes a record once. Its writer sends those exact bytes to
// stderr and, for terminal requests, the session file used by local readers.
type jsonHandler struct {
	slog.Handler
	output *jsonOutput
}

type jsonOutput struct {
	mu          sync.Mutex
	output      io.Writer
	destination usageLogDestination
	warn        bool
}

func (h *jsonHandler) Handle(ctx context.Context, r slog.Record) error {
	destination, _ := ctx.Value(usageLogContextKey{}).(usageLogDestination)
	h.output.mu.Lock()
	h.output.destination = destination
	h.output.warn = false
	err := h.Handler.Handle(ctx, r)
	warn := h.output.warn
	h.output.destination = usageLogDestination{}
	h.output.mu.Unlock()
	if warn {
		slog.New(h).Warn("usage log write failed", slog.Int("schema_version", SchemaVersion),
			slog.String("event", "usage_log_write_failure"))
	}
	return err
}

func (h *jsonHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &jsonHandler{Handler: h.Handler.WithAttrs(attrs), output: h.output}
}

func (h *jsonHandler) WithGroup(name string) slog.Handler {
	return &jsonHandler{Handler: h.Handler.WithGroup(name), output: h.output}
}

func (w *jsonOutput) Write(data []byte) (int, error) {
	n, err := w.output.Write(data)
	if n != len(data) && err == nil {
		err = io.ErrShortWrite
	}
	if destination := w.destination; destination.log != nil {
		w.warn = destination.log.append(destination.session, data)
	}
	return n, err
}

// LoggerOrDiscard prevents tests and non-serve callers from falling back to the
// process-global text logger. Runtime logs are always emitted through slog.
func LoggerOrDiscard(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return JSONLogger(io.Discard)
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
		slog.String("requestId", requestID),
		slog.String("protocol", protocol),
		slog.String("provider_protocol", providerProtocol),
		slog.String("endpoint", endpoint),
		slog.String("provider_ref", providerRef),
		slog.String("outcome", outcome),
		slog.String("error_code", errorCode),
		slog.Int("http_status", e.HTTPStatus),
		slog.Int64("latency_ms", e.LatencyMS),
		slog.Int64("queue_time_ms", e.QueueTimeMS),
		slog.Bool("upstream_attempted", e.UpstreamAttempted),
		slog.Bool("record_persisted", e.RecordPersisted),
	}
	group := ""
	if event == "request_terminal" {
		attrs = append(attrs, slog.String("type", "assistant"), slog.Any("message", e.usageMessage(requestID, model)))
		group = normalizedSessionID(e.SessionID)
		if group != "" {
			attrs = append(attrs, slog.String("sessionId", group))
		}
	} else {
		attrs = append(attrs, slog.Any("message", usageMessage{ID: requestID, Model: model}))
	}
	ctx := context.Background()
	if !logger.Enabled(ctx, level) {
		return
	}
	when := time.Now()
	if e.TimestampMS > 0 {
		when = time.UnixMilli(e.TimestampMS)
	}
	if event == "request_terminal" && e.UsageLog != nil {
		ctx = context.WithValue(ctx, usageLogContextKey{}, usageLogDestination{log: e.UsageLog, session: group})
	}
	record := slog.NewRecord(when, level, "request summary", 0)
	record.AddAttrs(attrs...)
	_ = logger.Handler().Handle(ctx, record)
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
