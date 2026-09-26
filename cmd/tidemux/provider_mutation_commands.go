package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/gateway"
	"golang.org/x/term"
)

func configurationKeychainReferencesInUse(c gateway.Config) (map[gateway.KeychainReference]struct{}, error) {
	inUse := make(map[gateway.KeychainReference]struct{})
	for _, reference := range []gateway.KeychainReference{c.AccessTokenKeychain, c.UpstreamKeychain} {
		if reference != (gateway.KeychainReference{}) {
			inUse[reference] = struct{}{}
		}
	}
	for _, provider := range c.Providers {
		references, err := provider.KeychainReferences()
		if err != nil {
			return nil, errors.New("provider Keychain references are invalid")
		}
		for _, reference := range references {
			inUse[reference] = struct{}{}
		}
	}
	return inUse, nil
}

func deleteUnreferencedProviderKeys(c gateway.Config, references []gateway.KeychainReference, store secretWriter) error {
	if store == nil {
		return errors.New("Keychain access is required")
	}
	for _, reference := range references {
		if err := reference.Validate("upstream_keychains"); err != nil {
			return errors.New("configuration was updated, but API key cleanup was skipped because a Keychain reference is invalid")
		}
	}
	inUse, err := configurationKeychainReferencesInUse(c)
	if err != nil {
		return errors.New("configuration was updated, but API key cleanup was skipped because Keychain references are invalid")
	}
	seen := make(map[gateway.KeychainReference]struct{}, len(references))
	deleteFailed := false
	for _, reference := range references {
		if _, duplicate := seen[reference]; duplicate {
			continue
		}
		seen[reference] = struct{}{}
		if _, referenced := inUse[reference]; referenced {
			continue
		}
		if err := store.Delete(context.Background(), reference); err != nil {
			deleteFailed = true
		}
	}
	if deleteFailed {
		return errors.New("configuration was updated, but one or more removed API keys could not be deleted from Keychain")
	}
	return nil
}

func providerUpdate(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	if ref == "" {
		return errors.New("usage: tidemux provider update REF [--endpoint URL] [--protocol PROTOCOL] [--model ID] [--rotate-key] [--config PATH]")
	}
	flags := flag.NewFlagSet("provider update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	endpoint := flags.String("endpoint", "", "replace API endpoint")
	protocol := flags.String("protocol", "", "force API protocol: openai or anthropic")
	model := flags.String("model", "", "replace default model")
	rotateKey := flags.Bool("rotate-key", false, "replace the API key using hidden terminal input")
	version := flags.String("anthropic-version", "2023-06-01", "Anthropic API version")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected provider update argument")
	}
	if *protocol != "" && *protocol != "openai" && *protocol != "anthropic" {
		return errors.New("--protocol must be openai or anthropic")
	}
	versionRequested := flagWasSet(flags, "anthropic-version")
	if *endpoint == "" && *protocol == "" && *model == "" && !*rotateKey && !versionRequested {
		return errors.New("provider update requires at least one changed field")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("provider credentials require macOS Keychain")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	c, err = migrateLegacyProvider(c, gateway.MacOSKeychain{})
	if err != nil {
		return err
	}
	p, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	var previousKeyRefs []gateway.KeychainReference
	if *rotateKey && len(p.UpstreamKeychains) > 1 {
		return errors.New("provider update --rotate-key supports one-key profiles; use provider key management for a key group")
	}
	if *rotateKey {
		previousKeyRefs, err = p.KeychainReferences()
		if err != nil {
			return errors.New("provider Keychain references are invalid")
		}
	}
	oldProtocol := p.Protocol
	if *endpoint != "" {
		if err := gateway.ValidateProviderBaseURL(*endpoint); err != nil {
			return err
		}
	}
	needsTTY := *endpoint != "" || *protocol != "" || *rotateKey
	var tty *os.File
	var newKey []byte
	if needsTTY {
		tty, err = openControlTTY("provider credential or endpoint changes require an interactive terminal")
		if err != nil {
			return err
		}
		defer tty.Close()
		if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
			return err
		}
	}
	key := ""
	if *endpoint != "" || *protocol != "" {
		references, referenceErr := p.KeychainReferences()
		if referenceErr != nil {
			return errors.New("provider Keychain references are invalid")
		}
		key, err = (gateway.MacOSKeychain{}).Lookup(context.Background(), references[0])
		if err != nil {
			return errors.New("provider Keychain item unavailable")
		}
	}
	if *rotateKey {
		fmt.Fprint(tty, "New provider API key (hidden): ")
		newKey, err = term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		if err != nil {
			return errors.New("could not read provider API key")
		}
		defer zeroBytes(newKey)
		if !validSecret(newKey) {
			return errors.New("a valid provider API key is required")
		}
		key = string(newKey)
	}
	if *endpoint != "" || *protocol != "" {
		baseURL := p.BaseURL
		if *endpoint != "" {
			baseURL = *endpoint
		}
		forced := *protocol
		if forced == "" && *endpoint == "" && p.Protocol != "auto" {
			forced = p.Protocol
		}
		selectedModel := *model
		if selectedModel == "" {
			selectedModel = p.Model
		}
		setup, err := inspectAndCompleteProvider(tty, baseURL, key, forced, selectedModel, *version, nil)
		if err != nil {
			return err
		}
		p.BaseURL, p.Protocol, p.APIVersion = setup.Provider.BaseURL, setup.Provider.Protocol, setup.Provider.APIVersion
		if *endpoint != "" {
			p.SupportedModels = nil
		}
		if *model != "" {
			p.Model = strings.TrimSpace(*model)
		}
	} else if *model != "" {
		p.Model = strings.TrimSpace(*model)
	}
	if p.Model == "" {
		return errors.New("provider default model cannot be empty")
	}
	if versionRequested && p.Protocol != "anthropic" {
		return errors.New("--anthropic-version is valid only for an Anthropic provider")
	}
	if versionRequested {
		p.APIVersion = *version
	}
	if p.Protocol == "anthropic" && p.APIVersion == "" {
		p.APIVersion = *version
	}
	if p.Protocol == "openai" {
		p.APIVersion = ""
	}
	if len(p.SupportedModels) > 0 && !containsConfiguredModel(p.SupportedModels, p.Model) {
		return errors.New("default model must be included in the provider model allowlist")
	}
	if c.DefaultProviders == nil {
		c.DefaultProviders = map[string]string{}
	}
	if oldProtocol != p.Protocol && c.DefaultProviders[oldProtocol] == ref {
		var alternatives []string
		for name, candidate := range c.Providers {
			if name != ref && candidate.Protocol == oldProtocol {
				alternatives = append(alternatives, name)
			}
		}
		sort.Strings(alternatives)
		if len(alternatives) > 1 {
			return fmt.Errorf("choose a replacement %s default first with `tidemux provider default %s REF`", oldProtocol, oldProtocol)
		}
		if len(alternatives) == 1 {
			c.DefaultProviders[oldProtocol] = alternatives[0]
		} else {
			delete(c.DefaultProviders, oldProtocol)
		}
	}
	c.Providers[ref] = p
	if c.DefaultProviders[p.Protocol] == "" {
		c.DefaultProviders[p.Protocol] = ref
	}
	if old := c.DefaultProviders; old != nil {
		for proto, selected := range old {
			if selected == ref && proto != p.Protocol {
				delete(old, proto)
			}
		}
	}
	if *rotateKey {
		gatewayKey, lookupErr := (gateway.MacOSKeychain{}).Lookup(context.Background(), c.AccessTokenKeychain)
		if lookupErr != nil {
			return errors.New("gateway Keychain item unavailable")
		}
		if gatewayKey == string(newKey) {
			return errors.New("provider API key must differ from the gateway API key")
		}
		newRef, err := newKeychainReference("com.tidemux.provider")
		if err != nil {
			return err
		}
		if len(p.UpstreamKeychains) > 0 {
			p.UpstreamKeychains = []gateway.KeychainReference{newRef}
			p.UpstreamKeychain = gateway.KeychainReference{}
		} else {
			p.UpstreamKeychain = newRef
		}
		c.Providers[ref] = p
		store := gateway.MacOSKeychain{}
		if err := store.StoreNew(context.Background(), newRef, string(newKey)); err != nil {
			return errors.New("cannot save provider API key in Keychain")
		}
		stored, lookupErr := store.Lookup(context.Background(), newRef)
		if lookupErr != nil || stored != string(newKey) {
			_ = store.Delete(context.Background(), newRef)
			return errors.New("provider API key Keychain read-back verification failed")
		}
		if err := writeCommandConfig(abs, c, before); err != nil {
			_ = store.Delete(context.Background(), newRef)
			return err
		}
		if err := deleteUnreferencedProviderKeys(c, previousKeyRefs, store); err != nil {
			return err
		}
	} else if err := writeCommandConfig(abs, c, before); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Updated provider %s.\n", ref)
	return nil
}

func providerRemove(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	if ref == "" {
		return errors.New("usage: tidemux provider remove REF [--default REF] [--yes] [--config PATH]")
	}
	flags := flag.NewFlagSet("provider remove", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	replacement := flags.String("default", "", "replacement provider when removing a protocol default")
	yes := flags.Bool("yes", false, "confirm removal")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected provider remove argument")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	c, err = migrateLegacyProvider(c, gateway.MacOSKeychain{})
	if err != nil {
		return err
	}
	p, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	keyReferences, err := p.KeychainReferences()
	if err != nil {
		return errors.New("provider Keychain references are invalid")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("provider removal requires macOS Keychain")
	}
	var tty *os.File
	if !*yes || (c.DefaultProviders[p.Protocol] == ref && *replacement == "") {
		tty, err = openControlTTY("provider removal or default replacement requires a terminal; pass --yes and --default for non-interactive use")
		if err != nil {
			return err
		}
		defer tty.Close()
	}
	if c.DefaultProviders[p.Protocol] == ref {
		var alternatives []string
		for name, candidate := range c.Providers {
			if name != ref && candidate.Protocol == p.Protocol {
				alternatives = append(alternatives, name)
			}
		}
		sort.Strings(alternatives)
		if *replacement != "" {
			if !containsConfiguredModel(alternatives, *replacement) {
				return errors.New("--default must name another provider with the same protocol")
			}
			c.DefaultProviders[p.Protocol] = *replacement
		} else if len(alternatives) == 0 {
			delete(c.DefaultProviders, p.Protocol)
		} else {
			fmt.Fprintf(tty, "Choose replacement default for %s (%s): ", p.Protocol, strings.Join(alternatives, ", "))
			answer, readErr := readTerminalLine(tty)
			if readErr != nil {
				return errors.New("could not read replacement provider")
			}
			answer = strings.TrimSpace(answer)
			if !containsConfiguredModel(alternatives, answer) {
				return errors.New("replacement must be one of the listed providers")
			}
			c.DefaultProviders[p.Protocol] = answer
		}
	} else if *replacement != "" {
		return errors.New("--default is only valid when removing a protocol default")
	}
	if !*yes {
		fmt.Fprintf(tty, "Remove provider %s (%s at %s)? [y/N] ", ref, p.Protocol, p.BaseURL)
		answer, readErr := readTerminalLine(tty)
		if readErr != nil || !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
			return errors.New("provider removal canceled")
		}
	}
	delete(c.Providers, ref)
	if err := writeCommandConfig(abs, c, before); err != nil {
		return err
	}
	if err := deleteUnreferencedProviderKeys(c, keyReferences, gateway.MacOSKeychain{}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Removed provider %s.\n", ref)
	return nil
}

func providerPricing(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New("usage: tidemux provider pricing <list|set|remove>")
	}
	verb := args[0]
	switch verb {
	case "list":
		return providerPricingList(args[1:], stdout, stderr)
	case "set":
		return providerPricingSet(args[1:], stdout, stderr)
	case "remove":
		return providerPricingRemove(args[1:], stdout, stderr)
	default:
		return errors.New("usage: tidemux provider pricing <list|set|remove>")
	}
}

func providerPricingList(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	if ref == "" {
		return errors.New("usage: tidemux provider pricing list REF [--config PATH]")
	}
	flags := flag.NewFlagSet("provider pricing list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected pricing list argument")
	}
	c, err := gateway.LoadConfig(*path)
	if err != nil {
		return err
	}
	p, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	models := make([]string, 0, len(p.Prices))
	for model := range p.Prices {
		models = append(models, model)
	}
	if _, ok := p.Prices[p.Model]; !ok {
		if _, builtIn := adapter.BuiltInPrice(p.BaseURL, p.Model, time.Now()); builtIn {
			models = append(models, p.Model)
		}
	}
	sort.Strings(models)
	for _, model := range models {
		price, ok := p.Prices[model]
		if !ok {
			price, _ = adapter.BuiltInPrice(p.BaseURL, model, time.Now())
		}
		fmt.Fprintf(stdout, "%s\t%s\tinput-hit=%s\tinput-miss=%s\toutput=%s\t%s (%s)\n", model, price.Currency, formatOptionalRate(price.InputCacheHit), formatOptionalRate(price.InputCacheMiss), formatOptionalRate(price.Output), price.Source, price.Version)
	}
	return nil
}

func providerPricingSet(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	model, rest := leadingEndpoint(rest)
	if ref == "" || model == "" {
		return errors.New("usage: tidemux provider pricing set REF MODEL --input-cache-hit RATE --input-cache-miss RATE --output RATE [--currency USD --source SOURCE --version VERSION]")
	}
	flags := flag.NewFlagSet("provider pricing set", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	currency := flags.String("currency", "USD", "ISO currency")
	source := flags.String("source", "manual-cli", "pricing source")
	version := flags.String("version", "manual", "pricing version")
	hit := flags.Float64("input-cache-hit", -1, "cache-hit input price per million tokens")
	miss := flags.Float64("input-cache-miss", -1, "cache-miss input price per million tokens")
	outputRate := flags.Float64("output", -1, "output price per million tokens")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected pricing set argument")
	}
	if *hit < 0 || *miss < 0 || *outputRate < 0 {
		return errors.New("pricing set requires --input-cache-hit, --input-cache-miss and --output")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	p, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	if p.Prices == nil {
		p.Prices = make(map[string]adapter.Price)
	}
	p.Prices[model] = adapter.Price{Currency: strings.ToUpper(*currency), Source: *source, Version: *version, InputCacheHit: ratePointer(*hit), InputCacheMiss: ratePointer(*miss), Output: ratePointer(*outputRate)}
	if err := p.Prices[model].Validate(); err != nil {
		return err
	}
	c.Providers[ref] = p
	if err := writeCommandConfig(abs, c, before); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Updated pricing for %s / %s.\n", ref, model)
	return nil
}

func providerPricingRemove(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	model, rest := leadingEndpoint(rest)
	if ref == "" || model == "" {
		return errors.New("usage: tidemux provider pricing remove REF MODEL [--config PATH]")
	}
	flags := flag.NewFlagSet("provider pricing remove", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected pricing remove argument")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	p, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	if _, ok := p.Prices[model]; !ok {
		return fmt.Errorf("no custom price for %s", model)
	}
	delete(p.Prices, model)
	if len(p.Prices) == 0 {
		p.Prices = nil
	}
	c.Providers[ref] = p
	if err := writeCommandConfig(abs, c, before); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Removed custom pricing for %s / %s.\n", ref, model)
	return nil
}

func ratePointer(value float64) *float64 { return &value }
func formatOptionalRate(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%g", *value)
}
