package adapter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSafeUpstreamParameterErrors(t *testing.T) {
	for _, c := range []struct {
		status     int
		body, code string
		retryable  bool
		cooldown   time.Duration
		retryAfter string
	}{
		{status: 400, body: `{"error":{"message":"response_format is not supported. SECRET prompt detail"}}`, code: "unsupported_parameter_response_format"},
		{status: 422, body: `{"error":{"message":"Extra inputs are not permitted for output_config. SECRET"}}`, code: "unsupported_parameter_output_config"},
		{status: 400, body: `{"error":{"message":"private account SECRET"}}`, code: "upstream_invalid_request"},
		{status: 401, body: `{"error":{"message":"SECRET key rejected"}}`, code: "upstream_error", retryable: true, cooldown: 30 * time.Second},
		{status: 403, body: `{"error":{"message":"SECRET forbidden"}}`, code: "upstream_error", retryable: true, cooldown: 30 * time.Second},
		{status: 500, body: `{"error":{"message":"response_format unsupported SECRET"}}`, code: "upstream_error"},
		{status: 429, body: `{"error":{"message":"SECRET"}}`, code: "upstream_rate_limited", retryable: true, cooldown: time.Second},
		{status: 429, body: `{"error":{"message":"SECRET"}}`, code: "upstream_rate_limited", retryable: true, cooldown: 9 * time.Second, retryAfter: "9"},
	} {
		resp := &http.Response{StatusCode: c.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(c.body))}
		if c.retryAfter != "" {
			resp.Header.Set("Retry-After", c.retryAfter)
		}
		e := upstreamError(resp)
		if e.Code != c.code || strings.Contains(e.Error(), "SECRET") {
			t.Fatal(e)
		}
		if e.Retryable != c.retryable || e.Cooldown != c.cooldown || e.UpstreamStatus != c.status {
			t.Fatalf("status %d retry metadata = %+v", c.status, e)
		}
		if (c.status == 400 || c.status == 422) && e.Status != c.status {
			t.Fatal("client cannot recover from parameter error")
		}
	}
}

func TestRetryAfterIsBounded(t *testing.T) {
	for _, test := range []struct {
		value       string
		want        time.Duration
		wantSeconds int64
		wantValid   bool
	}{
		{value: "999999", want: maximumRetryAfter, wantSeconds: 999999, wantValid: true},
		{value: "999999999999999999999999", want: maximumRetryAfter, wantSeconds: int64(maximumRetryAfter / time.Second), wantValid: true},
		{value: "-1"},
		{value: "not-a-date"},
	} {
		t.Run(test.value, func(t *testing.T) {
			response := httptest.NewRecorder()
			response.Header().Set("Retry-After", test.value)
			got, seconds, valid := retryAfter(&http.Response{Header: response.Header()})
			if got != test.want || seconds != test.wantSeconds || valid != test.wantValid {
				t.Fatalf("retryAfter(%q) = %s/%d/%t, want %s/%d/%t", test.value, got, seconds, valid, test.want, test.wantSeconds, test.wantValid)
			}
		})
	}
}

func TestRetryAfterHTTPDateAndZeroArePreserved(t *testing.T) {
	future := time.Now().Add(5 * time.Minute).UTC().Format(http.TimeFormat)
	for _, value := range []string{future, "0"} {
		t.Run(value, func(t *testing.T) {
			response := httptest.NewRecorder()
			response.Header().Set("Retry-After", value)
			delay, seconds, valid := retryAfter(&http.Response{Header: response.Header()})
			if !valid {
				t.Fatalf("Retry-After %q was not recognized", value)
			}
			if value == "0" && (delay != 0 || seconds != 0) {
				t.Fatalf("zero Retry-After = %s/%d", delay, seconds)
			}
			if value != "0" && (delay < 4*time.Minute+59*time.Second || delay > 5*time.Minute || seconds < 299 || seconds > 300) {
				t.Fatalf("HTTP-date Retry-After = %s/%d", delay, seconds)
			}
		})
	}
}

func TestConfiguredProviderErrorsRequireExactCodeAndScope(t *testing.T) {
	mappings := []ProviderErrorMapping{
		{UpstreamCode: "payment_required", Category: ProviderErrorInsufficientBalance},
		{UpstreamCode: "key_limited", HTTPStatus: 429, Category: ProviderErrorRateLimited},
		{UpstreamCode: "bad_key", HTTPStatus: 403, Category: ProviderErrorAuthentication},
		{UpstreamCode: "policy_blocked", HTTPStatus: 403, Category: ProviderErrorPolicyDenied},
		{UpstreamCode: "model_forbidden", HTTPStatus: 403, Category: ProviderErrorPermissionDenied},
	}
	tests := []struct {
		name      string
		status    int
		body      string
		want      ProviderErrorCategory
		wantCode  string
		wantHTTP  int
		wantRetry bool
	}{
		{name: "mapped billing code", status: 403, body: `{"error":{"code":"payment_required","message":"SECRET billing text"}}`, want: ProviderErrorInsufficientBalance, wantCode: "payment_required", wantHTTP: 403},
		{name: "status-qualified rate limit", status: 429, body: `{"error":{"code":"key_limited"}}`, want: ProviderErrorRateLimited, wantCode: "key_limited", wantHTTP: 429, wantRetry: true},
		{name: "mapped authentication retains key retry", status: 403, body: `{"error":{"code":"bad_key"}}`, want: ProviderErrorAuthentication, wantCode: "bad_key", wantHTTP: 403, wantRetry: true},
		{name: "policy rejection is not balance exhaustion", status: 403, body: `{"error":{"code":"policy_blocked","message":"SECRET policy details"}}`, want: ProviderErrorPolicyDenied, wantCode: "policy_blocked", wantHTTP: 403},
		{name: "model permission is not balance exhaustion", status: 403, body: `{"error":{"code":"model_forbidden"}}`, want: ProviderErrorPermissionDenied, wantCode: "model_forbidden", wantHTTP: 403},
		{name: "same code at a different status remains generic", status: 403, body: `{"error":{"code":"key_limited"}}`, wantHTTP: 502, wantRetry: true},
		{name: "unmapped message does not imply balance", status: 403, body: `{"error":{"message":"insufficient balance"}}`, wantHTTP: 502, wantRetry: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}
			e := upstreamError(resp, mappings...)
			if e.Category != test.want || e.ProviderCode != test.wantCode || e.Status != test.wantHTTP || e.Retryable != test.wantRetry {
				t.Fatalf("error=%+v", e)
			}
			if strings.Contains(e.Error(), "SECRET") {
				t.Fatal("provider message leaked through error")
			}
		})
	}
}

func TestProviderErrorStatusSpecificMappingOverridesUnqualifiedRule(t *testing.T) {
	mappings := []ProviderErrorMapping{
		{UpstreamCode: "shared_code", Category: ProviderErrorInsufficientBalance},
		{UpstreamCode: "shared_code", HTTPStatus: 403, Category: ProviderErrorPolicyDenied},
	}
	for _, test := range []struct {
		status   int
		want     ProviderErrorCategory
		wantCode string
	}{
		{status: 403, want: ProviderErrorPolicyDenied, wantCode: "upstream_policy_denied"},
		{status: 402, want: ProviderErrorInsufficientBalance, wantCode: "provider_insufficient_balance"},
		{status: 404, want: ProviderErrorInsufficientBalance, wantCode: "provider_insufficient_balance"},
	} {
		response := &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"shared_code"}}`))}
		callErr := upstreamError(response, mappings...)
		if callErr.Category != test.want || callErr.Code != test.wantCode {
			t.Fatalf("status=%d call error=%+v", test.status, callErr)
		}
	}
}

func TestProviderStreamErrorMappingsUseEmbeddedStatusAndSafeMessages(t *testing.T) {
	mappings := []ProviderErrorMapping{
		{UpstreamCode: "quota_empty", HTTPStatus: 402, Category: ProviderErrorInsufficientBalance},
		{UpstreamCode: "throttled", HTTPStatus: 429, Category: ProviderErrorRateLimited},
		{UpstreamCode: "generic_balance", Category: ProviderErrorInsufficientBalance},
	}
	for _, test := range []struct {
		name     string
		body     string
		want     ProviderErrorCategory
		wantCode string
	}{
		{name: "status-qualified match", body: `{"error":{"code":"quota_empty","status":402,"message":"SECRET"}}`, want: ProviderErrorInsufficientBalance, wantCode: "quota_empty"},
		{name: "different embedded status", body: `{"error":{"code":"throttled","status":403,"message":"SECRET"}}`},
		{name: "provider code only", body: `{"error":{"code":"generic_balance","message":"SECRET"}}`, want: ProviderErrorInsufficientBalance, wantCode: "generic_balance"},
	} {
		t.Run(test.name, func(t *testing.T) {
			callErr := providerStreamError([]byte(test.body), streamErrorContext{response: &http.Response{StatusCode: 200, Header: make(http.Header)}, mappings: mappings})
			if callErr.Category != test.want || callErr.ProviderCode != test.wantCode {
				t.Fatalf("stream error=%+v", callErr)
			}
			if strings.Contains(callErr.Error(), "SECRET") {
				t.Fatal("upstream stream error message leaked")
			}
		})
	}
}
