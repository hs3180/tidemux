package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

type memorySecrets struct {
	values        map[string]string
	fail          bool
	failDelete    bool
	failDeleteFor string
}

func (m *memorySecrets) StoreNew(_ context.Context, ref gateway.KeychainReference, value string) error {
	if m.fail {
		return errors.New("simulated keychain failure")
	}
	if m.values == nil {
		m.values = map[string]string{}
	}
	key := ref.Service + "/" + ref.Account
	if _, exists := m.values[key]; exists {
		return errors.New("duplicate keychain item")
	}
	m.values[key] = value
	return nil
}
func (m *memorySecrets) Lookup(_ context.Context, ref gateway.KeychainReference) (string, error) {
	value, ok := m.values[ref.Service+"/"+ref.Account]
	if !ok {
		return "", errors.New("missing keychain item")
	}
	return value, nil
}
func (m *memorySecrets) Delete(_ context.Context, ref gateway.KeychainReference) error {
	key := ref.Service + "/" + ref.Account
	if m.failDelete || m.failDeleteFor == key {
		return errors.New("simulated Keychain deletion failure")
	}
	delete(m.values, key)
	return nil
}

func TestDeleteUnreferencedProviderKeys(t *testing.T) {
	target := gateway.KeychainReference{Service: "provider", Account: "old"}
	other := gateway.KeychainReference{Service: "provider", Account: "other"}
	secret := "sensitive-provider-secret"

	t.Run("preserves references used by another provider", func(t *testing.T) {
		store := &memorySecrets{values: map[string]string{"provider/old": secret}}
		c := gateway.Config{Providers: map[string]gateway.Provider{
			"still-using-it": {UpstreamKeychain: target},
		}}
		if err := deleteUnreferencedProviderKeys(c, []gateway.KeychainReference{target}, store); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Lookup(context.Background(), target); err != nil {
			t.Fatal("shared Keychain item was deleted")
		}
	})

	t.Run("deletes references no longer used", func(t *testing.T) {
		store := &memorySecrets{values: map[string]string{"provider/old": secret}}
		c := gateway.Config{Providers: map[string]gateway.Provider{
			"different-key": {UpstreamKeychain: other},
		}}
		if err := deleteUnreferencedProviderKeys(c, []gateway.KeychainReference{target}, store); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Lookup(context.Background(), target); err == nil {
			t.Fatal("unreferenced Keychain item was not deleted")
		}
	})

	t.Run("reports deletion failures without exposing the key", func(t *testing.T) {
		store := &memorySecrets{values: map[string]string{"provider/old": secret}, failDelete: true}
		err := deleteUnreferencedProviderKeys(gateway.Config{}, []gateway.KeychainReference{target}, store)
		if err == nil || !strings.Contains(err.Error(), "could not be deleted from Keychain") {
			t.Fatalf("expected explicit cleanup error, got %v", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("cleanup error exposed the API key")
		}
		if _, lookupErr := store.Lookup(context.Background(), target); lookupErr != nil {
			t.Fatal("failed deletion unexpectedly removed the Keychain item")
		}
	})

	t.Run("attempts all deletions when one Keychain operation fails", func(t *testing.T) {
		store := &memorySecrets{
			values:        map[string]string{"provider/old": secret, "provider/other": "another-secret"},
			failDeleteFor: "provider/old",
		}
		err := deleteUnreferencedProviderKeys(gateway.Config{}, []gateway.KeychainReference{target, other}, store)
		if err == nil {
			t.Fatal("expected an explicit cleanup error")
		}
		if _, lookupErr := store.Lookup(context.Background(), target); lookupErr != nil {
			t.Fatal("failed Keychain deletion unexpectedly removed the item")
		}
		if _, lookupErr := store.Lookup(context.Background(), other); lookupErr == nil {
			t.Fatal("later Keychain deletions were skipped after the first failure")
		}
	})

	t.Run("does not delete when another provider reference is invalid", func(t *testing.T) {
		store := &memorySecrets{values: map[string]string{"provider/old": secret}}
		c := gateway.Config{Providers: map[string]gateway.Provider{
			"uncertain-reference": {UpstreamKeychain: gateway.KeychainReference{Service: "provider"}},
		}}
		err := deleteUnreferencedProviderKeys(c, []gateway.KeychainReference{target}, store)
		if err == nil || !strings.Contains(err.Error(), "references are invalid") {
			t.Fatalf("expected invalid-reference error, got %v", err)
		}
		if _, lookupErr := store.Lookup(context.Background(), target); lookupErr != nil {
			t.Fatal("Keychain item was deleted despite an uncertain reference")
		}
	})
}

func TestProviderAdditionWritesReferencesAndRollsBack(t *testing.T) {
	for _, failValidation := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "validation failure"}[failValidation], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			provider := gateway.Provider{Protocol: "openai", BaseURL: "https://provider.example/v1", Model: "test-model", UpstreamID: "test"}
			if failValidation {
				provider.Model = ""
			}
			c := gateway.Config{ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"), Providers: map[string]gateway.Provider{"test": provider}, DefaultProviders: map[string]string{"openai": "test"}}
			store := &memorySecrets{values: map[string]string{}}
			err := persistProviderAddition(path, c, nil, "test", "provider-secret", store)
			if failValidation {
				if err == nil || len(store.values) != 0 {
					t.Fatalf("expected rollback, err=%v entries=%d", err, len(store.values))
				}
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Fatal("partial config installed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := gateway.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := loaded.ResolveCredentials(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Providers["test"].APIKey != "provider-secret" || resolved.AccessToken == "" || resolved.AccessToken == "provider-secret" {
				t.Fatal("Keychain credentials did not round trip")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "provider-secret") {
				t.Fatal("provider key written to config")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("config permissions: %v %v", info, err)
			}
		})
	}
}

func TestProviderAdditionRejectsGatewayCredentialReuse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	accessRef := gateway.KeychainReference{Service: "gateway", Account: "local"}
	store := &memorySecrets{values: map[string]string{"gateway/local": "shared-secret"}}
	c := gateway.Config{ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"), AccessTokenKeychain: accessRef, Providers: map[string]gateway.Provider{"test": {Protocol: "openai", BaseURL: "https://provider.example/v1", Model: "model-a", UpstreamID: "test"}}, DefaultProviders: map[string]string{"openai": "test"}}
	if err := persistProviderAddition(path, c, nil, "test", "shared-secret", store); err == nil {
		t.Fatal("same provider and gateway secret was accepted")
	}
	if len(store.values) != 1 {
		t.Fatalf("unexpected Keychain mutations: %v", store.values)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("config was written after rejecting duplicate credential")
	}
}

func TestEmptyProviderConfigCanRetainGatewaySettings(t *testing.T) {
	c := gateway.Config{ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: "ledger.db", AccessToken: "local-token", AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gateway.NewHandler(c, nil); err == nil || !strings.Contains(err.Error(), "no upstream provider") {
		t.Fatalf("expected clear missing-provider error, got %v", err)
	}
}

func TestProviderNameGenerationDoesNotRequireUserLabel(t *testing.T) {
	providers := map[string]gateway.Provider{"api.example.com": {}}
	if got := uniqueProviderName(providers, "api.example.com"); got != "api.example.com-2" {
		t.Fatalf("generated ref=%q", got)
	}
	if !validProviderLabel("deepseek-2") || validProviderLabel("bad/name") {
		t.Fatal("provider label validation mismatch")
	}
}

func TestLegacyProviderMigrationPreservesGatewaySettings(t *testing.T) {
	c := gateway.Config{ListenAddr: "0.0.0.0:4000", Protocol: "openai", BaseURL: "https://provider.example/v1", Model: "model-a", UpstreamID: "primary", UpstreamKeychain: gateway.KeychainReference{Service: "provider", Account: "old"}, AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"}, MaxInFlight: 5, MaxActiveSessions: 12, ActiveSessionIdleTimeoutSeconds: 120, LedgerPath: "ledger.db"}
	got, err := migrateLegacyProvider(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "" || got.MaxInFlight != 5 || got.MaxActiveSessions != 12 || got.ActiveSessionIdleTimeoutSeconds != 120 || got.LedgerPath != c.LedgerPath {
		t.Fatalf("migration lost settings: %+v", got)
	}
	if len(got.Providers) != 1 || got.Providers["provider"].UpstreamKeychain != c.UpstreamKeychain || got.DefaultProviders["openai"] != "provider" {
		t.Fatalf("migration lost provider: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProviderModelScopeAndPricingCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"), AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"}, Providers: map[string]gateway.Provider{"p": {Protocol: "openai", BaseURL: "https://provider.example/v1", Model: "model-a", UpstreamID: "p", UpstreamKeychain: gateway.KeychainReference{Service: "provider", Account: "p"}}}, DefaultProviders: map[string]string{"openai": "p"}}
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
	if err := providerModels([]string{"p", "--only", "model-a,model-b", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(loaded.Providers["p"].SupportedModels, ",") != "model-a,model-b" {
		t.Fatalf("allowlist=%v", loaded.Providers["p"].SupportedModels)
	}
	if err := providerPricingSet([]string{"p", "model-a", "--input-cache-hit", "1", "--input-cache-miss", "2", "--output", "3", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err = gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Providers["p"].Prices["model-a"]; !ok {
		t.Fatal("price was not saved")
	}
	if err := providerPricingRemove([]string{"p", "model-a", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err = gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers["p"].Prices) != 0 {
		t.Fatal("price was not removed")
	}
}

func TestGatewayConfigureChangesOnlyGatewaySettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"), AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"}, Providers: map[string]gateway.Provider{"p": {Protocol: "openai", BaseURL: "https://provider.example/v1", Model: "model-a", UpstreamID: "p", UpstreamKeychain: gateway.KeychainReference{Service: "provider", Account: "p"}}}, DefaultProviders: map[string]string{"openai": "p"}}
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
	args := []string{"--config", path, "--listen", "loopback", "--max-in-flight", "4", "--max-active-sessions", "12", "--active-session-idle-timeout-seconds", "180"}
	if err := gatewayConfigure(args, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	updated, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ListenAddr != defaultListenAddr || updated.MaxInFlight != 4 || updated.MaxActiveSessions != 12 || updated.ActiveSessionIdleTimeoutSeconds != 180 {
		t.Fatalf("gateway settings=%+v", updated)
	}
	if updated.Providers["p"].BaseURL != c.Providers["p"].BaseURL || updated.DefaultProviders["openai"] != "p" {
		t.Fatal("gateway configure changed provider settings")
	}
}

func TestProviderCommandDoesNotRetainConfigureAlias(t *testing.T) {
	if err := providerCommand([]string{"configure"}, os.Stdout, os.Stderr); err == nil {
		t.Fatal("provider configure alias was retained")
	}
	if err := run([]string{"configure"}, os.Stdout, os.Stderr); err == nil || !strings.Contains(err.Error(), "commands:") {
		t.Fatalf("top-level configure still routed: %v", err)
	}
	if err := run([]string{"budget"}, os.Stdout, os.Stderr); err == nil || !strings.Contains(err.Error(), "commands:") {
		t.Fatalf("top-level budget still routed: %v", err)
	}
}

func TestLoadCommandConfigAllowsFirstProviderSetup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.json")
	c, abs, before, err := loadCommandConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != defaultListenAddr || c.MaxInFlight != 1 || before != nil || abs != path {
		t.Fatalf("initial config=%+v abs=%s before=%v", c, abs, before)
	}
}
