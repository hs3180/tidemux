package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestGatewayAvailabilityAuthenticatedReadOnly(t *testing.T) {
	const secret = "private-gateway-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tidemux/availability-status" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("unexpected availability request")
		}
		io.WriteString(w, `{"timestamp":"2026-10-10T00:00:00Z","recovery_mode":"request_driven","state_limit":4096,"idle_ttl_seconds":86400,"alternatives_scope":"model_health","providers":[{"ref":"a","generation":2,"state":"probing","scope":"provider","models":[{"model":"one","state":"cooling","scope":"model","reason":"model_not_found","retry_at":"2026-10-10T00:00:36Z","retry_after_seconds":0,"alternatives":["b/one"],"probe_due":true}]}]}`)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{ListenAddr: "0.0.0.0:" + port, MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"), AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"}}
	if err := writeCommandConfig(path, c, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for _, mode := range [][]string{{}, {"--json"}} {
		out, _ := os.CreateTemp(dir, "out")
		defer out.Close()
		errout, _ := os.CreateTemp(dir, "err")
		defer errout.Close()
		args := append([]string{"--config", path}, mode...)
		if err := gatewayAvailability(args, out, errout, &memorySecrets{values: map[string]string{"gateway/local": secret}}); err != nil {
			t.Fatal(err)
		}
		output, _ := os.ReadFile(out.Name())
		if !strings.Contains(string(output), "model_not_found") || strings.Contains(string(output), secret) || strings.Contains(string(output), path) {
			t.Fatal("unsafe or incomplete output", string(output))
		}
		if len(mode) == 0 {
			for _, want := range []string{"scope=model", "wait=0s", "wait=unknown", "alternatives: b/one"} {
				if !strings.Contains(string(output), want) {
					t.Fatalf("missing %s in availability output", want)
				}
			}
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("query modified configuration")
	}
}

func TestGatewayAvailabilityRejectsRedirectAndUnsafeAddress(t *testing.T) {
	var leaked bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer redirect.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	out, _ := os.CreateTemp(dir, "out")
	defer out.Close()
	errout, _ := os.CreateTemp(dir, "err")
	defer errout.Close()
	for _, address := range []string{redirect.Listener.Addr().String(), "192.0.2.1:8080"} {
		c := gateway.Config{ListenAddr: address, MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"), AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"}}
		raw, _ := json.Marshal(c)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		err := gatewayAvailability([]string{"--config", path}, out, errout, &memorySecrets{values: map[string]string{"gateway/local": "private-key"}})
		if err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatal("unsafe query error", err)
		}
	}
	if leaked {
		t.Fatal("credential followed redirect")
	}
}
