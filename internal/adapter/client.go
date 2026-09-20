package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

type Client struct {
	Limits                                          Limits
	Protocol, BaseURL, APIKey, APIVersion, Upstream string
	Prices                                          map[string]Price
	PromptCache                                     *PromptCache
	HTTP                                            *http.Client
	Ledger                                          *ledger.Ledger
	Gate                                            *limiter.ConcurrencyGate
}
type CallError struct {
	Status int
	Code   string
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
	return c.call(ctx, body, model, nil, CallOptions{})
}
func (c *Client) CallStream(ctx context.Context, body []byte, model string, sink StreamSink) ([]byte, string, error) {
	return c.call(ctx, body, model, sink, CallOptions{})
}
func (c *Client) CallWithOptions(ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions) ([]byte, string, error) {
	return c.call(ctx, body, model, sink, options)
}
func (c *Client) call(ctx context.Context, body []byte, model string, sink StreamSink, options CallOptions) (response []byte, id string, err error) {
	if err := options.Validate(c.Protocol); err != nil {
		return nil, "", &CallError{400, err.Error()}
	}
	started := time.Now()
	nonce := make([]byte, 16)
	if _, e := rand.Read(nonce); e != nil {
		return nil, "", &CallError{500, "request_id_failed"}
	}
	id = hex.EncodeToString(nonce)
	a := ledger.Audit{ID: id, TimestampMS: started.UnixMilli(), Protocol: c.Protocol, Upstream: c.Upstream, Model: model, Status: "error", Events: []string{}}
	price, priced := c.Prices[model]
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
				err = &CallError{408, "request_canceled"}
			}
		}
		if e := c.Ledger.AppendAudit(a); e != nil {
			response = nil
			err = &CallError{500, "audit_failed_do_not_retry_blindly"}
		}
	}()
	admission, e := c.Gate.Acquire(ctx)
	a.QueueMS = admission.QueueWait.Milliseconds()
	if admission.Queued {
		a.Events = append(a.Events, "queue_wait")
	}
	if e != nil {
		return nil, id, &CallError{408, "request_canceled"}
	}
	defer c.Gate.Release()
	if e = ctx.Err(); e != nil {
		return nil, id, &CallError{408, "request_canceled"}
	}
	limits := c.Limits.Effective()
	requestCtx, cancelRequest := context.WithTimeout(ctx, time.Duration(limits.UpstreamTimeoutSeconds)*time.Second)
	defer cancelRequest()
	defer func() {
		if err != nil && ctx.Err() == nil && requestCtx.Err() == context.DeadlineExceeded {
			err = &CallError{504, "upstream_timeout"}
		}
	}()
	path := "/chat/completions"
	if c.Protocol == "anthropic" {
		path = "/messages"
	}
	req, e := http.NewRequestWithContext(requestCtx, "POST", strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if e != nil {
		return nil, id, &CallError{502, "upstream_request_failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Protocol == "openai" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	} else {
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", c.APIVersion)
		if options.AnthropicBeta != "" {
			req.Header.Set("anthropic-beta", options.AnthropicBeta)
		}
	}
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	attempted = true
	resp, e := client.Do(req)
	if e != nil {
		return nil, id, &CallError{502, "upstream_transport_error"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ce := upstreamError(resp)
		if resp.StatusCode == 429 {
			a.Events = append(a.Events, "rate_limit_429")
		}
		return nil, id, ce
	}
	var data []byte
	var usage TokenUsage
	recordedBody := observedReader{Reader: resp.Body, observed: &observed}
	if sink != nil {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			return nil, id, &CallError{502, "invalid_upstream_content_type"}
		}
		usage, data, e = readStreamWithLimits(c.Protocol, recordedBody, limits, func(frame []byte) error {
			if err := sink(id, frame); err != nil {
				return &CallError{502, "downstream_write_error"}
			}
			return nil
		})
		if e != nil {
			localEstimate(observed.Bytes())
			return nil, id, e
		}
	} else {
		data, e = io.ReadAll(io.LimitReader(recordedBody, limits.ResponseBytes+1))
		if e != nil {
			return nil, id, &CallError{502, "upstream_read_error"}
		}
		if int64(len(data)) > limits.ResponseBytes {
			return nil, id, &CallError{502, "upstream_response_too_large"}
		}
		usage, e = ValidateResponse(c.Protocol, data)
		if e != nil {
			localEstimate(data)
			return nil, id, &CallError{502, "invalid_upstream_response"}
		}
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
		c.PromptCache.Remember(c.Protocol, model, options.SessionID, body)
	}
	a.Status = "ok"
	return data, id, nil
}
