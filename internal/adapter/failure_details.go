package adapter

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hs3180/tidemux/internal/diagnostics"
)

func providerFailure(body []byte, response *http.Response, source string, mappings []ProviderErrorMapping) *diagnostics.Failure {
	f := &diagnostics.Failure{Source: source, CodeState: "missing"}
	if response != nil {
		f.HTTPStatus = response.StatusCode
		f.ProviderRequestID = providerRequestID(response.Header)
	}
	f.ErrorStatus = providerErrorHTTPStatus(body)
	var detail struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &detail) == nil {
		f.ProviderType, f.ProviderMessage = detail.Error.Type, detail.Error.Message
	}
	code := providerErrorCode(body)
	if code != "" {
		f.ProviderCode = code
		status := f.HTTPStatus
		if source == "stream_error" {
			status = f.ErrorStatus
		}
		_, f.ConfiguredCode = resolveProviderErrorCategory(code, status, mappings)
	} else {
		var envelope struct {
			Error map[string]json.RawMessage `json:"error"`
		}
		if json.Unmarshal(body, &envelope) != nil {
			f.CodeState = "invalid"
		} else if envelope.Error["code"] != nil || envelope.Error["type"] != nil {
			f.CodeState = "redacted"
		}
	}
	// The attempt owner removes known credentials and sessions before
	// normalizing/truncating this text or exposing it outside the adapter.
	return f
}

func providerRequestID(header http.Header) string {
	for _, name := range []string{"request-id", "x-request-id", "x-ms-request-id"} {
		f := (&diagnostics.Failure{ProviderRequestID: header.Get(name)}).Normalize()
		if f.ProviderRequestID != "" {
			return f.ProviderRequestID
		}
	}
	return ""
}

func failureRetryability(e *CallError) string {
	if e.Category.ProviderScopedTerminal() {
		return "terminal"
	}
	if e.Code == "request_canceled" || e.Code == "server_shutting_down" || e.Code == "downstream_write_error" {
		return "terminal"
	}
	switch e.Category {
	case ProviderErrorRateLimited, ProviderErrorTemporarilyUnavailable:
		return "retryable"
	case ProviderErrorAuthentication, ProviderErrorPermissionDenied, ProviderErrorPolicyDenied, ProviderErrorInvalidRequest, ProviderErrorModelNotFound, ProviderErrorInsufficientBalance, ProviderErrorQuotaExhausted:
		return "terminal"
	}
	status := e.UpstreamStatus
	if e.Failure != nil && e.Failure.ErrorStatus != 0 {
		status = e.Failure.ErrorStatus
	}
	if status == 408 || status == 409 || status == 429 || status >= 500 && status <= 599 {
		return "retryable"
	}
	if status >= 400 && status <= 499 {
		return "terminal"
	}
	if e.Code == "upstream_timeout" || e.Code == "upstream_transport_error" || e.Code == "upstream_read_error" || e.Code == "upstream_stream_read_error" || e.Code == "incomplete_upstream_stream" {
		return "retryable"
	}
	return "unknown"
}

func numericProviderCode(raw json.RawMessage) string {
	value := strings.TrimSpace(string(raw))
	if decimalSeconds(value) && safeProviderErrorCode(value) {
		return value
	}
	return ""
}
