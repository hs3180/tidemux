package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

type Client struct {
	Limits                                          Limits
	Protocol, BaseURL, APIKey, APIVersion, Upstream string
	MaxOutputTokens                                 int64
	Prices                                          map[string]Price
	PromptCache                                     *PromptCache
	HTTP                                            *http.Client
	Ledger                                          *ledger.Ledger
	Gate                                            *limiter.ConcurrencyGate
}
type CallError struct {
	Status         int
	Code           string
	Param          string
	UpstreamStatus int
	Retryable      bool
	Cooldown       time.Duration
}

func (e *CallError) Error() string { return e.Code }

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
	return c.callWithKeyCandidates(clientProtocol, ctx, body, model, sink, options, []string{c.APIKey}, nil)
}

// CallFromKeyCandidates runs bounded same-provider credential attempts under
// one request ID and writes one terminal audit record. A retryable failure can
// advance only while no stream frame has reached the caller.
func (c *Client) CallFromKeyCandidates(clientProtocol string, ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions, keys []string, onFailure func(int, *CallError)) ([]byte, string, error) {
	return c.callWithKeyCandidates(clientProtocol, ctx, body, model, sink, options, keys, onFailure)
}

func (c *Client) callWithKeyCandidates(clientProtocol string, ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions, keys []string, onFailure func(int, *CallError)) (response []byte, id string, err error) {
	if err := options.Validate(clientProtocol); err != nil {
		return nil, "", &CallError{Status: 400, Code: err.Error(), Param: ValidationParameter(err)}
	}
	if len(keys) == 0 {
		keys = []string{c.APIKey}
	}
	started := time.Now()
	nonce := make([]byte, 16)
	if _, e := rand.Read(nonce); e != nil {
		return nil, "", &CallError{Status: 500, Code: "request_id_failed"}
	}
	id = hex.EncodeToString(nonce)
	a := ledger.Audit{ID: id, TimestampMS: started.UnixMilli(), Protocol: c.Protocol, Upstream: c.Upstream, Model: model, Status: "error", Events: []string{}}
	providerBody, preparedModel, requestWarnings, e := PrepareRequestWithWarnings(clientProtocol, c.Protocol, body, model, c.MaxOutputTokens)
	if e != nil {
		return nil, id, &CallError{Status: 400, Code: e.Error(), Param: ValidationParameter(e)}
	}
	logConversionWarnings(clientProtocol, c.Protocol, requestWarnings)
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
			response = nil
			err = &CallError{Status: 500, Code: "audit_failed_do_not_retry_blindly"}
		}
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
	delivered := false
	for candidateIndex, key := range keys {
		attempted = true
		data, usage, err = c.doAttempt(requestCtx, clientProtocol, providerBody, model, sink, options, limits, id, key, &observed, &delivered)
		if err == nil {
			break
		}
		var callErr *CallError
		if errors.As(err, &callErr) && callErr.UpstreamStatus == http.StatusTooManyRequests {
			a.Events = append(a.Events, "rate_limit_429")
		}
		if !errors.As(err, &callErr) || !callErr.Retryable {
			break
		}
		if onFailure != nil {
			onFailure(candidateIndex, callErr)
		}
		if !canFailover(callErr, candidateIndex+1 < len(keys), delivered, requestCtx) {
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

func logConversionWarnings(clientProtocol, providerProtocol string, fields []string) {
	if len(fields) == 0 {
		return
	}
	log.Printf("tidemux: cross-protocol conversion omitted fields client_protocol=%s provider_protocol=%s fields=%q", clientProtocol, providerProtocol, sortedUniqueStrings(append([]string(nil), fields...)))
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
		return nil, TokenUsage{}, upstreamError(resp)
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
		})
		if err == nil && translator != nil {
			data, err = translator.terminal(data)
		}
		if translator != nil {
			logConversionWarnings(clientProtocol, c.Protocol, translator.warnings())
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
	logConversionWarnings(clientProtocol, c.Protocol, responseWarnings)
	if err != nil {
		var conversion *TranslationError
		if errors.As(err, &conversion) {
			return nil, TokenUsage{}, &CallError{Status: 502, Code: conversion.Code, Param: conversion.Field}
		}
		return nil, TokenUsage{}, &CallError{Status: 502, Code: "invalid_upstream_response"}
	}
	return data, usage, nil
}
