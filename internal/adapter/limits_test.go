package adapter

import (
	"strings"
	"testing"
)

func TestLimitsDefaultsAndValidation(t *testing.T) {
	got := (Limits{}).Effective()
	if got.UpstreamTimeoutSeconds != 60 || got.RequestBytes != 1<<20 || got.ResponseBytes != 8<<20 || got.StreamBytes != 64<<20 || got.EventBytes != 1<<20 {
		t.Fatal("legacy defaults changed")
	}
	for _, bad := range []Limits{{RequestBytes: -1}, {ResponseBytes: 1<<30 + 1}, {UpstreamTimeoutSeconds: 3601}, {EventBytes: 100, StreamBytes: 50}} {
		if bad.Validate() == nil {
			t.Fatal("invalid limits accepted")
		}
	}
}

func TestStreamLimitsDoNotReleaseTerminal(t *testing.T) {
	for _, limits := range []Limits{{StreamBytes: 120, EventBytes: 100}, {StreamBytes: 1000, EventBytes: 30}} {
		emitted := ""
		body := strings.Repeat(": heartbeat\n\n", 12) + "data: [DONE]\n\n"
		if limits.EventBytes == 30 {
			body = ": " + strings.Repeat("x", 60) + "\n\n"
		}
		usage, terminal, err := readStreamWithLimits("openai", strings.NewReader(body), limits, func(p []byte) error { emitted += string(p); return nil })
		if err == nil || terminal != nil || usage.Input != nil || strings.Contains(emitted, "[DONE]") {
			t.Fatal("limit produced a successful stream")
		}
	}
}
