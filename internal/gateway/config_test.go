package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testSecrets map[string]string

func (s testSecrets) Lookup(_ context.Context, ref KeychainReference) (string, error) {
	return s[ref.Service+"/"+ref.Account], nil
}

func TestLoadConfigRejectsLegacyPlaintextSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen_addr":"127.0.0.1:8787","deepseek_api_key":"do-not-allow"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("plaintext key was not rejected: %v", err)
	}
}

func TestConfigUsesOnlyKeychainReferencesOnDisk(t *testing.T) {
	config := Config{
		ListenAddr:          "127.0.0.1:8080",
		DeepSeekKeychain:    KeychainReference{Service: "com.example.tidemux.deepseek", Account: "default"},
		AccessTokenKeychain: KeychainReference{Service: "com.example.tidemux.gateway", Account: "default"},
		MaxInFlight:         1, LedgerPath: "ledger.db",
	}
	resolved, err := config.ResolveCredentials(context.Background(), testSecrets{
		"com.example.tidemux.deepseek/default": "provider-secret",
		"com.example.tidemux.gateway/default":  "gateway-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.DeepSeekAPIKey != "provider-secret" || resolved.AccessToken != "gateway-secret" {
		t.Fatal("credentials not resolved")
	}
	encoded := string(mustJSON(t, resolved))
	if strings.Contains(encoded, "provider-secret") || strings.Contains(encoded, "gateway-secret") || strings.Contains(encoded, "deepseek_api_key") {
		t.Fatalf("secret persisted in JSON: %s", encoded)
	}
}

func mustJSON(t *testing.T, value Config) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
