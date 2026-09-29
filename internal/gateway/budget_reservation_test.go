package gateway

import (
	"context"
	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocallyRejectedRequestReleasesBudgetReservation(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			calls++
		}
		io.WriteString(w, responseBody("openai"))
	}))
	defer up.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL), "openai-main")
	provider := c.Providers["openai-main"]
	provider.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	provider.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 2, AlertThreshold: .8, Mode: "hard"}
	c.Providers["openai-main"] = provider
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	rejected := `{"model":"openai-main/custom-model","max_tokens":64,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"Reference"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(rejected))
	req.Header.Set("x-api-key", "local-secret")
	req.Header.Set("anthropic-version", "2023-06-01")
	first := httptest.NewRecorder()
	h.ServeHTTP(first, req)
	if first.Code != http.StatusBadRequest || !strings.Contains(first.Body.String(), "unsupported_request_feature") || calls != 0 {
		t.Fatalf("local rejection status=%d upstream_calls=%d body=%s", first.Code, calls, first.Body.String())
	}
	var budgetRows int
	if err := h.(*handler).ledger.QueryRow(context.Background(), `SELECT COUNT(*) FROM budget_charges`).Scan(&budgetRows); err != nil {
		t.Fatal(err)
	}
	if budgetRows != 0 {
		t.Fatalf("local rejection left budget charge rows=%d", budgetRows)
	}

	valid := `{"model":"openai-main/custom-model","max_tokens":16,"messages":[{"role":"user","content":"continue"}]}`
	next := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(valid))
	next.Header.Set("x-api-key", "local-secret")
	next.Header.Set("anthropic-version", "2023-06-01")
	second := httptest.NewRecorder()
	h.ServeHTTP(second, next)
	if second.Code != http.StatusOK || calls != 1 {
		t.Fatalf("follow-up status=%d upstream_calls=%d body=%s", second.Code, calls, second.Body.String())
	}
}
