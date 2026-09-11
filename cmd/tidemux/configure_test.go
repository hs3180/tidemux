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

func TestReplaceKeepsRestorableConfigAndCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{ListenAddr: "127.0.0.1:8787", Protocol: "openai", BaseURL: "https://example.com/v1", Model: "old-model", UpstreamID: "test", MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db")}
	secrets := &memorySecrets{values: map[string]string{}}
	if err := saveConfiguration(path, c, "old-secret", false, secrets); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Model = "new-model"
	if err := saveConfiguration(path, c, "new-secret", true, secrets); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(path + ".backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("missing unique backup")
	}
	saved, err := os.ReadFile(backups[0])
	if err != nil || string(saved) != string(before) {
		t.Fatal("backup changed config bytes")
	}
	info, _ := os.Stat(backups[0])
	if info.Mode().Perm() != 0o600 {
		t.Fatal("backup permissions")
	}
	old, err := gateway.LoadConfig(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := old.ResolveCredentials(context.Background(), secrets)
	if err != nil || resolved.APIKey != "old-secret" || old.Model != "old-model" {
		t.Fatal("old profile cannot be restored")
	}
	current, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = current.ResolveCredentials(context.Background(), secrets)
	if err != nil || resolved.APIKey != "new-secret" || current.Model != "new-model" {
		t.Fatal("new profile not installed")
	}
	if len(secrets.values) != 4 {
		t.Fatal("old credentials removed")
	}
}
