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
		{status: 429, body: `{"error":{"message":"SECRET"}}`, code: "upstream_rate_limited", retryable: true, cooldown: time.Minute},
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
		value string
		want  time.Duration
	}{
		{value: "999999", want: maximumKeyCooldown},
		{value: "-1", want: time.Second},
		{value: "not-a-date", want: defaultRateLimitCooldown},
	} {
		t.Run(test.value, func(t *testing.T) {
			response := httptest.NewRecorder()
			response.Header().Set("Retry-After", test.value)
			got := retryAfter(&http.Response{Header: response.Header()})
			if got != test.want {
				t.Fatalf("retryAfter(%q) = %s, want %s", test.value, got, test.want)
			}
		})
	}
}
