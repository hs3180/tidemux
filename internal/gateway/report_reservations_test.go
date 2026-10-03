package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
)

func TestReportOpenAndQueuedCancelDoesNotLatchBudgetBlock(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
			return
		}
		io.WriteString(w, responseBody("openai"))
	}))
	defer upstream.Close()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL), "mock")
	provider := c.Providers["mock"]
	provider.Prices = map[string]adapter.Price{"custom-model": testPrice()}
	policy := ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 2, AlertThreshold: .8, Mode: "hard"}
	provider.Budget = &policy
	c.Providers["mock"] = provider
	h, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	actual := h.(*handler)
	gate := actual.clients["mock"].Gate
	if _, err := gate.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	out := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBodyFor("openai", "mock", "custom-model"))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer local-secret")
	go func() {
		h.ServeHTTP(out, req)
		close(done)
	}()
	defer func() {
		cancel()
		gate.Release()
		<-done
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var pending int
		if err := actual.ledger.QueryRow(context.Background(), `SELECT COUNT(*) FROM budget_charges WHERE state='pending'`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not reach its budget reservation")
		}
		time.Sleep(time.Millisecond)
	}
	// This is the exact ledger-open path used by report list/generate/notify.
	reportStore, err := ledger.Open(c.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reportStore.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued cancellation did not complete")
	}
	var remaining int
	if err := actual.ledger.QueryRow(context.Background(), `SELECT COUNT(*) FROM budget_charges`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("unattempted cancellation left %d budget rows", remaining)
	}
	// Isolate the permanent memory latch from the rolling-window DB safeguard.
	future := time.Now().Add(8 * 24 * time.Hour)
	_, admissionErr := actual.ledger.CheckBudget(context.Background(), "future-request", "mock", policy, false, future)
	if admissionErr == nil {
		if err := actual.ledger.ReleaseBudgetReservation(context.Background(), "future-request"); err != nil {
			t.Fatal(err)
		}
	}
	if actual.isBudgetBlocked("mock", "USD") {
		t.Fatalf("unattempted queued cancellation returned %d; provider remains blocked in memory even though admission after 8 days returns %v", out.Code, admissionErr)
	}
}
