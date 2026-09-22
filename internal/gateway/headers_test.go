package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderAnthropicHeadersDoNotUseClientHeaders(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("anthropic-beta") != "" {
			t.Error("client-only Anthropic beta header was forwarded")
		}
		if r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Private") != "" {
			t.Error("wrong credential/header boundary")
		}
		io.WriteString(w, responseBody("anthropic"))
	}))
	defer up.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), up.URL)
	c.Protocol = "anthropic"
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(requestBody("openai")))
	r.Header.Set("Authorization", "Bearer local-secret")
	r.Header.Set("X-Private", "do-not-forward")
	r.Header.Set("anthropic-version", "2099-01-01")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || calls != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}
