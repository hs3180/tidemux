package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/diagnostics"
	"github.com/hs3180/tidemux/internal/observability"
)

func TestRetrySignalsMatchRecoveryAndPersistedResponse(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, tc := range []struct {
			name, code, wait, signal  string
			category                  adapter.ProviderErrorCategory
			status, wantStatus, delay int
			explicit, blockPeer       bool
		}{
			{name: "provider wait with alternative", code: "rate_limit_error", wait: "3600", status: 429, wantStatus: 429, signal: "true", delay: 2},
			{name: "key backoff with alternative", code: "rate_limit_error", status: 429, wantStatus: 429, signal: "true", delay: 2},
			{name: "bounded explicit wait", code: "rate_limit_error", wait: "3600", status: 429, wantStatus: 429, explicit: true, signal: "true", delay: 2},
			{name: "all routes cooling", code: "rate_limit_error", wait: "3600", status: 429, wantStatus: 429, blockPeer: true, signal: "true", delay: 30},
			{name: "quota with alternative", code: "1310", category: adapter.ProviderErrorQuotaExhausted, wait: "3600", status: 429, wantStatus: 429, signal: "true", delay: 2},
			{name: "quota stays explicit", code: "1310", category: adapter.ProviderErrorQuotaExhausted, wait: "3600", status: 429, wantStatus: 429, explicit: true, signal: "false"},
			{name: "quota has no alternative", code: "1310", category: adapter.ProviderErrorQuotaExhausted, status: 429, wantStatus: 429, blockPeer: true, signal: "false"},
			{name: "standard quota", code: "insufficient_quota", status: 429, wantStatus: 429, explicit: true, signal: "false"},
			{name: "numeric code needs provider mapping", code: "1310", status: 429, wantStatus: 429, signal: "true", delay: 2},
			{name: "authentication", status: 401, wantStatus: 502, signal: "false"},
			{name: "invalid request", status: 400, wantStatus: 400, signal: "false"},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				var postsA, postsB atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "GET" {
						io.WriteString(w, `{"data":[{"id":"one"},{"id":"two"}]}`)
						return
					}
					if strings.HasPrefix(r.URL.Path, "/b/") {
						postsB.Add(1)
						io.WriteString(w, openAIResponseForModel("one"))
						return
					}
					postsA.Add(1)
					w.Header().Set("x-should-retry", "PRIVATE_HEADER_TEXT")
					w.Header().Set("retry-after-ms", "PRIVATE_HEADER_TEXT")
					if tc.wait != "" {
						w.Header().Set("Retry-After", tc.wait)
					}
					w.WriteHeader(tc.status)
					fmt.Fprintf(w, `{"error":{"code":%q,"message":"Bearer PRIVATE_ERROR_TEXT"}}`, tc.code)
				}))
				defer upstream.Close()
				c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
				for _, ref := range []string{"a", "b"} {
					addRoutingProvider(&c, ref, upstream.URL+"/"+ref, "openai", "one", "two")
				}
				if tc.category != "" {
					p := c.Providers["a"]
					p.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: tc.code, HTTPStatus: tc.status, Category: tc.category}}
					c.Providers["a"] = p
				}
				configureRouting(&c, RoutingConfig{SharedModelStrategy: "random"})
				var logs bytes.Buffer
				raw, closeDB, err := NewHandlerWithLogger(c, upstream.Client(), observability.JSONLogger(&logs))
				if err != nil {
					t.Fatal(err)
				}
				defer closeDB()
				h := raw.(*handler)
				h.randomIndex = func(int) int { return 0 }
				if tc.blockPeer {
					h.keyPools["b"].CooldownAll(time.Hour)
					if tc.category == adapter.ProviderErrorQuotaExhausted {
						now := time.Now()
						h.availability.entries[availabilityTarget{"b", "", h.providerGenerations["b"]}] = &availabilityEntry{reason: "quota_exhausted", failures: 1, retryAt: now.Add(time.Hour), touched: now}
					}
				}
				model := "one"
				if tc.explicit {
					model = "a/one"
				}
				body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`, model)
				response := affinityGatewayRequest(h, protocol, "fixture-session", body)
				if response.Code != tc.wantStatus || postsA.Load() != 1 || postsB.Load() != 0 || response.Header().Get("x-should-retry") != tc.signal {
					t.Fatalf("status=%d headers=%v dispatches=%d/%d", response.Code, response.Header(), postsA.Load(), postsB.Load())
				}
				wantSeconds, wantMS := "", ""
				if tc.delay > 0 {
					wantSeconds, wantMS = strconv.Itoa(tc.delay), strconv.Itoa(tc.delay*1000)
				}
				if response.Header().Get("Retry-After") != wantSeconds || response.Header().Get("retry-after-ms") != wantMS {
					t.Fatalf("client wait was unbounded or inconsistent: %v", response.Header())
				}
				var payload struct {
					Type  string
					Error struct {
						Type    string
						Failure *diagnostics.Failure
					}
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if protocol == "anthropic" && (payload.Type != "error" || tc.status == 429 && payload.Error.Type != "rate_limit_error") {
					t.Fatal("canonical Anthropic error envelope was lost")
				}
				rows, err := h.ledger.Recent(context.Background(), 10)
				if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].Failure, payload.Error.Failure) {
					t.Fatalf("response differs from audit: %+v %v", rows, err)
				}
				f := rows[0].Failure
				if f == nil || f.ResponseRetryHeaders == nil || f.ResponseRetryHeaders.ShouldRetry != tc.signal {
					t.Fatal("audit lost retry response headers")
				}
				encoded, _ := json.Marshal(f)
				var expected map[string]any
				json.Unmarshal(encoded, &expected)
				terminal := findRuntimeEvent(decodeRuntimeEvents(t, logs.Bytes()), "request_terminal", rows[0].ID)
				if terminal == nil || !reflect.DeepEqual(terminal["failure"], expected) {
					t.Fatal("terminal log differs from the audit snapshot")
				}
				if tc.wait != "" {
					pool := h.keyPools["a"].diagnosticStatus(time.Now())
					if pool.ProviderRetryAt.Before(time.Now().Add(3590 * time.Second)) {
						t.Fatal("bounded client wait shortened the provider pause")
					}
				}
				if tc.category == adapter.ProviderErrorQuotaExhausted || tc.code == "insufficient_quota" {
					if f.Category != "quota_exhausted" || f.Retryability != "terminal" {
						t.Fatal("provider quota was classified as transient or billing")
					}
				}
				if strings.Contains(logs.String()+string(encoded)+response.Body.String()+fmt.Sprint(response.Header()), "PRIVATE_") {
					t.Fatal("provider free text or arbitrary headers escaped")
				}
				if tc.signal == "true" && !tc.explicit && !tc.blockPeer {
					next := affinityGatewayRequest(h, protocol, "fixture-session", body)
					if next.Code != 200 || postsA.Load() != 1 || postsB.Load() != 1 {
						t.Fatal("a new request did not recover through the alternative")
					}
				}
				if f.Category == "quota_exhausted" && tc.signal == "false" {
					next := affinityGatewayRequest(h, protocol, "fixture-session", body)
					if next.Code != 429 || next.Header().Get("x-should-retry") != "false" || postsA.Load() != 1 || postsB.Load() != 0 {
						t.Fatal("local quota cooldown invited another automatic attempt")
					}
				}
			})
		}
	}
}
