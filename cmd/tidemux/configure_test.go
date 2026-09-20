package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
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

func TestConfigurePricesUsesPeakDeepSeekPreset(t *testing.T) {
	flags, currency, source, version, input, output, cacheRead, cacheWrite := pricingTestFlags(t)
	prices, err := configurePrices(flags, "deepseek", "https://api.deepseek.com", "deepseek-flash", *currency, *source, *version, *input, *output, *cacheRead, *cacheWrite)
	if err != nil {
		t.Fatal(err)
	}
	price := prices["deepseek-flash"]
	if price.Version != "deepseek-v4-pricing-2026-08-16-peak" || price.Input == nil || *price.Input != .3 || price.Output == nil || *price.Output != 1.2 {
		t.Fatalf("price=%+v", price)
	}
}

func TestConfigurePricesAcceptsCustomCommandLineRates(t *testing.T) {
	flags, currency, source, version, input, output, cacheRead, cacheWrite := pricingTestFlags(t, "--pricing-currency", "CNY", "--pricing-source", "provider-docs", "--pricing-version", "2026-09-20", "--pricing-input", "2.5", "--pricing-output", "7.5", "--pricing-cache-read", "0.5", "--pricing-cache-write", "1")
	prices, err := configurePrices(flags, "", "https://provider.example/v1", "custom-model", *currency, *source, *version, *input, *output, *cacheRead, *cacheWrite)
	if err != nil {
		t.Fatal(err)
	}
	price := prices["custom-model"]
	if price.Currency != "CNY" || price.Source != "provider-docs" || price.Version != "2026-09-20" || price.Input == nil || *price.Input != 2.5 || price.Output == nil || *price.Output != 7.5 || price.CacheRead == nil || *price.CacheRead != .5 || price.CacheWrite == nil || *price.CacheWrite != 1 {
		t.Fatalf("price=%+v", price)
	}
}

func TestConfigurePricesRequiresCustomRatesForNonPreset(t *testing.T) {
	flags, currency, source, version, input, output, cacheRead, cacheWrite := pricingTestFlags(t)
	if _, err := configurePrices(flags, "", "https://provider.example/v1", "custom-model", *currency, *source, *version, *input, *output, *cacheRead, *cacheWrite); err == nil || !strings.Contains(err.Error(), "pricing is required") {
		t.Fatalf("error=%v", err)
	}
	flags, currency, source, version, input, output, cacheRead, cacheWrite = pricingTestFlags(t, "--pricing-input", "1")
	if _, err := configurePrices(flags, "", "https://provider.example/v1", "custom-model", *currency, *source, *version, *input, *output, *cacheRead, *cacheWrite); err == nil || !strings.Contains(err.Error(), "requires --pricing-input and --pricing-output") {
		t.Fatalf("partial error=%v", err)
	}
}

func TestConfigureDoesNotAcceptPricingFile(t *testing.T) {
	if err := configure([]string{"--pricing-file", "pricing.json"}, os.Stdout, os.Stderr); err == nil {
		t.Fatal("configure still accepts --pricing-file")
	}
}

func pricingTestFlags(t *testing.T, args ...string) (*flag.FlagSet, *string, *string, *string, *float64, *float64, *float64, *float64) {
	t.Helper()
	flags := flag.NewFlagSet("pricing-test", flag.ContinueOnError)
	currency := flags.String("pricing-currency", "USD", "")
	source := flags.String("pricing-source", "manual-cli", "")
	version := flags.String("pricing-version", "manual", "")
	input := flags.Float64("pricing-input", 0, "")
	output := flags.Float64("pricing-output", 0, "")
	cacheRead := flags.Float64("pricing-cache-read", 0, "")
	cacheWrite := flags.Float64("pricing-cache-write", 0, "")
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return flags, currency, source, version, input, output, cacheRead, cacheWrite
}
