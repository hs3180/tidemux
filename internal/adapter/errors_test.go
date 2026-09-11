package adapter

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSafeUpstreamParameterErrors(t *testing.T) {
	for _, c := range []struct {
		status     int
		body, code string
	}{
		{400, `{"error":{"message":"response_format is not supported. SECRET prompt detail"}}`, "unsupported_parameter_response_format"},
		{422, `{"error":{"message":"Extra inputs are not permitted for output_config. SECRET"}}`, "unsupported_parameter_output_config"},
		{400, `{"error":{"message":"private account SECRET"}}`, "upstream_invalid_request"},
		{401, `{"error":{"message":"SECRET key rejected"}}`, "upstream_error"},
		{500, `{"error":{"message":"response_format unsupported SECRET"}}`, "upstream_error"},
		{429, `{"error":{"message":"SECRET"}}`, "upstream_rate_limited"},
	} {
		e := upstreamError(&http.Response{StatusCode: c.status, Body: io.NopCloser(strings.NewReader(c.body))})
		if e.Code != c.code || strings.Contains(e.Error(), "SECRET") {
			t.Fatal(e)
		}
		if (c.status == 400 || c.status == 422) && e.Status != c.status {
			t.Fatal("client cannot recover from parameter error")
		}
	}
}
