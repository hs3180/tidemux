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
		err := saveConfiguration(p, c, "provider-private", nil, false, m)
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
		if saveConfiguration(p, c, "another", nil, false, m) == nil || len(m.values) != before {
			t.Fatal("existing config overwritten")
		}
	}
}

func TestSaveIndependentProviderConfigurationSharesOrSeparatesKeychainItems(t *testing.T) {
	for _, test := range []struct {
		name         string
		openAIKey    string
		anthropicKey string
		shared       bool
		wantItems    int
	}{
		{name: "shared key", openAIKey: "same-provider-key", anthropicKey: "same-provider-key", shared: true, wantItems: 2},
		{name: "separate keys", openAIKey: "openai-provider-key", anthropicKey: "anthropic-provider-key", wantItems: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			c := gateway.Config{
				ListenAddr: "127.0.0.1:8787", MaxInFlight: 1,
				LedgerPath: filepath.Join(dir, "ledger.db"),
				Providers: map[string]gateway.Provider{
					"deepseek-openai":    {Protocol: "openai", BaseURL: "https://api.deepseek.com/v1", Model: "openai-model", UpstreamID: "deepseek-openai"},
					"deepseek-anthropic": {Protocol: "anthropic", BaseURL: "https://api.deepseek.com/anthropic/v1", Model: "anthropic-model", UpstreamID: "deepseek-anthropic"},
				},
				DefaultProviders: map[string]string{"openai": "deepseek-openai", "anthropic": "deepseek-anthropic"},
			}
			secrets := &memorySecrets{values: map[string]string{}}
			providerKeys := map[string]string{"deepseek-openai": test.openAIKey, "deepseek-anthropic": test.anthropicKey}
			if err := saveConfigurationWithProviderKeys(path, c, providerKeys, nil, false, secrets); err != nil {
				t.Fatal(err)
			}
			if len(secrets.values) != test.wantItems {
				t.Fatalf("Keychain items=%d want %d", len(secrets.values), test.wantItems)
			}
			loaded, err := gateway.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			openAIRef := loaded.Providers["deepseek-openai"].UpstreamKeychain
			anthropicRef := loaded.Providers["deepseek-anthropic"].UpstreamKeychain
			if (openAIRef == anthropicRef) != test.shared {
				t.Fatalf("shared refs=%v openai=%+v anthropic=%+v", openAIRef == anthropicRef, openAIRef, anthropicRef)
			}
			resolved, err := loaded.ResolveCredentials(context.Background(), secrets)
			if err != nil || resolved.Providers["deepseek-openai"].APIKey != test.openAIKey || resolved.Providers["deepseek-anthropic"].APIKey != test.anthropicKey {
				t.Fatalf("resolved provider keys mismatch: err=%v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || strings.Contains(string(data), "provider-key") {
				t.Fatalf("provider secret written to config: err=%v", err)
			}
		})
	}
}

func TestSaveConfigurationOmitsAutoProtocol(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{ListenAddr: "127.0.0.1:8787", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", UpstreamID: "provider-primary", MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db")}
	secrets := &memorySecrets{values: map[string]string{}}
	if err := saveConfiguration(path, c, "provider-secret", nil, false, secrets); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"protocol"`) || !strings.Contains(string(data), `"service": "com.tidemux.provider"`) {
		t.Fatalf("configuration is not provider-neutral: %s", data)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil || loaded.Protocol != "" {
		t.Fatalf("loaded protocol=%q err=%v", loaded.Protocol, err)
	}
}

func TestReplaceKeepsRestorableConfigAndCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{ListenAddr: "127.0.0.1:8787", Protocol: "openai", BaseURL: "https://example.com/v1", Model: "old-model", UpstreamID: "test", MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db")}
	secrets := &memorySecrets{values: map[string]string{}}
	if err := saveConfiguration(path, c, "old-secret", nil, false, secrets); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Model = "new-model"
	if err := saveConfiguration(path, c, "new-secret", nil, true, secrets); err != nil {
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

func TestSaveConfigurationPersistsExternalListenMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{ListenAddr: "0.0.0.0:8787", Protocol: "openai", BaseURL: "https://example.com/v1", Model: "model", UpstreamID: "test", MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db")}
	secrets := &memorySecrets{values: map[string]string{}}
	if err := saveConfiguration(path, c, "secret", nil, false, secrets); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ListenAddr != c.ListenAddr {
		t.Fatalf("external listen settings were not persisted: %+v", loaded)
	}
	resolved, err := loaded.ResolveCredentials(context.Background(), secrets)
	if err != nil || resolved.AccessToken == "" || resolved.AccessToken == "secret" {
		t.Fatalf("random gateway API key was not persisted in Keychain: err=%v token=%q", err, resolved.AccessToken)
	}
}

func TestSaveConfigurationPersistsCustomGatewayKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := gateway.Config{ListenAddr: "0.0.0.0:8787", Protocol: "openai", BaseURL: "https://example.com/v1", Model: "model", UpstreamID: "test", MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db")}
	secrets := &memorySecrets{values: map[string]string{}}
	if err := saveConfiguration(path, c, "provider-secret", []byte("custom-gateway-key"), false, secrets); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := loaded.ResolveCredentials(context.Background(), secrets)
	if err != nil || resolved.AccessToken != "custom-gateway-key" {
		t.Fatalf("custom gateway API key was not persisted in Keychain: err=%v token=%q", err, resolved.AccessToken)
	}
}

func TestSaveConfigurationRejectsInvalidCustomGatewayKey(t *testing.T) {
	dir := t.TempDir()
	c := gateway.Config{ListenAddr: "0.0.0.0:8787", Protocol: "openai", BaseURL: "https://example.com/v1", Model: "model", UpstreamID: "test", MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db")}
	for _, gatewaySecret := range []string{"provider-secret", "\n", "   "} {
		if err := saveConfiguration(filepath.Join(dir, gatewaySecret+".json"), c, "provider-secret", []byte(gatewaySecret), false, &memorySecrets{values: map[string]string{}}); err == nil {
			t.Errorf("gateway key %q was accepted", gatewaySecret)
		}
	}
}

func TestLoopbackListenAddress(t *testing.T) {
	for _, test := range []struct {
		addr     string
		loopback bool
	}{
		{addr: "127.0.0.1:8787", loopback: true},
		{addr: "[::1]:8787", loopback: true},
		{addr: "0.0.0.0:8787", loopback: false},
		{addr: "192.168.1.10:8787", loopback: false},
	} {
		if got := isLoopbackListenAddr(test.addr); got != test.loopback {
			t.Errorf("isLoopbackListenAddr(%q)=%v, want %v", test.addr, got, test.loopback)
		}
	}
}

func TestConfigurePricesUsesPeakDeepSeekPreset(t *testing.T) {
	flags, currency, source, version, cacheHit, cacheMiss, output := pricingTestFlags(t)
	prices, err := configurePrices(flags, "deepseek-flash", "https://api.deepseek.com", "deepseek-flash", *currency, *source, *version, *cacheHit, *cacheMiss, *output)
	if err != nil {
		t.Fatal(err)
	}
	price := prices["deepseek-flash"]
	if price.Version != "deepseek-v4-pricing-2026-08-16-peak" || price.InputCacheMiss == nil || *price.InputCacheMiss != .3 || price.Output == nil || *price.Output != 1.2 || price.InputCacheHit == nil || *price.InputCacheHit != .006 {
		t.Fatalf("price=%+v", price)
	}
}

func TestConfigurePricesAcceptsCustomCommandLineRates(t *testing.T) {
	flags, currency, source, version, cacheHit, cacheMiss, output := pricingTestFlags(t, "--pricing-currency", "CNY", "--pricing-source", "provider-docs", "--pricing-version", "2026-09-20", "--pricing-input-cache-hit", "0.5", "--pricing-input-cache-miss", "2.5", "--pricing-output", "7.5")
	prices, err := configurePrices(flags, "", "https://provider.example/v1", "custom-model", *currency, *source, *version, *cacheHit, *cacheMiss, *output)
	if err != nil {
		t.Fatal(err)
	}
	price := prices["custom-model"]
	if price.Currency != "CNY" || price.Source != "provider-docs" || price.Version != "2026-09-20" || price.InputCacheMiss == nil || *price.InputCacheMiss != 2.5 || price.InputCacheHit == nil || *price.InputCacheHit != .5 || price.Output == nil || *price.Output != 7.5 {
		t.Fatalf("price=%+v", price)
	}
}

func TestConfigurePricesRequiresCustomRatesForNonPreset(t *testing.T) {
	flags, currency, source, version, cacheHit, cacheMiss, output := pricingTestFlags(t)
	if _, err := configurePrices(flags, "", "https://provider.example/v1", "custom-model", *currency, *source, *version, *cacheHit, *cacheMiss, *output); err == nil || !strings.Contains(err.Error(), "pricing is required") {
		t.Fatalf("error=%v", err)
	}
	flags, currency, source, version, cacheHit, cacheMiss, output = pricingTestFlags(t, "--pricing-input-cache-miss", "1")
	if _, err := configurePrices(flags, "", "https://provider.example/v1", "custom-model", *currency, *source, *version, *cacheHit, *cacheMiss, *output); err == nil || !strings.Contains(err.Error(), "requires --pricing-input-cache-hit, --pricing-input-cache-miss and --pricing-output") {
		t.Fatalf("partial error=%v", err)
	}
}

func TestConfigureWizardPricesNeverGuessesUnknownRates(t *testing.T) {
	flags := flag.NewFlagSet("wizard-pricing", flag.ContinueOnError)
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	prices, err := configureWizardPrices(flags, "https://api.example.com/v1", "custom-model", "USD", "manual-cli", "manual", 0, 0, 0)
	if err != nil || len(prices) != 0 {
		t.Fatalf("unknown provider pricing=%v err=%v", prices, err)
	}
	prices, err = configureWizardPrices(flags, "https://api.deepseek.com", "deepseek-flash", "USD", "manual-cli", "manual", 0, 0, 0)
	if err != nil || len(prices) != 1 {
		t.Fatalf("built-in provider pricing=%v err=%v", prices, err)
	}
}

func TestConfigureDoesNotAcceptPricingFile(t *testing.T) {
	if err := configure([]string{"--pricing-file", "pricing.json"}, os.Stdout, os.Stderr); err == nil {
		t.Fatal("configure still accepts --pricing-file")
	}
}

func TestConfigureDoesNotAcceptProviderProtocolFlag(t *testing.T) {
	if err := configure([]string{"--protocol", "openai"}, os.Stdout, os.Stderr); err == nil {
		t.Fatal("configure still accepts --protocol")
	}
}

func TestConfigureDoesNotAcceptDualProviderFlag(t *testing.T) {
	if err := configure([]string{"--dual-provider"}, os.Stdout, os.Stderr); err == nil {
		t.Fatal("configure still accepts --dual-provider")
	}
}

func TestParseNamedProviderFlags(t *testing.T) {
	name, provider, err := parseProviderFlag("deepseek-alt,openai,https://api.deepseek.com/v1,deepseek-chat")
	if err != nil || name != "deepseek-alt" || provider.Protocol != "openai" || provider.BaseURL != "https://api.deepseek.com/v1" || provider.Model != "deepseek-chat" {
		t.Fatalf("parsed provider = %q, %+v, err=%v", name, provider, err)
	}
	name, provider, err = parseProviderFlag("automatic,https://api.example.com/v1,model-id")
	if err != nil || name != "automatic" || provider.Protocol != "auto" || provider.BaseURL != "https://api.example.com/v1" || provider.Model != "model-id" {
		t.Fatalf("parsed automatic provider = %q, %+v, err=%v", name, provider, err)
	}
	protocol, defaultName, err := parseDefaultProviderFlag("openai=deepseek-alt")
	if err != nil || protocol != "openai" || defaultName != "deepseek-alt" {
		t.Fatalf("parsed default = %q=%q, err=%v", protocol, defaultName, err)
	}
	if _, _, err := parseProviderFlag("missing-fields"); err == nil {
		t.Fatal("malformed provider spec accepted")
	}
	providerName, supported, err := parseProviderModelsFlag("deepseek-alt,deepseek-chat,deepseek-reasoner")
	if err != nil || providerName != "deepseek-alt" || strings.Join(supported, ",") != "deepseek-chat,deepseek-reasoner" {
		t.Fatalf("parsed provider model scope=%q %v err=%v", providerName, supported, err)
	}
	for _, invalid := range []string{"missing-models", "provider,model,,other", "provider,model,model"} {
		if _, _, err := parseProviderModelsFlag(invalid); err == nil {
			t.Errorf("invalid provider model scope %q was accepted", invalid)
		}
	}
}

func TestProviderNameInferredFromBaseURL(t *testing.T) {
	for _, test := range []struct {
		baseURL string
		want    string
	}{
		{baseURL: "https://api.deepseek.com/anthropic/v1", want: "deepseek"},
		{baseURL: "https://api.openai.com/v1", want: "openai"},
		{baseURL: "https://localhost:8443/v1", want: "local-provider"},
		{baseURL: "http://127.0.0.1:4000/v1", want: "local-provider"},
	} {
		if got := providerNameFromBaseURL(test.baseURL); got != test.want {
			t.Errorf("providerNameFromBaseURL(%q)=%q, want %q", test.baseURL, got, test.want)
		}
	}
}

func TestChooseProviderModelUsesDiscoveryAndMinimalInput(t *testing.T) {
	t.Run("single discovered model needs no input", func(t *testing.T) {
		in, out := promptTestFiles(t, "")
		defer in.Close()
		defer out.Close()
		got, err := chooseProviderModel(in, out, []string{"only-model"}, true)
		if err != nil || got != "only-model" {
			t.Fatalf("model=%q err=%v", got, err)
		}
	})

	t.Run("multiple models accept an index", func(t *testing.T) {
		in, out := promptTestFiles(t, "2\n")
		defer in.Close()
		defer out.Close()
		got, err := chooseProviderModel(in, out, []string{"first", "second"}, true)
		if err != nil || got != "second" {
			t.Fatalf("model=%q err=%v", got, err)
		}
	})

	t.Run("unavailable model list asks for an ID", func(t *testing.T) {
		in, out := promptTestFiles(t, "custom-model\n")
		defer in.Close()
		defer out.Close()
		got, err := chooseProviderModel(in, out, nil, false)
		if err != nil || got != "custom-model" {
			t.Fatalf("model=%q err=%v", got, err)
		}
	})

	t.Run("known catalog rejects an unlisted model", func(t *testing.T) {
		in, out := promptTestFiles(t, "not-listed\n")
		defer in.Close()
		defer out.Close()
		if _, err := chooseProviderModel(in, out, []string{"first", "second"}, true); err == nil {
			t.Fatal("unlisted model accepted")
		}
	})
}

func TestChooseProviderModelsDefaultsToAllAndCanRestrict(t *testing.T) {
	t.Run("blank means all discovered models", func(t *testing.T) {
		in, out := promptTestFiles(t, "\n")
		defer in.Close()
		defer out.Close()
		got, err := chooseProviderModels(in, out, []string{"first", "second"}, true, "first")
		if err != nil || len(got) != 0 {
			t.Fatalf("models=%v err=%v", got, err)
		}
	})

	t.Run("numeric selection restricts and preserves catalog order", func(t *testing.T) {
		in, out := promptTestFiles(t, "3,2\n")
		defer in.Close()
		defer out.Close()
		got, err := chooseProviderModels(in, out, []string{"first", "second", "third"}, true, "second")
		if err != nil || strings.Join(got, ",") != "second,third" {
			t.Fatalf("models=%v err=%v", got, err)
		}
	})

	t.Run("manual selection is available without discovery", func(t *testing.T) {
		in, out := promptTestFiles(t, "custom-a,custom-b\n")
		defer in.Close()
		defer out.Close()
		got, err := chooseProviderModels(in, out, nil, false, "custom-b")
		if err != nil || strings.Join(got, ",") != "custom-a,custom-b" {
			t.Fatalf("models=%v err=%v", got, err)
		}
	})

	t.Run("a restricted scope must include the fallback model", func(t *testing.T) {
		in, out := promptTestFiles(t, "2\n1\n")
		defer in.Close()
		defer out.Close()
		got, err := chooseProviderModels(in, out, []string{"first", "second"}, true, "first")
		if err != nil || strings.Join(got, ",") != "first" {
			t.Fatalf("models=%v err=%v", got, err)
		}
	})
}

func promptTestFiles(t *testing.T, input string) (*os.File, *os.File) {
	t.Helper()
	inputPath := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(inputPath, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		in.Close()
		t.Fatal(err)
	}
	return in, out
}

func TestConfigureRejectsOldDeepSeekPresetName(t *testing.T) {
	if err := configure([]string{"--preset", "deepseek"}, os.Stdout, os.Stderr); err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Fatalf("error=%v", err)
	}
}

func pricingTestFlags(t *testing.T, args ...string) (*flag.FlagSet, *string, *string, *string, *float64, *float64, *float64) {
	t.Helper()
	flags := flag.NewFlagSet("pricing-test", flag.ContinueOnError)
	currency := flags.String("pricing-currency", "USD", "")
	source := flags.String("pricing-source", "manual-cli", "")
	version := flags.String("pricing-version", "manual", "")
	cacheHit := flags.Float64("pricing-input-cache-hit", 0, "")
	cacheMiss := flags.Float64("pricing-input-cache-miss", 0, "")
	output := flags.Float64("pricing-output", 0, "")
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return flags, currency, source, version, cacheHit, cacheMiss, output
}
