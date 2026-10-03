package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestGeneratedSessionIDsDoNotRetainPrompts(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, capacity := range []int{0, 100} {
			t.Run(fmt.Sprintf("auto=%t/capacity=%d", automatic, capacity), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						io.WriteString(w, `{"object":"list","data":[{"id":"custom-model"}]}`)
						return
					}
					io.WriteString(w, responseBody("openai"))
				}))
				defer upstream.Close()
				c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL), "mock")
				c.MaxActiveSessions = capacity
				model := "mock/custom-model"
				if automatic {
					c.AutoChain = []AutoChainEntry{{Provider: "mock", Model: "custom-model"}}
					model = "auto"
				}
				h, closeGateway, err := NewHandler(c, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer closeGateway()
				body := `{"model":"` + model + `","messages":[{"role":"user","content":"synthetic"}]}`
				for i := 0; i < 50; i++ {
					if response := affinityGatewayRequest(h, "openai", "", body); response.Code != 200 {
						t.Fatalf("anonymous status=%d", response.Code)
					}
				}
				cache := h.(*handler).clients["mock"].PromptCache
				if entries, size := cache.Stats(); entries != 0 || size != 0 {
					t.Fatalf("one-shot IDs retained %d entries/%d bytes", entries, size)
				}
				if response := affinityGatewayRequest(h, "openai", "stable-session", body); response.Code != 200 {
					t.Fatalf("stable status=%d", response.Code)
				}
				if entries, _ := cache.Stats(); entries != 1 {
					t.Fatalf("stable identity not remembered: %d", entries)
				}
			})
		}
	}
}
