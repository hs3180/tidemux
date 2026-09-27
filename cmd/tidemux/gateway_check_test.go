package main

import (
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

func TestGatewayCheckUsesLoopbackCredentialWithoutChangingConfig(t *testing.T) {
	const secret = "local-gateway-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("unexpected check request %s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer server.Close()
	addr := server.Listener.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := gateway.Config{
		ListenAddr:          "0.0.0.0:" + port,
		MaxInFlight:         1,
		LedgerPath:          filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"},
	}
	if err := writeCommandConfig(path, config, nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := os.CreateTemp(dir, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(dir, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	secrets := &memorySecrets{values: map[string]string{"gateway/local": secret}}
	if err := gatewayCheck([]string{"--config", path}, stdout, stderr, secrets); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("check modified the config: err=%v", err)
	}
	output, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "authenticated /v1/models at http://127.0.0.1:"+port) || strings.Contains(string(output), secret) {
		t.Fatalf("output=%q", output)
	}
}

func TestGatewayCheckReportsUnreachableGatewayWithoutCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := gateway.Config{
		ListenAddr:          "127.0.0.1:1",
		MaxInFlight:         1,
		LedgerPath:          filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"},
	}
	if err := writeCommandConfig(path, config, nil); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.CreateTemp(dir, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(dir, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	secrets := &memorySecrets{values: map[string]string{"gateway/local": "local-gateway-secret"}}
	err = gatewayCheck([]string{"--config", path}, stdout, stderr, secrets)
	if err == nil || !strings.Contains(err.Error(), "gateway is not reachable") || strings.Contains(err.Error(), "local-gateway-secret") {
		t.Fatalf("err=%v", err)
	}
}
