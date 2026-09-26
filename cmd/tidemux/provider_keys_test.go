package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestAppendProviderKeyMigratesSingleReferenceAndKeepsSecretsOutOfConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	providerRef := gateway.KeychainReference{Service: "provider", Account: "first"}
	accessRef := gateway.KeychainReference{Service: "gateway", Account: "local"}
	store := &memorySecrets{values: map[string]string{
		"provider/first": "provider-secret-one",
		"gateway/local":  "gateway-secret",
	}}
	c := gateway.Config{
		ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: accessRef,
		Providers: map[string]gateway.Provider{
			"main": {
				Protocol: "openai", BaseURL: "https://provider.example/v1", Model: "model-a", UpstreamID: "main",
				UpstreamKeychain: providerRef,
			},
		},
		DefaultProviders: map[string]string{"openai": "main"},
	}
	if err := writeCommandConfig(path, c, nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendProviderKey(path, c, before, "main", "provider-secret-two", store, store); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := loaded.Providers["main"]
	if provider.UpstreamKeychain != (gateway.KeychainReference{}) || len(provider.UpstreamKeychains) != 2 {
		t.Fatalf("key references were not migrated to the group field: %+v", provider)
	}
	resolved, err := loaded.ResolveCredentials(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	keys := resolved.Providers["main"].ResolvedAPIKeys()
	if len(keys) != 2 || keys[0] != "provider-secret-one" || keys[1] != "provider-secret-two" {
		t.Fatalf("resolved group keys = %#v", keys)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"provider-secret-one", "provider-secret-two", "gateway-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("config leaked secret %q", secret)
		}
	}

	duplicateBefore := append([]byte(nil), data...)
	if err := appendProviderKey(path, loaded, data, "main", "provider-secret-one", store, store); err == nil {
		t.Fatal("duplicate key was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(duplicateBefore) {
		t.Fatalf("duplicate-key rejection changed config: err=%v", err)
	}
}

func TestProviderKeyListRedactsKeychainDetails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{
		ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"},
		Providers: map[string]gateway.Provider{
			"main": {
				Protocol: "openai", BaseURL: "https://provider.example/v1", Model: "model-a", UpstreamID: "main",
				UpstreamKeychains: []gateway.KeychainReference{
					{Service: "private-service", Account: "private-account-one"},
					{Service: "private-service", Account: "private-account-two"},
				},
			},
		},
		DefaultProviders: map[string]string{"openai": "main"},
	}
	if err := writeCommandConfig(path, c, nil); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(dir, "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err := providerKeyList([]string{"main", "--config", path}, output, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "1    stored in Keychain") || !strings.Contains(text, "2    stored in Keychain") {
		t.Fatalf("key slots missing: %q", text)
	}
	if strings.Contains(text, "private-service") || strings.Contains(text, "private-account") {
		t.Fatalf("Keychain details were not redacted: %q", text)
	}
}

func TestRemoveProviderKeyPreservesOneKeyAndDeletesOnlyItsReference(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	first := gateway.KeychainReference{Service: "provider", Account: "first"}
	second := gateway.KeychainReference{Service: "provider", Account: "second"}
	access := gateway.KeychainReference{Service: "gateway", Account: "local"}
	store := &memorySecrets{values: map[string]string{
		"provider/first":  "provider-secret-one",
		"provider/second": "provider-secret-two",
		"gateway/local":   "gateway-secret",
	}}
	c := gateway.Config{
		ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: access,
		Providers: map[string]gateway.Provider{
			"main": {
				Protocol: "openai", BaseURL: "https://provider.example/v1", Model: "model-a", UpstreamID: "main",
				UpstreamKeychains: []gateway.KeychainReference{first, second},
			},
		},
		DefaultProviders: map[string]string{"openai": "main"},
	}
	if err := writeCommandConfig(path, c, nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeProviderKey(path, c, before, "main", 1, store); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := loaded.Providers["main"].KeychainReferences()
	if err != nil || len(refs) != 1 || refs[0] != second {
		t.Fatalf("remaining key refs=%v err=%v", refs, err)
	}
	if _, err := store.Lookup(context.Background(), first); err == nil {
		t.Fatal("removed Keychain item still exists")
	}
	if _, err := store.Lookup(context.Background(), second); err != nil {
		t.Fatal("remaining Keychain item was deleted")
	}
	if err := removeProviderKey(path, loaded, mustReadConfig(t, path), "main", 1, store); err == nil || !strings.Contains(err.Error(), "retain at least one") {
		t.Fatalf("last key removal error=%v", err)
	}
}

func mustReadConfig(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
