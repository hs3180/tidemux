package gateway

import (
	"context"
	"encoding/json"
	"github.com/hs3180/tidemux/internal/ledger"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalRejectionsAreCorrelatedAndSeparate(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "http://127.0.0.1:1")
		c.Protocol = protocol
		h, closeDB, err := NewHandler(c, nil)
		if err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			path, body, auth string
			status           int
		}{
			{endpoint(protocol), `{"secret":"private-content"}`, "Bearer local-secret", 400},
			{"/private-path?key=private-query", "private-body", "Bearer local-secret", 404},
			{endpoint(protocol), "private-body", "Bearer private-token", 401},
			{endpoint(protocol), strings.Repeat("x", 1<<20+1), "Bearer local-secret", 413},
		}
		ids := map[string]bool{}
		for _, tc := range cases {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", tc.auth)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			id := w.Header().Get("X-TideMux-Request-ID")
			if w.Code != tc.status || len(id) != 32 || ids[id] {
				t.Fatalf("status/id: %d %q", w.Code, id)
			}
			ids[id] = true
		}
		closeDB()
		l, err := ledger.Open(c.LedgerPath)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := l.RecentDiagnostics(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		attempts, err := l.Recent(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		if len(rows) != len(cases) || len(attempts) != 0 {
			t.Fatal("local rejection confused with upstream attempt")
		}
		for _, d := range rows {
			if !ids[d.ID] {
				t.Fatal("uncorrelated record")
			}
		}
		data, _ := json.Marshal(rows)
		for _, secret := range []string{"private-", "local-secret", "provider-secret"} {
			if strings.Contains(string(data), secret) {
				t.Fatal("sensitive diagnostic")
			}
		}
	}
}

func TestLocalDiagnosticFailureIsExplicit(t *testing.T) {
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "http://127.0.0.1:1")
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	closeDB()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")))
	if w.Code != 500 || !strings.Contains(w.Body.String(), "local_diagnostic_failed") || w.Header().Get("X-TideMux-Request-ID") == "" {
		t.Fatal(w.Code, w.Body.String())
	}
}
