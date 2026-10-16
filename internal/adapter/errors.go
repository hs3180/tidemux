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
	defaultRateLimitCooldown = time.Second
	maximumKeyCooldown       = 24 * time.Hour
	maxProviderErrorBody     = 64 << 10
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
	now := time.Now()
	until, seconds, valid := retryAfterDeadline(resp, now)
	if !valid {
		return 0, 0, false
	}
	return until.Sub(now), seconds, valid
}

// Keep the upstream deadline independent of a duration (which saturates after
// about 292 years). An unrepresentable delta is held fail-closed at the latest
// RFC3339 time; it must never turn into a short retry fallback.
func retryAfterDeadline(resp *http.Response, now time.Time) (time.Time, int64, bool) {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if decimalSeconds(value) {
		seconds, err := strconv.ParseInt(value, 10, 64)
		latest := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		maximumSeconds := latest.Unix() - now.Unix() - 1
		if err != nil || seconds > maximumSeconds {
			return latest, maximumSeconds, true
		}
		return time.Unix(now.Unix()+seconds, int64(now.Nanosecond())), seconds, true
	}
	if at, err := http.ParseTime(value); err == nil {
		if !at.After(now) {
			return now, 0, true
		}
		seconds := at.Unix() - now.Unix()
		if at.Nanosecond() > now.Nanosecond() {
			seconds++
		}
		return at, seconds, true
	}
	return time.Time{}, 0, false
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
		Code json.RawMessage `json:"code"`
		Type json.RawMessage `json:"type"`
	}
	if json.Unmarshal(envelope.Error, &detail) != nil {
		return ""
	}
	var code string
	if json.Unmarshal(detail.Code, &code) == nil && safeProviderErrorCode(code) {
		return code
	}
	if code = numericProviderCode(detail.Code); code != "" {
		return code
	}
	var kind string
	if json.Unmarshal(detail.Type, &kind) == nil && safeProviderErrorCode(kind) {
		return kind
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

func providerStreamError(body []byte, stream streamErrorContext) (result *CallError) {
	defer func() {
		result.Failure = providerFailure(body, stream.response, "stream_error", stream.mappings)
	}()
	providerCode := providerErrorCode(body)
	if providerCode == "" {
		return &CallError{Status: 502, Code: "upstream_stream_error"}
	}
	status := providerErrorHTTPStatus(body)
	category, ok := resolveErrorCategory(body, status, stream.mappings)
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
		if upstreamStatus == 400 || upstreamStatus == 413 || upstreamStatus == 422 {
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
	if category == ProviderErrorInsufficientBalance || category == ProviderErrorQuotaExhausted || category == ProviderErrorModelNotFound || category == ProviderErrorTemporarilyUnavailable {
		result.FailoverSafe = true
	}
	if category == ProviderErrorRateLimited || category == ProviderErrorQuotaExhausted || category == ProviderErrorTemporarilyUnavailable {
		now := time.Now()
		until, seconds, provided := retryAfterDeadline(resp, now)
		result.RetryAfterSecs, result.RetryProvided = seconds, provided
		if result.RetryProvided {
			result.CooldownUntil, result.Cooldown = until, until.Sub(now)
			result.RetryAfterValue = strings.TrimSpace(resp.Header.Get("Retry-After"))
		} else if category == ProviderErrorRateLimited {
			result.Cooldown = defaultRateLimitCooldown
		}
	} else if category == ProviderErrorAuthentication {
		result.Cooldown = 30 * time.Second
	}
	return result
}

// Classification uses explicit mappings and standard API identifiers, never
// arbitrary message text. The attempt owner applies the message/privacy policy.
func upstreamError(resp *http.Response, mappings ...ProviderErrorMapping) (result *CallError) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProviderErrorBody))
	defer func() { result.Failure = providerFailure(body, resp, "http_error", mappings) }()
	if category, ok := resolveErrorCategory(body, resp.StatusCode, mappings); ok {
		if category == ProviderErrorInvalidRequest {
			if invalid := invalidProviderRequestError(resp, body); invalid.Param != "" {
				return invalid
			}
		}
		return mappedProviderError(resp, category, providerErrorCode(body))
	}
	if resp.StatusCode == 429 {
		result := mappedProviderError(resp, ProviderErrorRateLimited, "")
		result.ProviderCode = ""
		return result
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &CallError{Status: 502, Code: "upstream_error", UpstreamStatus: resp.StatusCode, Cooldown: 30 * time.Second}
	}
	if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
		now := time.Now()
		until, seconds, provided := retryAfterDeadline(resp, now)
		result := &CallError{Status: 502, Code: "upstream_error", UpstreamStatus: resp.StatusCode, RetryProvided: provided}
		if provided {
			result.CooldownUntil, result.Cooldown, result.RetryAfterSecs = until, until.Sub(now), seconds
			result.RetryAfterValue = strings.TrimSpace(resp.Header.Get("Retry-After"))
		}
		return result
	}
	if resp.StatusCode != 400 && resp.StatusCode != 413 && resp.StatusCode != 422 {
		return &CallError{Status: 502, Code: "upstream_error", UpstreamStatus: resp.StatusCode}
	}
	return invalidProviderRequestError(resp, body)
}

func invalidProviderRequestError(resp *http.Response, body []byte) *CallError {
	code := "upstream_invalid_request"
	param := ""
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		message := strings.ToLower(envelope.Error.Message)
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
