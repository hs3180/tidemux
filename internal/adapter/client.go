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
	"math"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
	"github.com/hs3180/tidemux/internal/observability"
)

type Client struct {
	Limits                                          Limits
	Protocol, BaseURL, APIKey, APIVersion, Upstream string
	ProviderRef                                     string
	Logger                                          *slog.Logger
	MaxOutputTokens                                 int64
	Prices                                          map[string]Price
	ErrorCodeMappings                               []ProviderErrorMapping
	PromptCache                                     *PromptCache
	HTTP                                            *http.Client
	Ledger                                          *ledger.Ledger
	Gate                                            *limiter.ConcurrencyGate
	waitRateLimit                                   func(context.Context, time.Duration) error
}
type CallError struct {
	Status int
	Code   string
	// UpstreamNotAttempted is true only when the adapter can prove that no
	// request was dispatched to the provider. The gateway may then release a
	// budget reservation instead of treating the missing usage as unknown.
	UpstreamNotAttempted bool
	// FailoverSafe marks only provider classifications that permit another
	// provider attempt without changing the requested model.
	FailoverSafe bool
	// BudgetCost preserves a known charge when the audit row itself cannot be
	// written. It is internal accounting data and is never sent to clients.
	BudgetCost     *float64
	Param          string
	ProviderCode   string
	Category       ProviderErrorCategory
	UpstreamStatus int
	Retryable      bool
	RateLimited    bool
	Cooldown       time.Duration
	RetryDelay     time.Duration
	RetryAfterSecs int64
	RetryProvided  bool
}

func (e *CallError) Error() string { return e.Code }

// KeyCandidateCallbacks rechecks cooldown state before an attempt and records
// retryable failures while preserving one audit record for the logical call.
type KeyCandidateCallbacks struct {
	Ready  func(index int) (bool, time.Duration)
	Failed func(index int, callErr *CallError) (hasNext bool, earliestCooldown time.Duration)
}

type observedReader struct {
	io.Reader
	observed *bytes.Buffer
}

func (r observedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
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

// CallFromKeyCandidates runs bounded same-provider credential attempts under
// one request ID and writes one terminal audit record. A retryable failure can
// advance only while no stream frame has reached the caller.
func (c *Client) CallFromKeyCandidates(clientProtocol string, ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions, keys []string, callbacks KeyCandidateCallbacks) ([]byte, string, error) {
	return c.callWithKeyCandidates(clientProtocol, ctx, body, model, sink, options, keys, callbacks)
}

func (c *Client) callWithKeyCandidates(clientProtocol string, ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions, keys []string, callbacks KeyCandidateCallbacks) (response []byte, id string, err error) {
	if err := options.Validate(clientProtocol); err != nil {
		return nil, "", &CallError{Status: 400, Code: err.Error(), Param: ValidationParameter(err), UpstreamNotAttempted: true}
	}
	if len(keys) == 0 {
		keys = []string{c.APIKey}
	}
	started := time.Now()
	nonce := make([]byte, 16)
	if _, e := rand.Read(nonce); e != nil {
		return nil, "", &CallError{Status: 500, Code: "request_id_failed", UpstreamNotAttempted: true}
	}
	id = hex.EncodeToString(nonce)
	a := ledger.Audit{ID: id, TimestampMS: started.UnixMilli(), Protocol: c.Protocol, Upstream: c.Upstream, Model: model, Status: "error", Events: []string{}}
	providerBody, preparedModel, requestWarnings, e := PrepareRequestWithWarnings(clientProtocol, c.Protocol, body, model, c.MaxOutputTokens)
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
	delivered := false
	auditPersisted := false
	localEstimate := func(response []byte) {
		if !priced || a.EstimatedCost != nil {
			return
		}
		cost, usage, source := c.PromptCache.LocalEstimate(c.Protocol, model, options.SessionID, body, response, price)
		if cost != nil {
			a.InputTokens, a.OutputTokens, a.CacheReadTokens = usage.Input, usage.Output, usage.CacheRead
			a.Currency, a.PriceSnapshot, a.EstimatedCost, a.CostSource = price.Currency, mustJSON(price), cost, source
		}
	}
	defer func() {
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
				a.Status = "canceled"
				a.ErrorCode = "request_canceled"
				err = &CallError{Status: 408, Code: "request_canceled"}
			}
		}
		if e := c.Ledger.AppendAudit(a); e != nil {
			observability.LoggerOrDiscard(c.Logger).Error("request audit append failed",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "request_audit_write_failure"),
				slog.String("request_id", id),
				slog.String("failure_code", "audit_write_failed"),
			)
			response = nil
			callErr := &CallError{Status: 500, Code: "audit_failed_do_not_retry_blindly"}
			if a.EstimatedCost != nil {
				cost := *a.EstimatedCost
				callErr.BudgetCost = &cost
			}
			err = callErr
		} else {
			auditPersisted = true
		}
		if err != nil {
			var callErr *CallError
			if errors.As(err, &callErr) {
				callErr.UpstreamNotAttempted = !attempted
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
			Model: a.Model, Outcome: outcome, ErrorCode: errorCode,
			HTTPStatus: status, LatencyMS: a.LatencyMS, QueueTimeMS: a.QueueMS,
			UpstreamAttempted: attempted, RecordPersisted: auditPersisted,
		}.Log(c.Logger)
	}()
	admission, e := c.Gate.Acquire(ctx)
	a.QueueMS = admission.QueueWait.Milliseconds()
	if admission.Queued {
		a.Events = append(a.Events, "queue_wait")
	}
	if e != nil {
		return nil, id, &CallError{Status: 408, Code: "request_canceled"}
	}
	defer c.Gate.Release()
	if e = ctx.Err(); e != nil {
		return nil, id, &CallError{Status: 408, Code: "request_canceled"}
	}
	limits := c.Limits.Effective()
	requestCtx, cancelRequest := context.WithTimeout(ctx, time.Duration(limits.UpstreamTimeoutSeconds)*time.Second)
	defer cancelRequest()
	defer func() {
		if err != nil && ctx.Err() == nil && requestCtx.Err() == context.DeadlineExceeded {
			err = &CallError{Status: 504, Code: "upstream_timeout"}
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
					err = &CallError{Status: 503, Code: "provider_keys_cooling_down", Cooldown: earliestCooldown}
					break
				}
				continue
			}
		}
		attempted = true
		handleRateLimit := func(callErr *CallError) {
			a.Events = append(a.Events, "rate_limit")
			if callbacks.Failed != nil {
				callbacks.Failed(candidateIndex, callErr)
			}
		}
		recordRateLimitRetry := func() { a.Events = append(a.Events, "rate_limit_retry") }
		data, usage, err = c.doAttemptWithRateLimitRetries(requestCtx, clientProtocol, providerBody, model, sink, options, limits, id, key, &observed, &delivered, handleRateLimit, recordRateLimitRetry)
		if err == nil {
			break
		}
		var callErr *CallError
		if !errors.As(err, &callErr) || !callErr.Retryable {
			break
		}
		hasNext := candidateIndex+1 < len(keys)
		var coolingDelay time.Duration
		if callbacks.Failed != nil {
			hasNext, coolingDelay = callbacks.Failed(candidateIndex, callErr)
		}
		if !hasNext && coolingDelay > 0 {
			err = &CallError{Status: 503, Code: "provider_keys_cooling_down", Cooldown: coolingDelay}
			break
		}
		if !canFailover(callErr, hasNext, delivered, requestCtx) {
			break
		}
		a.Events = append(a.Events, "key_failover")
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
	if c.PromptCache != nil {
		c.PromptCache.Remember(clientProtocol, model, options.SessionID, body)
	}
	a.Status = "ok"
	return data, id, nil
}

func (c *Client) doAttemptWithRateLimitRetries(ctx context.Context, clientProtocol string, providerBody []byte, model string, sink StreamSink, options CallOptions, limits Limits, id, apiKey string, observed *bytes.Buffer, delivered *bool, onRateLimit func(*CallError), onRetry func()) ([]byte, TokenUsage, error) {
	for attempt := 1; ; attempt++ {
		data, usage, err := c.doAttempt(ctx, clientProtocol, providerBody, model, sink, options, limits, id, apiKey, observed, delivered)
		if err == nil {
			return data, usage, nil
		}
		var callErr *CallError
		if !errors.As(err, &callErr) || !callErr.RateLimited {
			return nil, TokenUsage{}, err
		}
		if onRateLimit != nil {
			onRateLimit(callErr)
		}
		delay := callErr.RetryDelay
		if !callErr.RetryProvided {
			delay = rateLimitBackoff(attempt)
			callErr.RetryAfterSecs = int64(math.Ceil(delay.Seconds()))
		}
		if attempt >= 3 || *delivered || !retryDelayFits(ctx, delay) {
			callErr.Retryable = false
			return nil, TokenUsage{}, callErr
		}
		if delay > 0 {
			var waitErr error
			if c.waitRateLimit != nil {
				waitErr = c.waitRateLimit(ctx, delay)
			} else {
				waitErr = waitContext(ctx, delay)
			}
			if waitErr != nil {
				return nil, TokenUsage{}, &CallError{Status: 408, Code: "request_canceled"}
			}
		}
		if onRetry != nil {
			onRetry()
		}
	}
}

func rateLimitBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Second * time.Duration(1<<(attempt-1))
}

func retryDelayFits(ctx context.Context, delay time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || delay < time.Until(deadline)
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

func canFailover(callErr *CallError, hasNext, delivered bool, ctx context.Context) bool {
	return callErr != nil && callErr.Retryable && hasNext && !delivered && ctx.Err() == nil
}

func (c *Client) doAttempt(ctx context.Context, clientProtocol string, providerBody []byte, model string, sink StreamSink, options CallOptions, limits Limits, id, apiKey string, observed *bytes.Buffer, delivered *bool) ([]byte, TokenUsage, error) {
	path := "/chat/completions"
	if c.Protocol == "anthropic" {
		path = "/messages"
	}
	var headersWritten atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { headersWritten.Store(true) }}
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
	resp, err := client.Do(req)
	if err != nil {
		callErr := &CallError{Status: 502, Code: "upstream_transport_error"}
		if !headersWritten.Load() && ctx.Err() == nil {
			callErr.Retryable = true
			callErr.Cooldown = 5 * time.Second
		}
		return nil, TokenUsage{}, callErr
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, TokenUsage{}, upstreamError(resp, c.ErrorCodeMappings...)
	}
	recordedBody := observedReader{Reader: resp.Body, observed: observed}
	if sink != nil {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			return nil, TokenUsage{}, &CallError{Status: 502, Code: "invalid_upstream_content_type"}
		}
		translator := newStreamTranslator(c.Protocol, clientProtocol, model)
		usage, data, err := readStreamWithLimits(c.Protocol, recordedBody, limits, func(frame []byte) error {
			if translator != nil {
				var translateErr error
				frame, translateErr = translator.frame(frame)
				if translateErr != nil {
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
