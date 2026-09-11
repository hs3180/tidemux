package gateway

import (
	"context"
	"github.com/hs3180/tidemux/internal/ledger"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfiguredLimits(t *testing.T) {
	for _, scenario := range []string{"request", "response", "timeout", "stream-timeout"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if strings.Contains(scenario, "timeout") {
					if scenario == "stream-timeout" {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
						w.(http.Flusher).Flush()
					}
					select {
					case <-r.Context().Done():
					case <-time.After(3 * time.Second):
					}
					return
				}
				io.WriteString(w, responseBody("openai"))
			}))
			defer up.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
			switch scenario {
			case "request":
				c.Limits.RequestBytes = 8
			case "response":
				c.Limits.ResponseBytes = 8
			default:
				c.Limits.UpstreamTimeoutSeconds = 1
			}
			h, closeDB, err := NewHandler(c, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			body := requestBody("openai")
			if scenario == "stream-timeout" {
				body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			}
			req := httptest.NewRequest("POST", endpoint("openai"), strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			wantStatus := 502
			wantCode := "upstream_response_too_large"
			if scenario == "request" {
				wantStatus = 413
				wantCode = "request_too_large"
			}
			if scenario == "timeout" {
				wantStatus = 504
				wantCode = "upstream_timeout"
			}
			if scenario == "stream-timeout" {
				wantStatus = 200
				wantCode = "upstream_timeout"
			}
			if w.Code != wantStatus || !strings.Contains(w.Body.String(), wantCode) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			l, err := ledger.Open(c.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			rows, err := l.Recent(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "request" {
				if calls != 0 || len(rows) != 0 {
					t.Fatal("local limit created upstream attempt")
				}
			} else if len(rows) != 1 || rows[0].Status != "error" || rows[0].ErrorCode != wantCode || rows[0].InputTokens != nil || rows[0].EstimatedCost != nil {
				t.Fatalf("incorrect terminal audit: %+v", rows)
			}
		})
	}
}
