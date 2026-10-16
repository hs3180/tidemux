package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hs3180/tidemux/internal/diagnostics"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
	"github.com/hs3180/tidemux/internal/observability"
)

type Client struct {
	Limits                                          Limits
	Protocol, BaseURL, APIKey, APIVersion, Upstream string
	ProviderRef                                     string
	BackendID                                       string
	ForwardErrorMessages                            *bool    // nil enables bounded, redacted provider reasons
	RedactValues                                    []string // admitted gateway credentials; never serialized
	Logger                                          *slog.Logger
	UsageLog                                        *observability.UsageLog
	MaxOutputTokens                                 int64
	ModelOutputLimits                               map[string]int64 // immutable per-model catalog snapshot
	Prices                                          map[string]Price
	ErrorCodeMappings                               []ProviderErrorMapping
	PromptCache                                     *PromptCache
	// CacheNamespace separates local prefix history across provider connections.
	// It is internal only; SessionID remains unchanged at the upstream boundary.
	CacheNamespace string
	HTTP           *http.Client
	Ledger         *ledger.Ledger
	Gate           *limiter.ConcurrencyGate
}

// An operator's configured default retains its meaning. Discovery can reduce
// the generic conversion default to fit a model, but never increases it.
func (c *Client) OutputTokenDefault(model string) int64 {
	if c.MaxOutputTokens > 0 {
		return c.MaxOutputTokens
	}
	if limit := c.ModelOutputLimits[model]; limit > 0 && limit < defaultAnthropicMaxTokens {
		return limit
	}
	return 0
}

type CallError struct {
	Status int
	Code   string
	// UpstreamNotAttempted is true only when the adapter can prove that no
	// request was dispatched to the provider. The gateway may then release a
	// budget reservation instead of treating the missing usage as unknown.
	UpstreamNotAttempted bool
	// FailoverSafe marks provider failures that should affect route health for
	// future client requests. It never triggers another upstream attempt here.
	FailoverSafe bool
	// BudgetCost preserves a known charge when the audit row itself cannot be
	// written. It is internal accounting data and is never sent to clients.
	BudgetCost        *float64
	Param             string
	ProviderCode      string
	Category          ProviderErrorCategory
	UpstreamStatus    int
	Cooldown          time.Duration
	CooldownUntil     time.Time
	RetryAfterSecs    int64
	RetryProvided     bool
	RetryAfterValue   string
	Failure           *diagnostics.Failure
	AllKeysCooling    bool
	RateLimitFailures uint16
	// Internal only: a route can yield after queueing without an HTTP attempt.
	RequestStartedAt time.Time
	QueueMS          int64
}

// ErrServerShuttingDown identifies a gateway-initiated cancellation without
// confusing it with client cancellation or an upstream failure.
var ErrServerShuttingDown = errors.New("server shutting down")

func (e *CallError) Error() string { return e.Code }

// KeyCandidateCallbacks selects one ready key before dispatch and records the
// outcome. A failed upstream attempt is returned to the downstream client;
// this adapter does not replay it with another key.
type KeyCandidateCallbacks struct {
	KeyLabel         func(index int) string
	YieldUnavailable bool                                    // No audit/ID when yielding an undispatched route to its owner.
	Attempt          func(index int) func(HTTPAttemptResult) // one actual HTTP initiation/result
	Completed        func(index int, err error)              // upstream result, before releasing the dispatch gate
	Ready            func(index int) (bool, time.Duration)
	Failed           func(index int, callErr *CallError)
	// PrepareFailure runs after health completion/cancellation classification and
	// before persistence. The owner can freeze content-free response guidance.
	PrepareFailure func(callErr *CallError)
}

type observedReader struct {
	io.Reader
	observed *bytes.Buffer
	timedOut *bool
}

func (r observedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && r.timedOut != nil {
		var network interface{ Timeout() bool }
		if errors.As(err, &network) && network.Timeout() {
			*r.timedOut = true
		}
	}
	if n > 0 {
		r.observed.Write(p[:n])
	}
	return n, err
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
func (c *Client) Call(ctx context.Context, body []byte, model string) (response []byte, id string, err error) {
	return c.call(c.Protocol, ctx, body, model, nil, CallOptions{})
}
func (c *Client) CallStream(ctx context.Context, body []byte, model string, sink StreamSink) ([]byte, string, error) {
	return c.call(c.Protocol, ctx, body, model, sink, CallOptions{})
}
func (c *Client) CallWithOptions(ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions) ([]byte, string, error) {
	return c.call(c.Protocol, ctx, body, model, sink, options)
}
func (c *Client) CallFrom(clientProtocol string, ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions) ([]byte, string, error) {
	return c.call(clientProtocol, ctx, body, model, sink, options)
}
func (c *Client) call(clientProtocol string, ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions) (response []byte, id string, err error) {
	return c.callWithKeyCandidates(clientProtocol, ctx, body, model, sink, options, []string{c.APIKey}, KeyCandidateCallbacks{})
}

// CallFromKeyCandidates chooses one ready same-provider credential, performs a
// single upstream attempt, and writes one terminal audit record.
func (c *Client) CallFromKeyCandidates(clientProtocol string, ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions, keys []string, callbacks KeyCandidateCallbacks) ([]byte, string, error) {
	return c.callWithKeyCandidates(clientProtocol, ctx, body, model, sink, options, keys, callbacks)
}

func (c *Client) callWithKeyCandidates(clientProtocol string, ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions, keys []string, callbacks KeyCandidateCallbacks) (response []byte, id string, err error) {
	if err := options.Validate(clientProtocol); err != nil {
		return nil, "", &CallError{Status: 400, Code: err.Error(), Param: ValidationParameter(err), UpstreamNotAttempted: true}
	}
	if c.Ledger.AuditUnavailable() {
		return nil, "", &CallError{Status: 503, Code: "audit_unavailable", UpstreamNotAttempted: true}
	}
	if len(keys) == 0 {
		keys = []string{c.APIKey}
	}
	started := time.Now()
	if !options.RequestStartedAt.IsZero() {
		started = options.RequestStartedAt
	}
	nonce := make([]byte, 16)
	if _, e := rand.Read(nonce); e != nil {
		return nil, "", &CallError{Status: 500, Code: "request_id_failed", UpstreamNotAttempted: true}
	}
	id = hex.EncodeToString(nonce)
	a := ledger.Audit{ID: id, TimestampMS: started.UnixMilli(), Protocol: c.Protocol, Upstream: c.Upstream, ProviderRef: c.ProviderRef, Model: model, RoutingDecision: options.RoutingDecision.Normalize(), Status: "error", Events: []string{}}
	providerBody, preparedModel, requestWarnings, e := PrepareRequestWithWarnings(clientProtocol, c.Protocol, body, model, c.OutputTokenDefault(model))
	if e != nil {
		return nil, id, &CallError{Status: 400, Code: e.Error(), Param: ValidationParameter(e), UpstreamNotAttempted: true}
	}
	logConversionWarnings(c.Logger, clientProtocol, c.Protocol, requestWarnings)
	if preparedModel != "" {
		model = preparedModel
		a.Model = model
	}
	price, priced := c.Prices[model]
	if !priced {
		price, priced = BuiltInPrice(c.BaseURL, model, started)
	}
	var observed bytes.Buffer
	attempted := false
	selectedCandidate := -1
	delivered := false
	auditPersisted := false
	yielded := false // an undispatched rejection belongs to the gateway's local diagnostic
	localEstimate := func(response []byte) {
		if !priced || a.EstimatedCost != nil {
			return
		}
		cacheSession := c.cacheSession(clientProtocol, options.SessionID)
		if options.RequestScopedSession {
			cacheSession = ""
		}
		cost, usage, source := c.PromptCache.LocalEstimate(c.Protocol, model, cacheSession, body, response, price)
		if cost != nil {
			a.InputTokens, a.OutputTokens, a.CacheReadTokens = usage.Input, usage.Output, usage.CacheRead
			a.Currency, a.PriceSnapshot, a.EstimatedCost, a.CostSource = price.Currency, mustJSON(price), cost, source
		}
	}
	defer func() {
		if yielded {
			return
		}
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			response = nil
			err = &CallError{Status: http.StatusInternalServerError, Code: "internal_error"}
			observability.LoggerOrDiscard(c.Logger).Error("request handler panic recovered",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "request_panic"),
				slog.String("requestId", id),
			)
		}
		if a.EstimatedCost == nil && attempted {
			localEstimate(observed.Bytes())
		}
		a.LatencyMS = time.Since(started).Milliseconds()
		if err != nil {
			a.ErrorCode = "upstream_error"
			var ce *CallError
			if errors.As(err, &ce) {
				a.ErrorCode = ce.Code
			}
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				var previous *CallError
				var failure *diagnostics.Failure
				if errors.As(err, &previous) {
					failure = previous.Failure.Normalize()
				}
				if failure != nil {
					failure.Source, failure.Category, failure.Retryability = "canceled", "canceled", "terminal"
				}
				a.Status = "canceled"
				a.ErrorCode = "request_canceled"
				err = &CallError{Status: 408, Code: "request_canceled", Failure: failure}
				if errors.Is(context.Cause(ctx), ErrServerShuttingDown) {
					a.ErrorCode = "server_shutting_down"
					err = &CallError{Status: http.StatusServiceUnavailable, Code: a.ErrorCode, Failure: failure}
				}
			}
			var terminal *CallError
			if errors.As(err, &terminal) {
				if callbacks.PrepareFailure != nil {
					callbacks.PrepareFailure(terminal)
				}
				a.Failure = terminal.Failure.Normalize()
			}
		}
		if e := c.Ledger.AppendAudit(a); e != nil {
			observability.LoggerOrDiscard(c.Logger).Error("request audit append failed",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "request_audit_write_failure"),
				slog.String("requestId", id),
				slog.String("failure_code", "audit_write_failed"),
			)
			response = nil
			callErr := &CallError{Status: 500, Code: "audit_failed_do_not_retry_blindly", Failure: a.Failure.Normalize()}
			if a.EstimatedCost != nil {
				cost := *a.EstimatedCost
				callErr.BudgetCost = &cost
			}
			err = callErr
			// The provider may have completed, but the gateway operation failed.
			// Replace any earlier retry decision before emitting the terminal log.
			a.Status, a.ErrorCode = "error", callErr.Code
			if callbacks.PrepareFailure != nil {
				callbacks.PrepareFailure(callErr)
			}
			a.Failure = callErr.Failure.Normalize()
		} else {
			auditPersisted = true
		}
		if err != nil {
			var callErr *CallError
			if errors.As(err, &callErr) {
				if !attempted {
					callErr.UpstreamNotAttempted = true
				}
			}
		}
		status := terminalHTTPStatus(clientProtocol, delivered, err)
		errorCode := a.ErrorCode
		if err != nil {
			var callErr *CallError
			if errors.As(err, &callErr) {
				errorCode = callErr.Code
			} else {
				errorCode = "internal_error"
			}
		}
		outcome := a.Status
		if outcome == "ok" {
			outcome = "success"
		} else if outcome != "error" && outcome != "canceled" {
			outcome = "error"
		}
		observability.RequestSummary{
			Event: "request_terminal", RequestID: id,
			Protocol: clientProtocol, ProviderProtocol: c.Protocol,
			Endpoint: requestEndpoint(clientProtocol), ProviderRef: c.ProviderRef,
			KeyLabel: a.KeyLabel, BackendID: a.BackendID,
			Model: a.Model, Outcome: outcome, ErrorCode: errorCode,
			HTTPStatus: status, LatencyMS: a.LatencyMS, QueueTimeMS: a.QueueMS,
			UpstreamAttempted: attempted, RecordPersisted: auditPersisted,
			TimestampMS: a.TimestampMS,
			SessionID:   c.UsageLog.SessionID(clientProtocol, options.SessionID, options.RequestScopedSession),
			InputTokens: a.InputTokens, OutputTokens: a.OutputTokens,
			CacheReadTokens: a.CacheReadTokens, CacheWriteTokens: a.CacheWriteTokens,
			UsageLog:        c.UsageLog,
			Failure:         a.Failure,
			RoutingDecision: a.RoutingDecision,
		}.Log(c.Logger)
	}()
	admission, e := c.Gate.Acquire(ctx)
	a.QueueMS = options.PriorQueueMS + admission.QueueWait.Milliseconds()
	if admission.Queued || options.PriorQueueMS > 0 {
		a.Events = append(a.Events, "queue_wait")
	}
	if e != nil {
		return nil, id, &CallError{Status: 408, Code: "request_canceled"}
	}
	defer c.Gate.Release()
	defer func() {
		if attempted && callbacks.Completed != nil {
			callbacks.Completed(selectedCandidate, err)
		}
	}()
	if e = ctx.Err(); e != nil {
		return nil, id, &CallError{Status: 408, Code: "request_canceled"}
	}
	limits := c.Limits.Effective()
	requestCtx, cancelRequest := context.WithTimeout(ctx, c.Limits.UpstreamTimeout(sink != nil))
	defer cancelRequest()
	defer func() {
		if err != nil && ctx.Err() == nil && requestCtx.Err() == context.DeadlineExceeded {
			var previous *CallError
			var failure *diagnostics.Failure
			if errors.As(err, &previous) {
				failure = previous.Failure
			}
			if failure != nil {
				failure = failure.Normalize()
				failure.Category, failure.Retryability = "timeout", "retryable"
			}
			err = &CallError{Status: 504, Code: "upstream_timeout", Failure: failure}
		}
	}()
	var data []byte
	var usage TokenUsage
	for candidateIndex, key := range keys {
		// Empty entries are used to mark cooled candidates only when the pool
		// readiness callback is active. Keep the single-client path's historical
		// behavior, where an empty API key is still sent upstream.
		if key == "" && callbacks.Ready != nil {
			continue
		}
		if callbacks.Ready != nil {
			ready, cooldown := callbacks.Ready(candidateIndex)
			if !ready {
				keys[candidateIndex] = ""
				earliestCooldown := cooldown
				hasReadyCandidate := false
				for nextIndex := candidateIndex + 1; nextIndex < len(keys); nextIndex++ {
					if keys[nextIndex] == "" {
						continue
					}
					nextReady, nextCooldown := callbacks.Ready(nextIndex)
					if nextReady {
						hasReadyCandidate = true
						break
					}
					keys[nextIndex] = ""
					if nextCooldown > 0 && (earliestCooldown == 0 || nextCooldown < earliestCooldown) {
						earliestCooldown = nextCooldown
					}
				}
				if !hasReadyCandidate {
					err = &CallError{Status: 429, Code: "provider_keys_cooling_down", Cooldown: earliestCooldown, UpstreamNotAttempted: true, RequestStartedAt: started, QueueMS: a.QueueMS}
					if callbacks.YieldUnavailable {
						yielded, id = true, ""
					}
					break
				}
				continue
			}
		}
		// Recheck after the dispatch queue and readiness callbacks. A failure in
		// another request must not let queued/new SDK retries generate more work.
		if c.Ledger.AuditUnavailable() {
			yielded, id = true, ""
			return nil, "", &CallError{Status: 503, Code: "audit_unavailable", UpstreamNotAttempted: true}
		}
		attempted = true
		selectedCandidate = candidateIndex
		a.BackendID = diagnostics.KeyLabel(c.BackendID)
		if callbacks.KeyLabel != nil {
			a.KeyLabel = diagnostics.KeyLabel(callbacks.KeyLabel(candidateIndex))
		}
		data, usage, err = c.doAttempt(requestCtx, clientProtocol, providerBody, model, sink, options, limits, id, key, &observed, &delivered, func() func(HTTPAttemptResult) {
			if callbacks.Attempt == nil {
				return nil
			}
			return callbacks.Attempt(candidateIndex)
		})
		var callErr *CallError
		if err != nil && errors.As(err, &callErr) {
			if callErr.Failure != nil {
				callErr.Failure.KeyLabel, callErr.Failure.BackendID = a.KeyLabel, a.BackendID
			}
			if callErr.Category == ProviderErrorRateLimited {
				a.Events = append(a.Events, "rate_limit")
			}
			if callbacks.Failed != nil {
				callbacks.Failed(candidateIndex, callErr)
			}
		}
		break
	}
	if err != nil {
		if observed.Len() > 0 {
			localEstimate(observed.Bytes())
		}
		return nil, id, err
	}
	a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens = usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite
	if priced {
		a.Currency = price.Currency
		a.PriceSnapshot, _ = json.Marshal(price)
		a.EstimatedCost = price.Estimate(c.Protocol, usage)
	}
	if a.EstimatedCost == nil {
		if sink != nil {
			localEstimate(observed.Bytes())
		} else {
			localEstimate(data)
		}
	}
	if c.PromptCache != nil && !options.RequestScopedSession {
		c.PromptCache.Remember(c.Protocol, model, c.cacheSession(clientProtocol, options.SessionID), body)
	}
	a.Status = "ok"
	return data, id, nil
}

func (c *Client) cacheSession(clientProtocol, session string) string {
	if session == "" || c.CacheNamespace == "" {
		return session
	}
	return c.CacheNamespace + "\x00" + clientProtocol + "\x00" + session
}

func logConversionWarnings(logger *slog.Logger, clientProtocol, providerProtocol string, fields []string) {
	if len(fields) == 0 {
		return
	}
	observability.LoggerOrDiscard(logger).Warn("protocol conversion omitted unsupported fields",
		slog.Int("schema_version", observability.SchemaVersion),
		slog.String("event", "protocol_conversion_omitted_fields"),
		slog.String("client_protocol", clientProtocol),
		slog.String("provider_protocol", providerProtocol),
		slog.Int("field_count", len(sortedUniqueStrings(append([]string(nil), fields...)))),
	)
}

func requestEndpoint(clientProtocol string) string {
	if clientProtocol == "anthropic" {
		return "messages"
	}
	return "chat_completions"
}

func terminalHTTPStatus(clientProtocol string, delivered bool, err error) int {
	if err == nil || delivered {
		return http.StatusOK
	}
	var callErr *CallError
	if !errors.As(err, &callErr) {
		return http.StatusInternalServerError
	}
	status := callErr.Status
	if callErr.Category != "" && (status < 400 || status > 599) {
		status, _, _, _ = callErr.Category.ClientError(clientProtocol)
	}
	if status < 100 || status > 599 {
		return http.StatusInternalServerError
	}
	return status
}

func (c *Client) doAttempt(ctx context.Context, clientProtocol string, providerBody []byte, model string, sink StreamSink, options CallOptions, limits Limits, id, apiKey string, observed *bytes.Buffer, delivered *bool, observers ...HTTPAttemptObserver) (attemptResponse []byte, attemptUsage TokenUsage, attemptErr error) {
	path := "/chat/completions"
	if c.Protocol == "anthropic" {
		path = "/messages"
	}
	var headersWritten atomic.Bool
	var phase atomic.Uint32
	advance := func(next uint32) {
		for previous := phase.Load(); previous < next; previous = phase.Load() {
			if phase.CompareAndSwap(previous, next) {
				break
			}
		}
	}
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { advance(1) },
		ConnectStart:         func(string, string) { advance(2) },
		TLSHandshakeStart:    func() { advance(3) },
		GotConn:              func(httptrace.GotConnInfo) { advance(4) },
		WroteHeaders:         func() { headersWritten.Store(true); advance(5) },
		GotFirstResponseByte: func() { advance(6) },
	}
	var upstreamResponse *http.Response
	defer func() {
		var callErr *CallError
		if !errors.As(attemptErr, &callErr) {
			return
		}
		if callErr.Failure == nil {
			callErr.Failure = &diagnostics.Failure{}
		}
		f := callErr.Failure
		if f.Source == "" {
			switch {
			case callErr.Code == "downstream_write_error":
				f.Source = "downstream"
			case phase.Load() == 8:
				f.Source = "conversion"
			case upstreamResponse == nil:
				f.Source = "transport"
			case sink != nil:
				f.Source = "stream"
			default:
				f.Source = "response"
			}
		}
		phases := []string{"request", "dns", "connect", "tls", "request", "response_headers", "response_body", "stream", "conversion"}
		f.Phase = phases[phase.Load()]
		if upstreamResponse != nil {
			f.HTTPStatus = upstreamResponse.StatusCode
			if f.ProviderRequestID == "" {
				f.ProviderRequestID = providerRequestID(upstreamResponse.Header)
			}
		}
		f.Category = SafeFailureClass(callErr)
		if callErr.Category.ProviderScopedTerminal() {
			f.CooldownScope = "provider"
		}
		if f.Category == "upstream_error" {
			f.Category = "unknown"
		}
		f.Retryability = failureRetryability(callErr)
		secrets := append([]string{apiKey, options.SessionID}, c.RedactValues...)
		callErr.Failure = f.Redact(secrets...)
		if c.ForwardErrorMessages != nil && !*c.ForwardErrorMessages {
			callErr.Failure.ProviderMessage = ""
		}
		for _, secret := range secrets {
			if secret != "" && strings.Contains(callErr.ProviderCode, secret) {
				callErr.ProviderCode = ""
			}
		}
	}()
	requestCtx := httptrace.WithClientTrace(ctx, trace)
	req, err := http.NewRequestWithContext(requestCtx, "POST", strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(providerBody))
	if err != nil {
		return nil, TokenUsage{}, &CallError{Status: 502, Code: "upstream_request_failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Protocol == "openai" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", c.APIVersion)
		if options.AnthropicBeta != "" {
			req.Header.Set("anthropic-beta", options.AnthropicBeta)
		}
	}
	if options.SessionID != "" {
		req.Header.Set(SessionIDHeader, options.SessionID)
	}
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if ctx.Err() != nil {
		return nil, TokenUsage{}, &CallError{Status: 502, Code: "upstream_transport_error"}
	}
	timedOut := false
	if len(observers) > 0 && observers[0] != nil {
		if finish := observers[0](); finish != nil {
			defer func() { finish(httpAttemptResult(ctx, attemptErr, timedOut)) }()
		}
	}
	resp, err := client.Do(req)
	upstreamResponse = resp
	if err != nil {
		var network interface{ Timeout() bool }
		timedOut = errors.As(err, &network) && network.Timeout()
		callErr := &CallError{Status: 502, Code: "upstream_transport_error"}
		if !headersWritten.Load() && ctx.Err() == nil {
			callErr.Cooldown = 5 * time.Second
			callErr.UpstreamNotAttempted = true
			callErr.FailoverSafe = true
		}
		return nil, TokenUsage{}, callErr
	}
	defer resp.Body.Close()
	advance(6)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, TokenUsage{}, upstreamError(resp, c.ErrorCodeMappings...)
	}
	recordedBody := observedReader{Reader: resp.Body, observed: observed, timedOut: &timedOut}
	if sink != nil {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			return nil, TokenUsage{}, &CallError{Status: 502, Code: "invalid_upstream_content_type"}
		}
		advance(7)
		translator := newStreamTranslator(c.Protocol, clientProtocol, model)
		var completion bytes.Buffer
		usage, data, err := readStreamWithLimits(c.Protocol, recordedBody, limits, func(frame []byte) error {
			if translator != nil {
				var translateErr error
				frame, translateErr = translator.frame(frame)
				if translateErr != nil {
					advance(8)
					var conversion *TranslationError
					if errors.As(translateErr, &conversion) {
						return &CallError{Status: 502, Code: conversion.Code, Param: conversion.Field}
					}
					return &CallError{Status: 502, Code: "invalid_upstream_stream"}
				}
			}
			if options.ResponseModel != "" {
				frame = setStreamResponseModel(clientProtocol, frame, options.ResponseModel)
			}
			if len(frame) == 0 {
				return nil
			}
			if completion.Len() > 0 || streamSignalsCompletion(clientProtocol, frame) {
				if int64(completion.Len())+int64(len(frame)) > limits.StreamBytes {
					return &CallError{Status: 502, Code: "upstream_response_too_large"}
				}
				// A stop reason can complete the client's turn before [DONE] or
				// message_stop. Hold it and the ordered tail under the stream limit.
				completion.Write(frame)
				return nil
			}
			// Treat even a failing sink as potentially having written bytes. Once
			// the downstream response may have started, switching keys is unsafe.
			*delivered = true
			if err := sink(id, frame); err != nil {
				return &CallError{Status: 502, Code: "downstream_write_error"}
			}
			return nil
		}, streamErrorContext{response: resp, mappings: c.ErrorCodeMappings})
		if err == nil && translator != nil {
			data, err = translator.terminal(data)
			if err != nil {
				advance(8)
			}
		}
		if translator != nil {
			logConversionWarnings(c.Logger, clientProtocol, c.Protocol, translator.warnings())
		}
		if err != nil {
			var conversion *TranslationError
			if errors.As(err, &conversion) {
				return nil, TokenUsage{}, &CallError{Status: 502, Code: conversion.Code, Param: conversion.Field}
			}
			return nil, TokenUsage{}, err
		}
		if int64(completion.Len())+int64(len(data)) > limits.StreamBytes {
			return nil, TokenUsage{}, &CallError{Status: 502, Code: "upstream_response_too_large"}
		}
		data = append(completion.Bytes(), data...)
		return data, usage, nil
	}
	data, err := io.ReadAll(io.LimitReader(recordedBody, limits.ResponseBytes+1))
	if err != nil {
		return nil, TokenUsage{}, &CallError{Status: 502, Code: "upstream_read_error"}
	}
	if int64(len(data)) > limits.ResponseBytes {
		return nil, TokenUsage{}, &CallError{Status: 502, Code: "upstream_response_too_large"}
	}
	usage, err := ValidateResponse(c.Protocol, data)
	if err != nil {
		return nil, TokenUsage{}, &CallError{Status: 502, Code: "invalid_upstream_response"}
	}
	var responseWarnings []string
	advance(8)
	data, responseWarnings, err = TranslateResponseWithWarnings(c.Protocol, clientProtocol, data, model)
	logConversionWarnings(c.Logger, clientProtocol, c.Protocol, responseWarnings)
	if err != nil {
		var conversion *TranslationError
		if errors.As(err, &conversion) {
			return nil, TokenUsage{}, &CallError{Status: 502, Code: conversion.Code, Param: conversion.Field}
		}
		return nil, TokenUsage{}, &CallError{Status: 502, Code: "invalid_upstream_response"}
	}
	if options.ResponseModel != "" {
		data = setResponseModel(data, options.ResponseModel)
	}
	return data, usage, nil
}
