package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
	"golang.org/x/term"
)

type secretWriter interface {
	gateway.SecretLookup
	StoreNew(context.Context, gateway.KeychainReference, string) error
	Delete(context.Context, gateway.KeychainReference) error
}

const defaultListenAddr = "127.0.0.1:4000"

type repeatedFlag []string

func (values *repeatedFlag) String() string { return strings.Join(*values, ",") }
func (values *repeatedFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func parseProviderFlag(value string) (string, gateway.Provider, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 3 && len(parts) != 4 {
		return "", gateway.Provider{}, errors.New("--provider must be NAME,BASE_URL,MODEL (automatic) or NAME,PROTOCOL,BASE_URL,MODEL (forced protocol)")
	}
	name, protocol := strings.TrimSpace(parts[0]), "auto"
	baseURL, model := strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
	if len(parts) == 4 {
		protocol = strings.ToLower(strings.TrimSpace(parts[1]))
		baseURL, model = strings.TrimSpace(parts[2]), strings.TrimSpace(parts[3])
	}
	return name, gateway.Provider{Protocol: protocol, BaseURL: baseURL, Model: model, UpstreamID: name}, nil
}

func parseDefaultProviderFlag(value string) (string, string, error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return "", "", errors.New("--default-provider must be PROTOCOL=NAME")
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func sortedProviderNames(providers map[string]gateway.Provider) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func formatDefaultProviderRoutes(routes map[string]string) string {
	ordered := make([]string, 0, len(routes))
	for _, protocol := range []string{"openai", "anthropic"} {
		if name, ok := routes[protocol]; ok {
			ordered = append(ordered, protocol+"="+name)
		}
	}
	return strings.Join(ordered, ", ")
}

type interactiveProviderSetup struct {
	Name     string
	Provider gateway.Provider
	APIKey   []byte
}

func providerNameFromBaseURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "provider"
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || net.ParseIP(host) != nil {
		return "local-provider"
	}
	labels := strings.Split(host, ".")
	for len(labels) > 1 && (labels[0] == "api" || labels[0] == "www") {
		labels = labels[1:]
	}
	candidate := labels[0]
	var slug strings.Builder
	lastHyphen := false
	for _, r := range candidate {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			slug.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && slug.Len() > 0 {
			slug.WriteByte('-')
			lastHyphen = true
		}
	}
	name := strings.Trim(slug.String(), "-")
	if name == "" {
		return "provider"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

func chooseProviderModel(in, out *os.File, models []string, modelsKnown bool) (string, error) {
	if modelsKnown {
		if len(models) == 1 {
			fmt.Fprintf(out, "Only one model was discovered; selecting %s.\n", models[0])
			return models[0], nil
		}
		if len(models) == 0 {
			return "", errors.New("the provider returned an empty model list")
		}
		fmt.Fprintln(out, "Available models:")
		const visibleModels = 20
		for i, model := range models {
			if i == visibleModels {
				fmt.Fprintf(out, "  ... and %d more (enter a model ID to use it)\n", len(models)-visibleModels)
				break
			}
			fmt.Fprintf(out, "  %2d. %s\n", i+1, model)
		}
		for {
			fmt.Fprint(out, "Default model (number or exact ID): ")
			value, err := readTerminalLine(in)
			if err != nil {
				return "", errors.New("could not read default model")
			}
			value = strings.TrimSpace(value)
			for _, model := range models {
				if value == model {
					return model, nil
				}
			}
			if index, parseErr := strconv.Atoi(value); parseErr == nil && index > 0 && index <= visibleModels && index <= len(models) {
				return models[index-1], nil
			}
			fmt.Fprintln(out, "Choose a listed model number or exact model ID.")
		}
	}

	fmt.Fprint(out, "Default model ID (not available from endpoint): ")
	model, err := readTerminalLine(in)
	if err != nil || strings.TrimSpace(model) == "" {
		return "", errors.New("a default model ID is required")
	}
	return strings.TrimSpace(model), nil
}

func promptProviderProtocol(in, out *os.File) (string, error) {
	for {
		fmt.Fprint(out, "Provider protocol could not be inferred. Enter openai or anthropic: ")
		value, err := readTerminalLine(in)
		if err != nil {
			return "", errors.New("could not read provider protocol")
		}
		protocol := strings.ToLower(strings.TrimSpace(value))
		if protocol == "openai" || protocol == "anthropic" {
			return protocol, nil
		}
		fmt.Fprintln(out, "Provider protocol must be openai or anthropic.")
	}
}

func promptNewProvider(in, out *os.File, anthropicVersion string, httpClient *http.Client) (interactiveProviderSetup, error) {
	var baseURL string
	for {
		fmt.Fprint(out, "Provider API Base URL: ")
		value, err := readTerminalLine(in)
		if err != nil {
			return interactiveProviderSetup{}, errors.New("could not read provider API Base URL")
		}
		baseURL = strings.TrimSpace(value)
		if baseURL == "" {
			fmt.Fprintln(out, "Provider API Base URL is required.")
			continue
		}
		if err := gateway.ValidateProviderBaseURL(baseURL); err != nil {
			fmt.Fprintf(out, "Invalid provider API Base URL: %v\n", err)
			continue
		}
		break
	}
	name := providerNameFromBaseURL(baseURL)
	fmt.Fprintf(out, "Provider name inferred from endpoint: %s\n", name)
	var apiKey []byte
	for {
		fmt.Fprint(out, "Provider API key (hidden): ")
		var err error
		apiKey, err = term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return interactiveProviderSetup{}, errors.New("could not read provider API key")
		}
		if strings.TrimSpace(string(apiKey)) != "" && !strings.ContainsAny(string(apiKey), "\r\n\x00") {
			break
		}
		for i := range apiKey {
			apiKey[i] = 0
		}
		fmt.Fprintln(out, "A valid provider API key is required.")
	}

	var err error
	info, inspectErr := gateway.InspectProviderEndpoint(context.Background(), baseURL, string(apiKey), anthropicVersion, httpClient)
	protocol := ""
	models := []string(nil)
	modelsKnown := false
	if inspectErr != nil {
		fmt.Fprintf(out, "Automatic protocol detection was unavailable: %v\n", inspectErr)
		protocol, err = promptProviderProtocol(in, out)
		if err != nil {
			for i := range apiKey {
				apiKey[i] = 0
			}
			return interactiveProviderSetup{}, err
		}
		models, modelsKnown = gateway.DiscoverProviderModels(baseURL, string(apiKey), anthropicVersion, protocol, httpClient)
	} else {
		protocol, models, modelsKnown = info.Protocol, info.Models, info.ModelsKnown
		fmt.Fprintf(out, "Detected upstream protocol: %s\n", protocol)
		if !modelsKnown {
			fmt.Fprintln(out, "Model discovery is unavailable; the model ID will be entered manually.")
		}
	}
	model, err := chooseProviderModel(in, out, models, modelsKnown)
	if err != nil {
		for i := range apiKey {
			apiKey[i] = 0
		}
		return interactiveProviderSetup{}, err
	}
	version := ""
	if protocol == "anthropic" {
		version = anthropicVersion
	}
	return interactiveProviderSetup{
		Name: name,
		Provider: gateway.Provider{
			Protocol: protocol, BaseURL: baseURL, APIVersion: version,
			Model: model, UpstreamID: name,
		},
		APIKey: apiKey,
	}, nil
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "tidemux.json"
	}
	return filepath.Join(home, "Library", "Application Support", "TideMux", "config.json")
}
func configure(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("configure", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: tidemux configure [flags]")
		fmt.Fprintln(stderr, "Without --provider, --base-url, --model or --preset, starts guided setup.")
		flags.PrintDefaults()
	}
	preset := flags.String("preset", "", "optional preset: deepseek-flash")
	baseURL := flags.String("base-url", "", "API root including version prefix")
	var providerFlags, defaultProviderFlags repeatedFlag
	flags.Var(&providerFlags, "provider", "named provider NAME,BASE_URL,MODEL (auto) or NAME,PROTOCOL,BASE_URL,MODEL (forced; repeatable)")
	flags.Var(&defaultProviderFlags, "default-provider", "default route PROTOCOL=NAME (repeatable)")
	anthropicVersion := flags.String("anthropic-version", "", "Anthropic API version (default: 2023-06-01)")
	model := flags.String("model", "", "default model ID")
	configPath := flags.String("config", defaultConfigPath(), "configuration path")
	listen := flags.String("listen", defaultListenAddr, "IP:port (loopback by default)")
	max := flags.Int("max-in-flight", 1, "maximum simultaneous upstream requests")
	maxSessions := flags.Int("max-active-sessions", 0, "maximum active logical sessions; zero disables the limit")
	sessionIdleTimeout := flags.Int("active-session-idle-timeout-seconds", 0, "idle time before releasing a retained session in seconds; zero uses the five-minute default")
	contextTokens := flags.Int64("context-tokens", 0, "verified upstream context window; zero means unknown")
	outputTokens := flags.Int64("output-tokens", 0, "verified upstream output ceiling; zero means unknown")
	budget5h := flags.Float64("budget-5h", 0, "rolling five-hour budget in the pricing currency; zero disables it")
	budgetWeekly := flags.Float64("budget-weekly", 0, "rolling seven-day budget in the pricing currency; zero disables it")
	budgetCurrency := flags.String("budget-currency", "USD", "budget currency")
	budgetMode := flags.String("budget-mode", "hard", "budget mode: alert, soft or hard")
	budgetThreshold := flags.Float64("budget-alert-threshold", 0.8, "budget alert threshold from 0 to 1")
	notificationTime := flags.String("notification-time", "", "daily report notification time in local time (HH:MM); empty disables it")
	pricingCurrency := flags.String("pricing-currency", "USD", "pricing currency")
	pricingSource := flags.String("pricing-source", "manual-cli", "pricing source or provider reference")
	pricingVersion := flags.String("pricing-version", "manual", "pricing version or verification date")
	pricingInputCacheHit := flags.Float64("pricing-input-cache-hit", 0, "cache-hit input price per million tokens")
	pricingInputCacheMiss := flags.Float64("pricing-input-cache-miss", 0, "cache-miss input price per million tokens")
	pricingOutput := flags.Float64("pricing-output", 0, "output price per million tokens")
	replace := flags.Bool("replace", false, "replace configuration using new Keychain references (old credentials retained)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected configure argument")
	}
	if *preset != "" && *preset != "deepseek-flash" {
		return errors.New("unknown preset; use --base-url")
	}
	interactiveMode := len(providerFlags) == 0 && *preset == "" && !flagWasSet(flags, "base-url", "model")
	namedProviderConfig := len(providerFlags) != 0 || interactiveMode
	if *preset == "deepseek-flash" {
		if namedProviderConfig {
			return errors.New("--preset cannot be combined with named --provider entries")
		}
		if *baseURL == "" {
			*baseURL = "https://api.deepseek.com"
		}
		if *model == "" {
			*model = "deepseek-flash"
		}
	}
	if interactiveMode && len(defaultProviderFlags) != 0 {
		return errors.New("--default-provider requires command-line --provider entries")
	}
	if namedProviderConfig && !interactiveMode {
		if *baseURL != "" || *model != "" {
			return errors.New("use either named --provider entries or the legacy --base-url/--model options")
		}
	} else if !interactiveMode && (*model == "" || *baseURL == "") {
		return errors.New("use configure --preset deepseek-flash, provide --base-url/--model, or define named --provider entries")
	}
	if !namedProviderConfig && len(defaultProviderFlags) != 0 {
		return errors.New("--default-provider requires named --provider entries")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("configure requires macOS Keychain")
	}
	abs, err := filepath.Abs(*configPath)
	if err != nil {
		return errors.New("invalid config path")
	}
	if _, err = os.Lstat(abs); err == nil && !*replace {
		return errors.New("config already exists; use --replace to update it with new credentials")
	}
	var tty *os.File
	var wizardProvider *interactiveProviderSetup
	if interactiveMode {
		tty, err = os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return errors.New("run configure in an interactive terminal; provider API keys are never accepted as command arguments")
		}
		defer tty.Close()
		if !term.IsTerminal(int(tty.Fd())) {
			return errors.New("interactive terminal required")
		}
		setup, setupErr := promptNewProvider(tty, tty, *anthropicVersion, nil)
		if setupErr != nil {
			return setupErr
		}
		wizardProvider = &setup
		defer func() {
			for i := range wizardProvider.APIKey {
				wizardProvider.APIKey[i] = 0
			}
		}()
	}
	prices := map[string]adapter.Price{}
	if !namedProviderConfig {
		prices, err = configurePrices(flags, *preset, *baseURL, *model, *pricingCurrency, *pricingSource, *pricingVersion, *pricingInputCacheHit, *pricingInputCacheMiss, *pricingOutput)
		if err != nil {
			return err
		}
	}
	budgetPolicy := ledger.BudgetPolicy{}
	if *budget5h > 0 || *budgetWeekly > 0 {
		budgetPolicy = ledger.BudgetPolicy{Currency: *budgetCurrency, FiveHourLimit: *budget5h, WeeklyLimit: *budgetWeekly, AlertThreshold: *budgetThreshold, Mode: *budgetMode}
	}
	schedule := gateway.ReportSchedule{}
	if flagWasSet(flags, "notification-time") && strings.TrimSpace(*notificationTime) != "" {
		normalized, err := gateway.NormalizeReportScheduleTime(*notificationTime)
		if err != nil {
			return err
		}
		schedule = gateway.ReportSchedule{Time: normalized, Channel: "macos"}
	}
	capabilities := gateway.ModelCapabilities{ContextTokens: *contextTokens, MaxOutputTokens: *outputTokens}
	c := gateway.Config{Budget: budgetPolicy, ReportSchedule: schedule, Prices: prices, ModelCapabilities: capabilities, ListenAddr: *listen, BaseURL: *baseURL, Model: *model, UpstreamID: "provider-primary", MaxInFlight: *max, MaxActiveSessions: *maxSessions, ActiveSessionIdleTimeoutSeconds: *sessionIdleTimeout, LedgerPath: filepath.Join(filepath.Dir(abs), "ledger.db"), UpstreamKeychain: gateway.KeychainReference{Service: "pending", Account: "pending"}, AccessTokenKeychain: gateway.KeychainReference{Service: "pending", Account: "pending"}}
	if namedProviderConfig {
		c.BaseURL = ""
		c.UpstreamKeychain = gateway.KeychainReference{}
		c.Model = ""
		c.UpstreamID = ""
		c.Prices = nil
		c.ModelCapabilities = gateway.ModelCapabilities{}
		providerCount := len(providerFlags)
		if wizardProvider != nil {
			providerCount++
		}
		c.Providers = make(map[string]gateway.Provider, providerCount)
		c.DefaultProviders = make(map[string]string, len(defaultProviderFlags))
		pending := gateway.KeychainReference{Service: "pending", Account: "pending"}
		providersToConfigure := make(map[string]gateway.Provider, providerCount)
		if wizardProvider != nil {
			providersToConfigure[wizardProvider.Name] = wizardProvider.Provider
		}
		for _, value := range providerFlags {
			name, provider, parseErr := parseProviderFlag(value)
			if parseErr != nil {
				return parseErr
			}
			if _, exists := providersToConfigure[name]; exists {
				return fmt.Errorf("duplicate provider name %q", name)
			}
			providersToConfigure[name] = provider
		}
		for name, provider := range providersToConfigure {
			if _, exists := c.Providers[name]; exists {
				return fmt.Errorf("duplicate provider name %q", name)
			}
			if provider.Protocol == "anthropic" || provider.Protocol == "auto" {
				provider.APIVersion = *anthropicVersion
			}
			provider.UpstreamKeychain = pending
			provider.ModelCapabilities = capabilities
			var providerPrices map[string]adapter.Price
			var priceErr error
			if wizardProvider != nil && name == wizardProvider.Name {
				providerPrices, priceErr = configureWizardPrices(flags, provider.BaseURL, provider.Model, *pricingCurrency, *pricingSource, *pricingVersion, *pricingInputCacheHit, *pricingInputCacheMiss, *pricingOutput)
			} else {
				providerPrices, priceErr = configurePrices(flags, "", provider.BaseURL, provider.Model, *pricingCurrency, *pricingSource, *pricingVersion, *pricingInputCacheHit, *pricingInputCacheMiss, *pricingOutput)
			}
			if priceErr != nil {
				return fmt.Errorf("provider %s: %w", name, priceErr)
			}
			provider.Prices = providerPrices
			c.Providers[name] = provider
		}
		for _, value := range defaultProviderFlags {
			protocol, name, parseErr := parseDefaultProviderFlag(value)
			if parseErr != nil {
				return parseErr
			}
			if _, exists := c.DefaultProviders[protocol]; exists {
				return fmt.Errorf("duplicate default provider for %s", protocol)
			}
			c.DefaultProviders[protocol] = name
		}
	} else if *anthropicVersion != "" {
		c.APIVersion = *anthropicVersion
	}
	if err = c.Validate(); err != nil {
		return err
	}
	if tty == nil {
		tty, err = os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return errors.New("run configure in an interactive terminal; API keys are never accepted as command arguments")
		}
		defer tty.Close()
		if !term.IsTerminal(int(tty.Fd())) {
			return errors.New("interactive terminal required")
		}
	}
	if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
		return err
	}
	if namedProviderConfig {
		for _, name := range sortedProviderNames(c.Providers) {
			provider := c.Providers[name]
			fmt.Fprintf(stdout, "Provider %s (%s): %s, model %s\n", name, provider.Protocol, provider.BaseURL, provider.Model)
		}
		if wizardProvider != nil {
			fmt.Fprintf(stdout, "Default route: inferred from provider protocol\nConfig: %s\n", abs)
		} else {
			fmt.Fprintf(stdout, "Default provider routes: %s\nConfig: %s\n", formatDefaultProviderRoutes(c.DefaultProviders), abs)
		}
	} else {
		fmt.Fprintf(stdout, "Provider API root: %s\nProvider protocol: automatic (checked when the gateway starts)\nModel: %s\nConfig: %s\n", c.BaseURL, c.Model, abs)
	}
	if !isLoopbackListenAddr(c.ListenAddr) {
		fmt.Fprintf(stdout, "WARNING: external gateway access is enabled on %s; protect the network and gateway token.\n", c.ListenAddr)
	}
	if c.Budget != (ledger.BudgetPolicy{}) {
		fmt.Fprintf(stdout, "Budget: %g %s / 5h, %g %s / 7d (%s mode)\n", c.Budget.FiveHourLimit, c.Budget.Currency, c.Budget.WeeklyLimit, c.Budget.Currency, c.Budget.Mode)
	}
	if !flagWasSet(flags, "notification-time") {
		schedule, err = promptReportSchedule(tty, tty)
		if err != nil {
			return err
		}
		c.ReportSchedule = schedule
		if err := c.Validate(); err != nil {
			return err
		}
	}
	if c.ReportSchedule != (gateway.ReportSchedule{}) {
		fmt.Fprintf(stdout, "Daily report notification: %s (%s, local time)\n", c.ReportSchedule.Time, c.ReportSchedule.EffectiveChannel())
	} else {
		fmt.Fprintln(stdout, "Daily report notification: disabled")
	}
	providerSecrets := map[string]string{}
	if namedProviderConfig {
		for _, name := range sortedProviderNames(c.Providers) {
			if wizardProvider != nil && name == wizardProvider.Name {
				providerSecrets[name] = string(wizardProvider.APIKey)
				continue
			}
			protocol := c.Providers[name].Protocol
			fmt.Fprintf(tty, "Provider %q (%s) API key (hidden; paste then press Enter): ", name, protocol)
			secret, readErr := term.ReadPassword(int(tty.Fd()))
			fmt.Fprintln(tty)
			if readErr != nil {
				return fmt.Errorf("could not read API key for provider %q", name)
			}
			defer func(secret []byte) {
				for i := range secret {
					secret[i] = 0
				}
			}(secret)
			providerSecrets[name] = string(secret)
		}
	} else {
		fmt.Fprint(tty, "API key (hidden; paste then press Enter): ")
		secret, readErr := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		if readErr != nil {
			return errors.New("could not read API key")
		}
		defer func() {
			for i := range secret {
				secret[i] = 0
			}
		}()
		providerSecrets["legacy"] = string(secret)
	}
	fmt.Fprint(tty, "Gateway API key (hidden; press Enter to generate randomly): ")
	gatewaySecret, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return errors.New("could not read gateway API key")
	}
	defer func() {
		for i := range gatewaySecret {
			gatewaySecret[i] = 0
		}
	}()
	generatedGatewaySecret := len(gatewaySecret) == 0
	if err = saveConfigurationWithProviderKeys(abs, c, providerSecrets, gatewaySecret, *replace, gateway.MacOSKeychain{}); err != nil {
		return err
	}
	if _, err := syncReportSchedule(abs, c.ReportSchedule); err != nil {
		return fmt.Errorf("configuration saved, but scheduled notification setup failed: %w", err)
	}
	if generatedGatewaySecret {
		fmt.Fprintln(stdout, "Configured. Provider API key(s) and randomly generated gateway API key are stored in macOS Keychain.")
	} else {
		fmt.Fprintln(stdout, "Configured. Provider API key(s) and custom gateway API key are stored in macOS Keychain.")
	}
	if *replace {
		fmt.Fprintln(stdout, "If a config was replaced, its exact backup is beside it as <config>.backup-<id>; old Keychain items are retained.")
	}
	if wizardProvider != nil {
		fmt.Fprintln(stdout, "Protocol and model discovery used GET /models only; no completion request was sent.")
		if len(c.Providers[wizardProvider.Name].Prices) == 0 {
			fmt.Fprintln(stdout, "No verified price is configured; cost estimates will remain unknown until pricing is added.")
		}
	} else {
		fmt.Fprintln(stdout, "Local checks passed; no upstream request was sent. Pricing is stored with this provider profile.")
	}
	defaultPath, _ := filepath.Abs(defaultConfigPath())
	if abs == defaultPath {
		fmt.Fprintln(stdout, "Next: tidemux serve\nInspect: tidemux billing")
	} else {
		fmt.Fprintf(stdout, "Next: tidemux serve --config %q\n", abs)
	}
	return nil
}

func configurePrices(flags *flag.FlagSet, preset, baseURL, model, currency, source, version string, inputCacheHit, inputCacheMiss, output float64) (map[string]adapter.Price, error) {
	prices := map[string]adapter.Price{}
	if preset == "deepseek-flash" {
		if price, ok := adapter.BuiltInPrice(baseURL, model, time.Now()); ok {
			prices[model] = price
		}
	}
	pricingFlags := []string{"pricing-currency", "pricing-source", "pricing-version", "pricing-input-cache-hit", "pricing-input-cache-miss", "pricing-output"}
	if !flagWasSet(flags, pricingFlags...) {
		if len(prices) == 0 {
			return nil, errors.New("pricing is required; use --pricing-input-cache-hit, --pricing-input-cache-miss and --pricing-output")
		}
		return prices, nil
	}
	if !flagWasSet(flags, "pricing-input-cache-hit") || !flagWasSet(flags, "pricing-input-cache-miss") || !flagWasSet(flags, "pricing-output") {
		return nil, errors.New("custom pricing requires --pricing-input-cache-hit, --pricing-input-cache-miss and --pricing-output")
	}
	price := adapter.Price{
		Currency:       currency,
		Source:         source,
		Version:        version,
		InputCacheHit:  &inputCacheHit,
		InputCacheMiss: &inputCacheMiss,
		Output:         &output,
	}
	if err := price.Validate(); err != nil {
		return nil, fmt.Errorf("invalid custom pricing: %w", err)
	}
	prices[model] = price
	return prices, nil
}

func configureWizardPrices(flags *flag.FlagSet, baseURL, model, currency, source, version string, inputCacheHit, inputCacheMiss, output float64) (map[string]adapter.Price, error) {
	pricingFlags := []string{"pricing-currency", "pricing-source", "pricing-version", "pricing-input-cache-hit", "pricing-input-cache-miss", "pricing-output"}
	if flagWasSet(flags, pricingFlags...) {
		return configurePrices(flags, "", baseURL, model, currency, source, version, inputCacheHit, inputCacheMiss, output)
	}
	if price, ok := adapter.BuiltInPrice(baseURL, model, time.Now()); ok {
		return map[string]adapter.Price{model: price}, nil
	}
	return map[string]adapter.Price{}, nil
}

func saveConfiguration(path string, c gateway.Config, secret string, gatewaySecret []byte, replace bool, store secretWriter) error {
	return saveConfigurationWithProviderKeys(path, c, map[string]string{"legacy": secret}, gatewaySecret, replace, store)
}

type keychainEntry struct {
	reference gateway.KeychainReference
	secret    string
}

func saveConfigurationWithProviderKeys(path string, c gateway.Config, providerSecrets map[string]string, gatewaySecret []byte, replace bool, store secretWriter) error {
	if len(c.Providers) == 0 {
		if len(providerSecrets) != 1 || providerSecrets["legacy"] == "" {
			return errors.New("API key is empty or contains invalid characters")
		}
	} else if len(providerSecrets) != len(c.Providers) {
		return errors.New("an API key is required for each configured provider")
	}
	if len(c.Providers) > 0 {
		for name := range c.Providers {
			if _, ok := providerSecrets[name]; !ok {
				return fmt.Errorf("API key required for provider %q", name)
			}
		}
	}
	for name, secret := range providerSecrets {
		if strings.TrimSpace(secret) == "" || strings.ContainsAny(secret, "\r\n\x00") {
			return fmt.Errorf("%s API key is empty or contains invalid characters", name)
		}
		if len(c.Providers) == 0 && name != "legacy" {
			return errors.New("API key supplied for an unconfigured provider")
		}
		if len(c.Providers) > 0 {
			if _, ok := c.Providers[name]; !ok {
				return errors.New("API key supplied for an unconfigured provider")
			}
		}
	}
	gatewayKey := string(gatewaySecret)
	for _, secret := range providerSecrets {
		if len(gatewaySecret) > 0 && gatewayKey == secret {
			return errors.New("gateway API key must differ from every provider API key")
		}
	}
	if len(gatewaySecret) > 0 && (strings.TrimSpace(gatewayKey) == "" || strings.ContainsAny(gatewayKey, "\r\n\x00")) {
		return errors.New("gateway API key is empty, invalid, or identical to the provider API key")
	}
	if _, err := os.Lstat(path); err == nil && !replace {
		return errors.New("config already exists")
	}
	var previous []byte
	previousExists := false
	if replace {
		var err error
		previous, err = os.ReadFile(path)
		if err == nil {
			previousExists = true
		} else if !os.IsNotExist(err) {
			return errors.New("cannot read existing config for backup")
		}
	}
	newReference := func(service string) (gateway.KeychainReference, error) {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return gateway.KeychainReference{}, errors.New("cannot generate credential reference")
		}
		return gateway.KeychainReference{Service: service, Account: hex.EncodeToString(nonce)}, nil
	}
	accessReference, err := newReference("com.tidemux.gateway")
	if err != nil {
		return err
	}
	providerEntries := make([]keychainEntry, 0, len(providerSecrets))
	sharedReferences := make(map[string]gateway.KeychainReference, len(providerSecrets))
	if len(c.Providers) == 0 {
		ref, err := newReference("com.tidemux.provider")
		if err != nil {
			return err
		}
		c.UpstreamKeychain = ref
		providerEntries = append(providerEntries, keychainEntry{reference: ref, secret: providerSecrets["legacy"]})
	} else {
		for _, name := range sortedProviderNames(c.Providers) {
			provider := c.Providers[name]
			secret := providerSecrets[name]
			ref, shared := sharedReferences[secret]
			if !shared {
				ref, err = newReference("com.tidemux.provider")
				if err != nil {
					return err
				}
				sharedReferences[secret] = ref
				providerEntries = append(providerEntries, keychainEntry{reference: ref, secret: secret})
			}
			provider.UpstreamKeychain = ref
			c.Providers[name] = provider
		}
	}
	gatewayCredential := gatewayKey
	if len(gatewaySecret) == 0 {
		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			return errors.New("cannot generate gateway token")
		}
		defer func() {
			for i := range token {
				token[i] = 0
			}
		}()
		gatewayCredential = hex.EncodeToString(token)
	}
	c.AccessTokenKeychain = accessReference
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("cannot create configuration directory")
	}
	if err := os.MkdirAll(filepath.Dir(c.LedgerPath), 0700); err != nil {
		return errors.New("cannot create ledger directory")
	}
	if err := checkLedgerParent(c.LedgerPath); err != nil {
		return errors.New("ledger directory is not writable")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tidemux-config-*")
	if err != nil {
		return errors.New("cannot prepare config file")
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if _, err = tmp.Write(append(data, '\n')); err != nil {
		return errors.New("cannot prepare config")
	}
	if err = tmp.Sync(); err != nil {
		return errors.New("cannot sync config")
	}
	if err = tmp.Close(); err != nil {
		return errors.New("cannot close config")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	created := []gateway.KeychainReference{}
	committed := false
	defer func() {
		if !committed {
			cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			for _, ref := range created {
				store.Delete(cleanup, ref)
			}
		}
	}()
	for _, entry := range providerEntries {
		if err = store.StoreNew(ctx, entry.reference, entry.secret); err != nil {
			return errors.New("cannot save provider API key in Keychain; unlock your login keychain and try again")
		}
		created = append(created, entry.reference)
	}
	if err = store.StoreNew(ctx, c.AccessTokenKeychain, gatewayCredential); err != nil {
		return errors.New("cannot save local gateway credential")
	}
	created = append(created, c.AccessTokenKeychain)
	if _, err = c.ResolveCredentials(ctx, store); err != nil {
		return errors.New("Keychain read-back verification failed")
	}
	if replace && previousExists {
		current, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(current, previous) {
			return errors.New("config changed during setup; existing config was not replaced")
		}
		backup, backupErr := os.OpenFile(path+".backup-"+accessReference.Account, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if backupErr != nil {
			return errors.New("cannot create config backup; existing config was not replaced")
		}
		_, writeErr := backup.Write(previous)
		syncErr := backup.Sync()
		closeErr := backup.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			os.Remove(backup.Name())
			return errors.New("cannot save config backup; existing config was not replaced")
		}
		err = os.Rename(tmp.Name(), path)
	} else {
		err = os.Link(tmp.Name(), path)
	}
	if err != nil {
		return errors.New("could not install configuration; existing config was not replaced")
	}
	committed = true
	return nil
}

// The system utility reads the login password directly from the controlling TTY.
// Never use -p, capture its output, or read that password in TideMux.
func unlockKeychainIfNeeded(ctx context.Context, tty *os.File) error {
	if _, err := exec.CommandContext(ctx, "security", "show-keychain-info").CombinedOutput(); err == nil {
		return nil
	}
	fmt.Fprintln(tty, "Your login keychain is locked. macOS will ask for your keychain password (usually your Mac login password, not your API key). Input is hidden. Press Control-C to cancel.")
	command := exec.CommandContext(ctx, "security", "unlock-keychain")
	command.Stdin, command.Stdout, command.Stderr = tty, tty, tty
	if err := command.Run(); err != nil {
		return errors.New("keychain unlock failed or was canceled; no API key was requested and no configuration was changed")
	}
	if _, err := exec.CommandContext(ctx, "security", "show-keychain-info").CombinedOutput(); err != nil {
		return errors.New("keychain is still unavailable after unlock; no configuration was changed")
	}
	fmt.Fprintln(tty, "Keychain unlocked. Continuing setup.")
	return nil
}
