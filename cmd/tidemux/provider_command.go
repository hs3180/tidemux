package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
	"golang.org/x/term"
)

func providerCommand(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New("usage: tidemux provider <add|list|show|update|remove|default|models|pricing>")
	}
	switch args[0] {
	case "add":
		return providerAdd(args[1:], stdout, stderr)
	case "list":
		return providerList(args[1:], stdout, stderr)
	case "show":
		return providerShow(args[1:], stdout, stderr)
	case "update":
		return providerUpdate(args[1:], stdout, stderr)
	case "remove":
		return providerRemove(args[1:], stdout, stderr)
	case "default":
		return providerDefault(args[1:], stdout, stderr)
	case "models":
		return providerModels(args[1:], stdout, stderr)
	case "pricing":
		return providerPricing(args[1:], stdout, stderr)
	default:
		return errors.New("usage: tidemux provider <add|list|show|update|remove|default|models|pricing>")
	}
}

func providerAdd(args []string, stdout, stderr *os.File) error {
	endpoint, rest := leadingEndpoint(args)
	flags := flag.NewFlagSet("provider add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "configuration path")
	name := flags.String("name", "", "optional provider label")
	protocol := flags.String("protocol", "", "force endpoint protocol: openai or anthropic")
	model := flags.String("model", "", "default model when clients omit one")
	apiVersion := flags.String("anthropic-version", "2023-06-01", "Anthropic API version")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*configPath) == "" {
		return errors.New("usage: tidemux provider add [ENDPOINT] [--name LABEL] [--protocol openai|anthropic] [--model ID] [--config PATH]")
	}
	if *protocol != "" && *protocol != "openai" && *protocol != "anthropic" {
		return errors.New("--protocol must be openai or anthropic")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("provider credentials require macOS Keychain")
	}
	tty, err := openControlTTY("provider setup requires an interactive terminal; API keys are read securely and never accepted as arguments")
	if err != nil {
		return err
	}
	defer tty.Close()
	if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
		return err
	}

	var setup interactiveProviderSetup
	if endpoint == "" {
		setup, err = promptNewProvider(tty, tty, *apiVersion, nil, *protocol, *model, *name)
		if err != nil {
			return err
		}
	} else {
		if err := gateway.ValidateProviderBaseURL(endpoint); err != nil {
			return err
		}
		fmt.Fprint(tty, "Provider API key (hidden): ")
		key, readErr := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		if readErr != nil {
			return errors.New("could not read provider API key")
		}
		defer zeroBytes(key)
		if !validSecret(key) {
			return errors.New("a valid provider API key is required")
		}
		setup, err = inspectAndCompleteProvider(tty, endpoint, string(key), *protocol, *model, *apiVersion, nil)
		if err != nil {
			return err
		}
		setup.APIKey = key
	}
	defer zeroBytes(setup.APIKey)
	if *name != "" {
		setup.Name = *name
	}
	path, err := filepath.Abs(*configPath)
	if err != nil {
		return errors.New("invalid config path")
	}
	c, path, before, err := loadCommandConfig(path)
	if err != nil {
		return err
	}
	newSetup := before == nil
	var reportScheduleChanged bool
	if endpoint == "" && newSetup {
		c.ReportSchedule, err = promptReportSchedule(tty, tty)
		if err != nil {
			return err
		}
		reportScheduleChanged = c.ReportSchedule != (gateway.ReportSchedule{})
	}
	c, err = migrateLegacyProvider(c, gateway.MacOSKeychain{})
	if err != nil {
		return err
	}
	providerName := uniqueProviderName(c.Providers, setup.Name)
	if !validProviderLabel(providerName) {
		return errors.New("provider labels may contain only letters, numbers, dots, underscores and hyphens")
	}
	if *name != "" && providerName != *name {
		return fmt.Errorf("provider %q already exists; choose another --name", *name)
	}
	setup.Provider.UpstreamID = providerName
	setup.Provider.ModelCapabilities = gateway.ModelCapabilities{}
	if setup.Provider.Protocol == "anthropic" && setup.Provider.APIVersion == "" {
		setup.Provider.APIVersion = *apiVersion
	}
	if c.Providers == nil {
		c.Providers = map[string]gateway.Provider{}
	}
	if c.DefaultProviders == nil {
		c.DefaultProviders = map[string]string{}
	}
	selectInitialProviderDefault(c.Providers, c.DefaultProviders, setup.Provider.Protocol, providerName)
	c.Providers[providerName] = setup.Provider
	store := gateway.MacOSKeychain{}
	if err := persistProviderAddition(path, c, before, providerName, string(setup.APIKey), store); err != nil {
		return err
	}
	if reportScheduleChanged {
		if _, err := syncReportSchedule(path, c.ReportSchedule); err != nil {
			return fmt.Errorf("provider added, but daily notification setup failed: %w", err)
		}
	}
	fmt.Fprintf(stdout, "Added provider %s (%s) at %s; default model %s.\n", providerName, setup.Provider.Protocol, setup.Provider.BaseURL, setup.Provider.Model)
	if c.DefaultProviders[setup.Provider.Protocol] == providerName {
		fmt.Fprintf(stdout, "Default %s provider: %s\n", setup.Provider.Protocol, providerName)
	}
	fmt.Fprintln(stdout, "All models are allowed. Restrict them with `tidemux provider models` if needed.")
	return nil
}

func leadingEndpoint(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func openControlTTY(message string) (*os.File, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New(message)
	}
	if !term.IsTerminal(int(tty.Fd())) {
		tty.Close()
		return nil, errors.New("interactive terminal required")
	}
	return tty, nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func validSecret(value []byte) bool {
	return strings.TrimSpace(string(value)) != "" && !strings.ContainsAny(string(value), "\r\n\x00")
}

func inspectAndCompleteProvider(tty *os.File, endpoint, key, forcedProtocol, model, apiVersion string, client *http.Client) (interactiveProviderSetup, error) {
	protocol := forcedProtocol
	var models []string
	var known bool
	if protocol == "" {
		info, err := gateway.InspectProviderEndpoint(context.Background(), endpoint, key, apiVersion, client)
		if err == nil {
			protocol, models, known = info.Protocol, info.Models, info.ModelsKnown
			fmt.Fprintf(tty, "Detected upstream protocol: %s\n", protocol)
		} else {
			fmt.Fprintf(tty, "Automatic protocol detection was unavailable: %v\n", err)
			protocol, err = promptProviderProtocol(tty, tty)
			if err != nil {
				return interactiveProviderSetup{}, err
			}
		}
	} else {
		fmt.Fprintf(tty, "Using upstream protocol: %s\n", protocol)
	}
	if len(models) == 0 && !known {
		models, known = gateway.DiscoverProviderModels(endpoint, key, apiVersion, protocol, client)
		if !known {
			fmt.Fprintln(tty, "Model discovery is unavailable; the model ID will be entered manually.")
		}
	}
	selectedModel := strings.TrimSpace(model)
	if selectedModel == "" {
		var err error
		selectedModel, err = chooseProviderModel(tty, tty, models, known)
		if err != nil {
			return interactiveProviderSetup{}, err
		}
	}
	version := ""
	if protocol == "anthropic" {
		version = apiVersion
	}
	name := providerNameFromBaseURL(endpoint)
	return interactiveProviderSetup{Name: name, Provider: gateway.Provider{Protocol: protocol, BaseURL: endpoint, APIVersion: version, Model: selectedModel, UpstreamID: name}}, nil
}

func uniqueProviderName(providers map[string]gateway.Provider, candidate string) string {
	base := strings.Trim(strings.TrimSpace(candidate), "-.")
	if base == "" {
		base = "provider"
	}
	if len(base) > 80 {
		base = base[:80]
	}
	if _, exists := providers[base]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		ending := fmt.Sprintf("-%d", suffix)
		prefix := base
		if len(prefix)+len(ending) > 80 {
			prefix = prefix[:80-len(ending)]
		}
		name := prefix + ending
		if _, exists := providers[name]; !exists {
			return name
		}
	}
}

func validProviderLabel(value string) bool {
	if value == "" || len(value) > 80 || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func selectInitialProviderDefault(providers map[string]gateway.Provider, defaults map[string]string, protocol, added string) {
	if defaults[protocol] != "" {
		return
	}
	var existing []string
	for name, provider := range providers {
		if provider.Protocol == protocol {
			existing = append(existing, name)
		}
	}
	if len(existing) == 0 {
		defaults[protocol] = added
		return
	}
	sort.Strings(existing)
	defaults[protocol] = existing[0]
}

func migrateLegacyProvider(c gateway.Config, secrets gateway.SecretLookup) (gateway.Config, error) {
	if c.BaseURL == "" || len(c.Providers) != 0 {
		return c, nil
	}
	name := uniqueProviderName(nil, providerNameFromBaseURL(c.BaseURL))
	protocol := c.Protocol
	if protocol == "" {
		protocol = "auto"
	}
	if secrets != nil && (protocol == "" || protocol == "auto") {
		key, err := secrets.Lookup(context.Background(), c.UpstreamKeychain)
		if err == nil {
			if info, inspectErr := gateway.InspectProviderEndpoint(context.Background(), c.BaseURL, key, c.APIVersion, nil); inspectErr == nil {
				protocol = info.Protocol
			}
		}
	}
	provider := gateway.Provider{Protocol: protocol, BaseURL: c.BaseURL, UpstreamKeychain: c.UpstreamKeychain, APIVersion: c.APIVersion, Model: c.Model, UpstreamID: c.UpstreamID, ModelCapabilities: c.ModelCapabilities, Prices: c.Prices}
	c.Providers = map[string]gateway.Provider{name: provider}
	c.DefaultProviders = map[string]string{}
	if protocol == "openai" || protocol == "anthropic" {
		c.DefaultProviders[protocol] = name
	}
	c.Protocol, c.BaseURL, c.APIVersion, c.UpstreamKeychain = "", "", "", gateway.KeychainReference{}
	c.Model, c.UpstreamID = "", ""
	c.ModelCapabilities, c.Prices = gateway.ModelCapabilities{}, nil
	return c, nil
}

func persistProviderAddition(path string, c gateway.Config, before []byte, name, apiKey string, store secretWriter) error {
	provider := c.Providers[name]
	providerRef, err := newKeychainReference("com.tidemux.provider")
	if err != nil {
		return err
	}
	provider.UpstreamKeychain = providerRef
	c.Providers[name] = provider
	created := []gateway.KeychainReference{}
	committed := false
	defer func() {
		if !committed {
			for i := len(created) - 1; i >= 0; i-- {
				_ = store.Delete(context.Background(), created[i])
			}
		}
	}()
	ctx := context.Background()
	if !credentialRefEmpty(c.AccessTokenKeychain) {
		gatewayKey, err := store.Lookup(ctx, c.AccessTokenKeychain)
		if err != nil {
			return errors.New("gateway Keychain item unavailable")
		}
		if gatewayKey == apiKey {
			return errors.New("provider API key must differ from the gateway API key")
		}
	}
	if err := store.StoreNew(ctx, providerRef, apiKey); err != nil {
		return errors.New("cannot save provider API key in Keychain; unlock your login keychain and try again")
	}
	created = append(created, providerRef)
	storedProviderKey, err := store.Lookup(ctx, providerRef)
	if err != nil || storedProviderKey != apiKey {
		return errors.New("provider API key Keychain read-back verification failed")
	}
	if credentialRefEmpty(c.AccessTokenKeychain) {
		gatewayRef, err := newKeychainReference("com.tidemux.gateway")
		if err != nil {
			return err
		}
		credential, err := randomCredential()
		if err != nil {
			return err
		}
		defer zeroBytes(credential)
		if err := store.StoreNew(ctx, gatewayRef, string(credential)); err != nil {
			return errors.New("cannot save gateway credential in Keychain")
		}
		created = append(created, gatewayRef)
		storedGatewayKey, err := store.Lookup(ctx, gatewayRef)
		if err != nil || storedGatewayKey != string(credential) {
			return errors.New("gateway credential Keychain read-back verification failed")
		}
		c.AccessTokenKeychain = gatewayRef
	}
	if err := writeCommandConfig(path, c, before); err != nil {
		return err
	}
	committed = true
	return nil
}

func providerList(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("provider list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	jsonOutput := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("provider list takes no positional arguments")
	}
	c, err := gateway.LoadConfig(*path)
	if err != nil {
		return err
	}
	if c.BaseURL != "" {
		return errors.New("configuration uses the legacy single-provider format; run `tidemux provider add` to migrate it")
	}
	names := sortedProviderNames(c.Providers)
	if *jsonOutput {
		items := make([]map[string]any, 0, len(names))
		for _, name := range names {
			p := c.Providers[name]
			items = append(items, map[string]any{"ref": name, "endpoint": p.BaseURL, "protocol": p.Protocol, "default_model": p.Model, "supported_models": p.SupportedModels, "default": c.DefaultProviders[p.Protocol] == name})
		}
		return json.NewEncoder(stdout).Encode(items)
	}
	if len(names) == 0 {
		fmt.Fprintln(stdout, "No providers configured. Add one with `tidemux provider add`.")
		return nil
	}
	for _, name := range names {
		p := c.Providers[name]
		scope := "all models"
		if len(p.SupportedModels) > 0 {
			scope = strings.Join(p.SupportedModels, ",")
		}
		marker := ""
		if c.DefaultProviders[p.Protocol] == name {
			marker = " (default)"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\tmodel=%s\t%s%s\n", name, p.Protocol, p.BaseURL, p.Model, scope, marker)
	}
	return nil
}

func sortedProviderNames(providers map[string]gateway.Provider) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func providerShow(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	if ref == "" {
		return errors.New("usage: tidemux provider show REF [--config PATH]")
	}
	flags := flag.NewFlagSet("provider show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected provider show argument")
	}
	c, err := gateway.LoadConfig(*path)
	if err != nil {
		return err
	}
	p, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	scope := "all models"
	if len(p.SupportedModels) > 0 {
		scope = strings.Join(p.SupportedModels, ", ")
	}
	fmt.Fprintf(stdout, "Reference: %s\nProtocol: %s\nEndpoint: %s\nDefault model: %s\nModel scope: %s\nProtocol default: %t\n", ref, p.Protocol, p.BaseURL, p.Model, scope, c.DefaultProviders[p.Protocol] == ref)
	return nil
}

func providerDefault(args []string, stdout, stderr *os.File) error {
	protocol, rest := leadingEndpoint(args)
	ref, rest := leadingEndpoint(rest)
	if protocol != "openai" && protocol != "anthropic" || ref == "" {
		return errors.New("usage: tidemux provider default openai|anthropic REF [--config PATH]")
	}
	flags := flag.NewFlagSet("provider default", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected provider default argument")
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
	if p.Protocol == "" || p.Protocol == "auto" {
		return errors.New("provider protocol is unresolved; use `tidemux provider update REF --protocol openai|anthropic` first")
	}
	if p.Protocol != protocol {
		return fmt.Errorf("provider %q uses %s, not %s", ref, p.Protocol, protocol)
	}
	if c.DefaultProviders == nil {
		c.DefaultProviders = map[string]string{}
	}
	c.DefaultProviders[protocol] = ref
	if err := writeCommandConfig(abs, c, before); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Default %s provider: %s\n", protocol, ref)
	return nil
}

func providerModels(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	if ref == "" {
		return errors.New("usage: tidemux provider models REF [--only MODELS|--all] [--config PATH]")
	}
	flags := flag.NewFlagSet("provider models", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	only := flags.String("only", "", "comma-separated model allowlist")
	flags.Bool("all", false, "allow all models")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	onlyRequested := flagWasSet(flags, "only")
	allRequested := flagWasSet(flags, "all")
	if flags.NArg() != 0 || (allRequested && onlyRequested) {
		return errors.New("use at most one of --only or --all")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	p, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	if onlyRequested || allRequested {
		if onlyRequested {
			p.SupportedModels = parseCommaList(*only)
			if len(p.SupportedModels) == 0 {
				return errors.New("--only requires at least one model ID")
			}
		} else {
			p.SupportedModels = nil
		}
		if len(p.SupportedModels) > 0 && !containsConfiguredModel(p.SupportedModels, p.Model) {
			return fmt.Errorf("model allowlist must include default model %q", p.Model)
		}
		c.Providers[ref] = p
		if err := writeCommandConfig(abs, c, before); err != nil {
			return err
		}
		if allRequested {
			fmt.Fprintf(stdout, "Provider %s now allows all models.\n", ref)
		} else {
			fmt.Fprintf(stdout, "Provider %s model allowlist: %s\n", ref, strings.Join(p.SupportedModels, ", "))
		}
		return nil
	}
	if runtime.GOOS != "darwin" {
		return errors.New("model discovery requires macOS Keychain")
	}
	resolved, err := c.ResolveCredentials(context.Background(), gateway.MacOSKeychain{})
	if err != nil {
		return err
	}
	p = resolved.Providers[ref]
	var models []string
	var known bool
	if p.Protocol == "" || p.Protocol == "auto" {
		info, inspectErr := gateway.InspectProviderEndpoint(context.Background(), p.BaseURL, p.APIKey, p.APIVersion, nil)
		if inspectErr != nil {
			return errors.New("cannot determine provider protocol for model discovery")
		}
		models, known = info.Models, info.ModelsKnown
	} else {
		models, known = gateway.DiscoverProviderModels(p.BaseURL, p.APIKey, p.APIVersion, p.Protocol, nil)
	}
	if !known {
		return errors.New("provider does not expose a complete recognizable model list")
	}
	if len(p.SupportedModels) > 0 {
		models = intersectModels(models, p.SupportedModels)
	}
	for _, model := range models {
		fmt.Fprintln(stdout, model)
	}
	return nil
}

func parseCommaList(value string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range strings.Split(value, ",") {
		model := strings.TrimSpace(item)
		if model != "" && !seen[model] {
			seen[model] = true
			out = append(out, model)
		}
	}
	return out
}

func intersectModels(models, allow []string) []string {
	allowed := map[string]bool{}
	for _, item := range allow {
		allowed[item] = true
	}
	var out []string
	for _, item := range models {
		if allowed[item] {
			out = append(out, item)
		}
	}
	return out
}
