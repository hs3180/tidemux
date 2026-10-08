package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestProviderCapacityUpdateAndShowWithoutChangingOtherSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	p := gateway.Provider{Protocol: "openai", BaseURL: "https://provider.example/v1", UpstreamKeychain: gateway.KeychainReference{Service: "provider", Account: "local"}, SupportedModels: []string{"custom-model"}}
	c := gateway.Config{ListenAddr: defaultListenAddr, MaxInFlight: 3, MaxActiveSessions: 17, ActiveSessionIdleTimeoutSeconds: 40, LedgerPath: filepath.Join(dir, "ledger.db"), AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"}, Providers: map[string]gateway.Provider{"glm": p, "other": p}}
	if err := writeCommandConfig(path, c, nil); err != nil {
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
	for _, limit := range []string{"5", "0", "4096"} {
		if err := providerUpdate([]string{"glm", "--max-active-sessions", limit, "--config", path}, stdout, stderr); err != nil {
			t.Fatal(err)
		}
		updated, err := gateway.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if updated.MaxActiveSessions != 17 || updated.ActiveSessionIdleTimeoutSeconds != 40 || updated.Providers["other"].MaxActiveSessions != 0 || updated.Providers["glm"].BaseURL != p.BaseURL {
			t.Fatalf("unrelated config changed: %+v", updated)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []string{"-1", "4097"} {
		if err := providerUpdate([]string{"glm", "--max-active-sessions", limit, "--config", path}, stdout, stderr); err == nil {
			t.Fatal("invalid limit accepted")
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(before) {
			t.Fatal("invalid limit changed config")
		}
	}
	if err := providerShow([]string{"glm", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	output, _ := os.ReadFile(stdout.Name())
	if !strings.Contains(string(output), "Max active sessions: 4096") {
		t.Fatalf("cap not inspectable: %s", output)
	}
}
