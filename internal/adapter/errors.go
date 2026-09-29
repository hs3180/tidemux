package adapter

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRateLimitRetry = time.Second
	maximumKeyCooldown    = 24 * time.Hour
	maximumRetryAfter     = 24 * time.Hour
	maxProviderErrorBody  = 64 << 10
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

func retryAfter(resp *http.Response) (time.Duration, int64, bool) {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if decimalSeconds(value) {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			// A syntactically valid but unrepresentably large delta must not be
			// mistaken for a malformed header and retried with the short fallback.
			return maximumRetryAfter, int64(maximumRetryAfter / time.Second), true
		}
		delay := time.Duration(seconds) * time.Second
		if seconds > int64(maximumRetryAfter/time.Second) {
			delay = maximumRetryAfter
		}
		return delay, seconds, true
	}
	if at, err := http.ParseTime(value); err == nil {
		delay := time.Until(at)
		if delay < 0 {
			delay = 0
		}
		if delay > maximumRetryAfter {
			delay = maximumRetryAfter
		}
		seconds := int64(math.Ceil(delay.Seconds()))
		return delay, seconds, true
	}
	return 0, 0, false
}

func decimalSeconds(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func providerErrorCode(body []byte) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Error) == 0 {
		return ""
	}
	var detail struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if json.Unmarshal(envelope.Error, &detail) != nil {
		return ""
	}
	if safeProviderErrorCode(detail.Code) {
		return detail.Code
	}
	if safeProviderErrorCode(detail.Type) {
		return detail.Type
	}
	return ""
}

func providerErrorHTTPStatus(body []byte) int {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Error) == 0 {
		return 0
	}
	var detail struct {
		Status     int `json:"status"`
		StatusCode int `json:"status_code"`
	}
	if json.Unmarshal(envelope.Error, &detail) != nil {
		return 0
	}
	for _, status := range []int{detail.Status, detail.StatusCode} {
		if status >= 400 && status <= 599 {
			return status
		}
	}
	return 0
}

func providerStreamError(body []byte, stream streamErrorContext) *CallError {
	providerCode := providerErrorCode(body)
	if providerCode == "" {
		return &CallError{Status: 502, Code: "upstream_stream_error"}
	}
	status := providerErrorHTTPStatus(body)
	category, ok := resolveProviderErrorCategory(providerCode, status, stream.mappings)
	if !ok {
		return &CallError{Status: 502, Code: "upstream_stream_error"}
	}
	response := &http.Response{StatusCode: 200, Header: make(http.Header)}
	if stream.response != nil {
		response.StatusCode = stream.response.StatusCode
		response.Header = stream.response.Header
	}
	if status > 0 {
		response.StatusCode = status
	}
	return mappedProviderError(response, category, providerCode)
}

func mappedHTTPStatus(category ProviderErrorCategory, upstreamStatus int) int {
	status, _, _, _ := category.ClientError("openai")
	if upstreamStatus < 400 || upstreamStatus > 499 {
		return status
	}
	switch category {
	case ProviderErrorInsufficientBalance:
		if upstreamStatus == 402 || upstreamStatus == 403 {
			return upstreamStatus
		}
	case ProviderErrorAuthentication:
		if upstreamStatus == 401 || upstreamStatus == 403 {
			return upstreamStatus
		}
	case ProviderErrorPermissionDenied:
		if upstreamStatus == 403 {
			return upstreamStatus
		}
	case ProviderErrorPolicyDenied:
		if upstreamStatus == 400 || upstreamStatus == 403 {
			return upstreamStatus
		}
	case ProviderErrorInvalidRequest:
		if upstreamStatus == 400 || upstreamStatus == 422 {
			return upstreamStatus
		}
	case ProviderErrorModelNotFound:
		if upstreamStatus == 400 || upstreamStatus == 404 {
			return upstreamStatus
		}
	}
	return status
}

func mappedProviderError(resp *http.Response, category ProviderErrorCategory, providerCode string) *CallError {
	_, code, _, _ := category.ClientError("openai")
	status := mappedHTTPStatus(category, resp.StatusCode)
	result := &CallError{Status: status, Code: code, ProviderCode: providerCode, Category: category, UpstreamStatus: resp.StatusCode}
	if category == ProviderErrorRateLimited {
		result.RateLimited = true
		result.Retryable = true
		result.RetryDelay, result.RetryAfterSecs, result.RetryProvided = retryAfter(resp)
		if result.RetryProvided {
			result.Cooldown = boundedCooldown(result.RetryDelay)
		} else {
			result.Cooldown = defaultRateLimitRetry
		}
	} else if category == ProviderErrorAuthentication {
		result.Retryable = true
		result.Cooldown = 30 * time.Second
	}
	return result
}

// Preserve safe built-in categories without forwarding provider messages, which
// can contain echoed prompts, credentials, or account details. Exact configured
// error codes can add provider-specific meaning without parsing arbitrary text.
func upstreamError(resp *http.Response, mappings ...ProviderErrorMapping) *CallError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProviderErrorBody))
	if providerCode := providerErrorCode(body); providerCode != "" {
		if category, ok := resolveProviderErrorCategory(providerCode, resp.StatusCode, mappings); ok {
			return mappedProviderError(resp, category, providerCode)
		}
	}
	if resp.StatusCode == 429 {
		result := mappedProviderError(resp, ProviderErrorRateLimited, "")
		result.ProviderCode = ""
		return result
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &CallError{Status: 502, Code: "upstream_error", UpstreamStatus: resp.StatusCode, Retryable: true, Cooldown: 30 * time.Second}
	}
	if resp.StatusCode != 400 && resp.StatusCode != 422 {
		return &CallError{Status: 502, Code: "upstream_error", UpstreamStatus: resp.StatusCode}
	}
	code := "upstream_invalid_request"
	param := ""
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Param   string `json:"param"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
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
	return &CallError{Status: resp.StatusCode, Code: code, Param: param, UpstreamStatus: resp.StatusCode}
}
