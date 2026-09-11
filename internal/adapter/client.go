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
	Protocol, BaseURL, APIKey, APIVersion, Upstream string
	Prices                                          map[string]Price
	HTTP                                            *http.Client
	Ledger                                          *ledger.Ledger
	Gate                                            *limiter.ConcurrencyGate
}
type CallError struct {
	Status int
	Code   string
}

func (e *CallError) Error() string { return e.Code }
func (c *Client) Call(ctx context.Context, body []byte, model string) (response []byte, id string, err error) {
	started := time.Now()
	nonce := make([]byte, 16)
	if _, e := rand.Read(nonce); e != nil {
		return nil, "", &CallError{500, "request_id_failed"}
	}
	id = hex.EncodeToString(nonce)
	a := ledger.Audit{ID: id, TimestampMS: started.UnixMilli(), Protocol: c.Protocol, Upstream: c.Upstream, Model: model, Status: "error", Events: []string{}}
	defer func() {
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
	path := "/chat/completions"
	if c.Protocol == "anthropic" {
		path = "/messages"
	}
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if e != nil {
		return nil, id, &CallError{502, "upstream_request_failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Protocol == "openai" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	} else {
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", c.APIVersion)
	}
	client := http.Client{Timeout: 60 * time.Second}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := client.Do(req)
	if e != nil {
		return nil, id, &CallError{502, "upstream_transport_error"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := 502
		code := "upstream_error"
		if resp.StatusCode == 429 {
			status = 429
			code = "upstream_rate_limited"
			a.Events = append(a.Events, "rate_limit_429")
		}
		return nil, id, &CallError{status, code}
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if e != nil {
		return nil, id, &CallError{502, "upstream_read_error"}
	}
	if len(data) > 8<<20 {
		return nil, id, &CallError{502, "upstream_response_too_large"}
	}
	usage, e := ValidateResponse(c.Protocol, data)
	if e != nil {
		return nil, id, &CallError{502, "invalid_upstream_response"}
	}
	a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens = usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite
	if price, ok := c.Prices[model]; ok {
		a.Currency = price.Currency
		a.PriceSnapshot, _ = json.Marshal(price)
		a.EstimatedCost = price.Estimate(c.Protocol, usage)
	}
	a.Status = "ok"
	return data, id, nil
}
