package adapter

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"testing"
)

func TestZaiMappingsAreExplicitForHTTPAndSSE(t *testing.T) {
	for _, mapping := range []ProviderErrorMapping{
		{UpstreamCode: "1308", HTTPStatus: 429, Category: ProviderErrorQuotaExhausted},
		{UpstreamCode: "1310", HTTPStatus: 429, Category: ProviderErrorQuotaExhausted},
		{UpstreamCode: "1311", HTTPStatus: 429, Category: ProviderErrorAccessDenied},
		{UpstreamCode: "1313", HTTPStatus: 429, Category: ProviderErrorFairUseThrottled},
	} {
		for _, numeric := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, configured := range []bool{false, true} {
					code := fmt.Sprintf("%q", mapping.UpstreamCode)
					if numeric {
						code = mapping.UpstreamCode
					}
					body := []byte(fmt.Sprintf(`{"error":{"code":%s,"status":429}}`, code))
					response := &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
					var mappings []ProviderErrorMapping
					if configured {
						mappings = []ProviderErrorMapping{mapping}
					}
					var err *CallError
					if stream {
						response.StatusCode = 200
						err = providerStreamError(body, streamErrorContext{response: response, mappings: mappings})
					} else {
						err = upstreamError(response, mappings...)
					}
					want := ProviderErrorRateLimited
					if configured {
						want = mapping.Category
					}
					if err.Category != want {
						t.Fatalf("code=%s numeric=%t stream=%t configured=%t: got %s want %s", code, numeric, stream, configured, err.Category, want)
					}
					if configured && failureRetryability(err) != "terminal" || !configured && failureRetryability(err) != "retryable" {
						t.Fatal("terminal/transient policy was lost")
					}
				}
			}
		}
	}
}
