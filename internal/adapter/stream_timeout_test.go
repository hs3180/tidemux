package adapter

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

type longStreamTransport struct{}

func (longStreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &longStreamBody{ctx: r.Context()}, Request: r}, nil
}

type longStreamBody struct {
	ctx  context.Context
	sent bool
}

func (b *longStreamBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	b.sent = true
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(61 * time.Second):
	}
	data := `data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\ndata: [DONE]\n\n"
	return copy(p, data), nil
}
func (*longStreamBody) Close() error { return nil }

func TestLongStreamDefaultAndExplicitDeadline(t *testing.T) {
	store, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit-60s"}[explicit], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate, _ := limiter.NewConcurrencyGate(1)
				client := &Client{Protocol: "openai", BaseURL: "http://local.test/v1", APIKey: "provider-secret", Upstream: "test", HTTP: &http.Client{Transport: longStreamTransport{}}, Ledger: store, Gate: gate}
				if explicit {
					client.Limits.UpstreamTimeoutSeconds = 60
				}
				body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":true}`)
				terminal, _, err := client.CallStream(context.Background(), body, "test-model", func(string, []byte) error { return nil })
				if explicit {
					if err == nil || err.Error() != "upstream_timeout" || terminal != nil {
						t.Fatalf("explicit deadline: terminal=%s error=%v", terminal, err)
					}
				} else if err != nil || !strings.Contains(string(terminal), "[DONE]") {
					t.Fatalf("61-second stream: terminal=%s error=%v", terminal, err)
				}
			})
		})
	}
}
