package main

import (
	"context"
	"errors"
	"github.com/hs3180/tidemux/internal/gateway"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memorySecrets struct {
	values map[string]string
	fail   bool
}

func (m *memorySecrets) StoreNew(_ context.Context, r gateway.KeychainReference, v string) error {
	if m.fail && len(m.values) > 0 {
		return errors.New("simulated")
	}
	m.values[r.Service+r.Account] = v
	return nil
}
func (m *memorySecrets) Lookup(_ context.Context, r gateway.KeychainReference) (string, error) {
	return m.values[r.Service+r.Account], nil
}
func (m *memorySecrets) Delete(_ context.Context, r gateway.KeychainReference) error {
	delete(m.values, r.Service+r.Account)
	return nil
}
func TestConfigureSavesReferencesAndRollsBack(t *testing.T) {
	for _, fail := range []bool{false, true} {
		dir := t.TempDir()
		p := filepath.Join(dir, "app", "config.json")
		m := &memorySecrets{values: map[string]string{}, fail: fail}
		c := gateway.Config{ListenAddr: "127.0.0.1:8787", Protocol: "openai", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", UpstreamID: "deepseek", MaxInFlight: 1, LedgerPath: filepath.Join(dir, "app", "ledger.db")}
		err := saveConfiguration(p, c, "provider-private", false, m)
		if fail {
			if err == nil || len(m.values) != 0 {
				t.Fatal("credentials not rolled back")
			}
			if _, e := os.Stat(p); !os.IsNotExist(e) {
				t.Fatal("partial config")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "provider-private") {
			t.Fatal("credential on disk")
		}
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0600 {
			t.Fatal("config mode")
		}
		loaded, err := gateway.LoadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := loaded.ResolveCredentials(context.Background(), m)
		if err != nil || resolved.APIKey != "provider-private" || resolved.AccessToken == resolved.APIKey {
			t.Fatal("credential roundtrip")
		}
		before := len(m.values)
		if saveConfiguration(p, c, "another", false, m) == nil || len(m.values) != before {
			t.Fatal("existing config overwritten")
		}
	}
}
