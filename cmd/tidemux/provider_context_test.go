package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestProviderContextOverridePreservesOtherSettings(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("provider configuration is supported only on macOS")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	provider := gateway.Provider{
		Protocol: "openai", BaseURL: "https://provider.example/v1",
		UpstreamKeychain: gateway.KeychainReference{Service: "provider", Account: "local"},
		SupportedModels:  []string{"model-a", "model-b"}, MaxActiveSessions: 3,
		ModelCapabilities: gateway.ModelCapabilities{ContextTokens: 65536, MaxOutputTokens: 8192},
	}
	c := gateway.Config{
		ListenAddr: defaultListenAddr, MaxInFlight: 4, MaxActiveSessions: 17,
		LedgerPath:          filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"},
		Providers:           map[string]gateway.Provider{"glm": provider, "other": provider},
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

	for _, limit := range []int64{1000000, 1000000000, 0, 1000000} {
		want, err := gateway.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		changed := want.Providers["glm"]
		changed.ModelCapabilities.ContextTokens = limit
		want.Providers["glm"] = changed
		if err := providerUpdate([]string{"glm", "--context-tokens", strconv.FormatInt(limit, 10), "--config", path}, stdout, stderr); err != nil {
			t.Fatal(err)
		}
		got, err := gateway.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("context override changed unrelated configuration")
		}
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []string{"-1", "1000000001", "4096", "1M"} {
		if err := providerUpdate([]string{"glm", "--context-tokens", limit, "--config", path}, stdout, stderr); err == nil {
			t.Fatalf("invalid context %q was accepted", limit)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("invalid context changed saved configuration")
		}
	}
	if err := providerShow([]string{"glm", "--config", path}, stdout, stderr); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(stdout.Name())
	if err != nil || !strings.Contains(string(output), "Context token override: 1000000") {
		t.Fatal("configured context override was not inspectable")
	}
}
