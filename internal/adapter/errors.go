package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRateLimitCooldown = time.Minute
	maximumKeyCooldown       = 5 * time.Minute
)

func boundedCooldown(delay time.Duration) time.Duration {
	if delay < time.Second {
		return time.Second
	}
	if delay > maximumKeyCooldown {
		return maximumKeyCooldown
	}
	return delay
}

func retryAfter(resp *http.Response) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return time.Second
		}
		if seconds >= int64(maximumKeyCooldown/time.Second) {
			return maximumKeyCooldown
		}
		return boundedCooldown(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(value); err == nil {
		return boundedCooldown(time.Until(at))
	}
	return defaultRateLimitCooldown
}

// Preserve actionable error categories without forwarding provider text, which
// can contain echoed prompts, credentials, or account details. A client can
// then apply its own documented recovery (for example unsupported JSON Schema).
func upstreamError(resp *http.Response) *CallError {
	if resp.StatusCode == 429 {
		return &CallError{Status: 429, Code: "upstream_rate_limited", UpstreamStatus: resp.StatusCode, Retryable: true, Cooldown: retryAfter(resp)}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &CallError{Status: 502, Code: "upstream_error", UpstreamStatus: resp.StatusCode, Retryable: true, Cooldown: 30 * time.Second}
	}
	if resp.StatusCode != 400 && resp.StatusCode != 422 {
		return &CallError{Status: 502, Code: "upstream_error", UpstreamStatus: resp.StatusCode}
	}
	code := "upstream_invalid_request"
	param := ""
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err == nil {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
				Param   string `json:"param"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			message := strings.ToLower(envelope.Error.Message)
			param = envelope.Error.Param
			unsupported := strings.Contains(message, "not support") || strings.Contains(message, "unsupported") || strings.Contains(message, "unavailable") || strings.Contains(message, "unknown parameter") || strings.Contains(message, "unrecognized") || strings.Contains(message, "extra inputs are not permitted")
			if unsupported {
				for _, p := range []string{"response_format", "output_config", "reasoning_effort", "thinking", "temperature", "tools", "tool_choice"} {
					if envelope.Error.Param == p || strings.Contains(message, p) {
						code = "unsupported_parameter_" + p
						param = p
						break
					}
				}
			}
		}
	}
	return &CallError{Status: resp.StatusCode, Code: code, Param: param, UpstreamStatus: resp.StatusCode}
}
