package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/gateway"
)

func TestProviderErrorMapAddListRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{
		ListenAddr:          defaultListenAddr,
		MaxInFlight:         1,
		LedgerPath:          filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"},
		Providers: map[string]gateway.Provider{
			"p": {Protocol: "openai", BaseURL: "https://provider.example/v1", UpstreamID: "p", UpstreamKeychain: gateway.KeychainReference{Service: "provider", Account: "p"}},
		},
	}
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
	readOutput := func() string {
		t.Helper()
		if _, err := stdout.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(stdout.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	// Support both the common REF-before-flags form and flags before REF.
	if err := providerErrorMapCommand([]string{"add", "p", "--code", "balance_low", "--status", "402", "--category", "insufficient_balance", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []adapter.ProviderErrorMapping{{UpstreamCode: "balance_low", HTTPStatus: 402, Category: adapter.ProviderErrorInsufficientBalance}}
	if len(loaded.Providers["p"].ErrorCodeMappings) != 1 || loaded.Providers["p"].ErrorCodeMappings[0] != want[0] {
		t.Fatalf("mappings=%+v", loaded.Providers["p"].ErrorCodeMappings)
	}
	if err := providerErrorMapCommand([]string{"add", "--code", "policy_blocked", "--status", "403", "--category", "policy_denied", "p", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err = gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if mappings := loaded.Providers["p"].ErrorCodeMappings; len(mappings) != 2 || mappings[1] != (adapter.ProviderErrorMapping{UpstreamCode: "policy_blocked", HTTPStatus: 403, Category: adapter.ProviderErrorPolicyDenied}) {
		t.Fatalf("policy mapping was not persisted: %+v", mappings)
	}
	if err := providerErrorMapCommand([]string{"list", "--config", path, "p"}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	if output := readOutput(); !strings.Contains(output, "balance_low\t402\tinsufficient_balance") || !strings.Contains(output, "policy_blocked\t403\tpolicy_denied") {
		t.Fatalf("list output=%q", readOutput())
	}
	if err := providerErrorMapCommand([]string{"remove", "--code", "balance_low", "--status", "402", "--config", path, "p"}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	if err := providerErrorMapCommand([]string{"remove", "--code", "policy_blocked", "--status", "403", "--config", path, "p"}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	loaded, err = gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers["p"].ErrorCodeMappings) != 0 {
		t.Fatalf("remove left mappings=%+v", loaded.Providers["p"].ErrorCodeMappings)
	}
}

func TestProviderErrorMapRejectsInvalidAndDuplicateMappingsWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{
		ListenAddr:          defaultListenAddr,
		MaxInFlight:         1,
		LedgerPath:          filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"},
		Providers: map[string]gateway.Provider{
			"p": {Protocol: "openai", BaseURL: "https://provider.example/v1", UpstreamID: "p", UpstreamKeychain: gateway.KeychainReference{Service: "provider", Account: "p"}, ErrorCodeMappings: []adapter.ProviderErrorMapping{{UpstreamCode: "balance_low", Category: adapter.ProviderErrorInsufficientBalance}}},
		},
	}
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
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "p", "--code", "balance_low", "--category", "rate_limited", "--config", path},
		{"add", "p", "--code", "secret/message", "--category", "invalid_request", "--config", path},
	} {
		if err := providerErrorMapCommand(args, stdout, stderr); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("failed command changed config: err=%v", err)
		}
	}
}
