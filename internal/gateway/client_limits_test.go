package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnthropicImageRequestAboveOneMiB(t *testing.T) {
	image := strings.Repeat("a", 1200<<10)
	body := fmt.Sprintf(`{"model":"legacy/custom-model","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}`, image)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		if !bytes.Contains(data, []byte(image)) {
			t.Error("image data was not preserved")
		}
		io.WriteString(w, responseBody("anthropic"))
	}))
	defer upstream.Close()
	c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL)
	c.Protocol = "anthropic"
	h, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "local-secret")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusOK || calls != 1 {
		t.Fatalf("image request: status=%d calls=%d body=%s", out.Code, calls, out.Body)
	}
}

func TestRequestSizeBoundaryAndActionableError(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%v", protocol, explicit), func(t *testing.T) {
				limit := int64(32 << 20)
				if explicit {
					limit = 2048
				}
				calls := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					io.Copy(io.Discard, r.Body)
					io.WriteString(w, responseBody(protocol))
				}))
				defer upstream.Close()
				c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL)
				c.Protocol = protocol
				if explicit {
					c.Limits.RequestBytes = limit
				}
				h, closeDB, err := NewHandler(c, upstream.Client())
				if err != nil {
					t.Fatal(err)
				}
				defer closeDB()
				for _, extra := range []int64{0, 1} {
					body := requestBody(protocol)
					body = strings.Replace(body, "hello", strings.Repeat("a", int(limit+extra)-len(body)+5), 1)
					req := httptest.NewRequest(http.MethodPost, clientEndpoint(protocol), strings.NewReader(body))
					req.Header.Set("Authorization", "Bearer local-secret")
					out := httptest.NewRecorder()
					h.ServeHTTP(out, req)
					if extra == 0 {
						if out.Code != http.StatusOK || calls != 1 {
							t.Fatalf("request at limit: %d calls=%d", out.Code, calls)
						}
						continue
					}
					var payload struct {
						Error map[string]any `json:"error"`
					}
					if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
						t.Fatal(err)
					}
					if out.Code != http.StatusRequestEntityTooLarge || calls != 1 || payload.Error["code"] != "request_too_large" || payload.Error["limit_bytes"] != float64(limit) || !strings.Contains(fmt.Sprint(payload.Error["message"]), "limits.request_bytes") {
						t.Fatalf("request above limit: %d calls=%d %s", out.Code, calls, out.Body)
					}
				}
			})
		}
	}
}

func TestSessionCapacityGuidanceForBothProtocols(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				io.WriteString(w, responseBody(protocol))
			}))
			defer upstream.Close()
			c := testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL)
			c.Protocol, c.MaxActiveSessions = protocol, 1
			h, closeDB, err := NewHandler(c, upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			for _, session := range []string{"private-session-a", "private-session-b"} {
				req := httptest.NewRequest(http.MethodPost, clientEndpoint(protocol), strings.NewReader(requestBody(protocol)))
				req.Header.Set("Authorization", "Bearer local-secret")
				req.Header.Set("X-TideMux-Session-ID", session)
				out := httptest.NewRecorder()
				h.ServeHTTP(out, req)
				if session == "private-session-a" {
					if out.Code != http.StatusOK {
						t.Fatalf("first session: %d %s", out.Code, out.Body)
					}
					continue
				}
				var payload struct {
					Error map[string]any `json:"error"`
				}
				if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				retry, ok := payload.Error["retry"].(map[string]any)
				if out.Code != http.StatusTooManyRequests || calls != 1 || out.Header().Get("Retry-After") != "" || payload.Error["code"] != "active_session_limit" || payload.Error["scope"] != "gateway" || payload.Error["limit"] != float64(1) || !ok || retry["strategy"] != "exponential_backoff_with_jitter" || retry["max_delay_seconds"] != float64(30) {
					t.Fatalf("capacity guidance: %d calls=%d headers=%v body=%s", out.Code, calls, out.Header(), out.Body)
				}
				if strings.Contains(out.Body.String(), "private-session") {
					t.Fatal("session ID leaked")
				}
			}
		})
	}
}

func clientEndpoint(protocol string) string {
	if protocol == "anthropic" {
		return "/v1/messages"
	}
	return "/v1/chat/completions"
}
