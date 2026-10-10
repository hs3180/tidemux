package adapter

import (
	"context"
	"errors"
)

// HTTPAttemptResult describes one initiated Do call including response/stream
// processing. It excludes audit persistence and retry/cooldown waiting.
type HTTPAttemptResult struct {
	Outcome      string
	FailureClass string
}

type HTTPAttemptObserver func() func(HTTPAttemptResult)

// SafeFailureClass never returns provider codes, messages or validation text.
func SafeFailureClass(err error) string {
	var failure *CallError
	if !errors.As(err, &failure) {
		return "upstream_error"
	}
	switch failure.Category {
	case ProviderErrorAuthentication, ProviderErrorRateLimited, ProviderErrorInsufficientBalance,
		ProviderErrorModelNotFound, ProviderErrorTemporarilyUnavailable, ProviderErrorPermissionDenied,
		ProviderErrorPolicyDenied, ProviderErrorInvalidRequest:
		return string(failure.Category)
	}
	switch failure.UpstreamStatus {
	case 401:
		return "authentication"
	case 403:
		return "permission_denied"
	case 429:
		return "rate_limited"
	}
	switch failure.Code {
	case "upstream_transport_error":
		return "transport_error"
	case "invalid_upstream_response", "invalid_upstream_stream", "invalid_upstream_content_type", "upstream_read_error", "upstream_response_too_large":
		return "invalid_response"
	case "downstream_write_error":
		return "downstream_error"
	case "upstream_timeout":
		return "timeout"
	case "request_canceled", "server_shutting_down":
		return "canceled"
	}
	return "upstream_error"
}

func httpAttemptResult(ctx context.Context, err error, timedOut bool) HTTPAttemptResult {
	if err == nil {
		return HTTPAttemptResult{Outcome: "success"}
	}
	class := SafeFailureClass(err)
	if timedOut || errors.Is(ctx.Err(), context.DeadlineExceeded) || class == "timeout" {
		return HTTPAttemptResult{"timeout", "timeout"}
	}
	if errors.Is(ctx.Err(), context.Canceled) || class == "canceled" {
		return HTTPAttemptResult{"canceled", "canceled"}
	}
	return HTTPAttemptResult{"failure", class}
}
