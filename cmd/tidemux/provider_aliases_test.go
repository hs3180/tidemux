package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestProviderAliasUpdatePreservesUnrelatedConfigurationAndRejectsInvalidMaps(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("provider update requires macOS")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	p := gateway.Provider{Protocol: "openai", BaseURL: "https://provider.example/v1",
		UpstreamKeychain: gateway.KeychainReference{Service: "provider", Account: "default"},
		SupportedModels:  []string{"kimi-k3"}, MaxActiveSessions: 3,
		ModelCapabilities: gateway.ModelCapabilities{ContextTokens: 1024000, MaxOutputTokens: 131072}}
	c := gateway.Config{ListenAddr: defaultListenAddr, MaxInFlight: 4,
		LedgerPath:          filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "default"},
		Providers:           map[string]gateway.Provider{"ark": p, "other": p}}
	if err := writeCommandConfig(path, c, nil); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(dir, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	for _, aliasJSON := range []string{`{"kimi-k3":"kimi-k3.1-pro"}`, `{}`} {
		want, err := gateway.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		p := want.Providers["ark"]
		p.ModelAliases = nil
		if aliasJSON != `{}` {
			p.ModelAliases = map[string]string{"kimi-k3": "kimi-k3.1-pro"}
		}
		want.Providers["ark"] = p
		if err := providerUpdate([]string{"ark", "--model-aliases", aliasJSON, "--config", path}, out, out); err != nil {
			t.Fatal(err)
		}
		got, err := gateway.LoadConfig(path)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("alias update changed unrelated settings", err)
		}
	}
	before, _ := os.ReadFile(path)
	for _, bad := range []string{`null`, `[]`, `true`, `{"stable":null}`, `{"stable":1}`, `{"a":"a"}`, `{"a":"b","b":"c"}`, `{"auto":"target"}`} {
		if err := providerUpdate([]string{"ark", "--model-aliases", bad, "--config", path}, out, out); err == nil {
			t.Fatal("invalid alias map accepted")
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(before) {
			t.Fatal("failed update changed persisted config")
		}
	}
}
