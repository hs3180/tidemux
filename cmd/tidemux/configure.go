package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
	"golang.org/x/term"
)

const defaultListenAddr = "127.0.0.1:4000"

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
		fmt.Fprintln(stderr, "Setup modes:")
		fmt.Fprintln(stderr, "  Guided:  tidemux configure")
		fmt.Fprintln(stderr, "  Named:   --provider NAME,BASE_URL,MODEL (repeatable)")
		fmt.Fprintln(stderr, "           or NAME,PROTOCOL,BASE_URL,MODEL to force the protocol")
		fmt.Fprintln(stderr, "  Legacy:  --base-url URL --model ID")
		fmt.Fprintln(stderr, "Named providers support all models by default; use --provider-models to restrict the list.")
		flags.PrintDefaults()
	}
	baseURL := flags.String("base-url", "", "API root including version prefix")
	var providerFlags, providerModelFlags, defaultProviderFlags repeatedFlag
	flags.Var(&providerFlags, "provider", "named provider NAME,BASE_URL,MODEL (auto) or NAME,PROTOCOL,BASE_URL,MODEL (forced; repeatable)")
	flags.Var(&providerModelFlags, "provider-models", "restrict a named provider to MODEL IDs: NAME,MODEL[,MODEL...] (repeatable; include its default model; omit for all)")
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
	if len(providerModelFlags) > 0 && len(providerFlags) == 0 {
		return errors.New("--provider-models requires command-line --provider entries")
	}
	interactiveMode := len(providerFlags) == 0 && !flagWasSet(flags, "base-url", "model")
	namedProviderConfig := len(providerFlags) != 0 || interactiveMode
	if interactiveMode && len(defaultProviderFlags) != 0 {
		return errors.New("--default-provider requires command-line --provider entries")
	}
	if namedProviderConfig && !interactiveMode {
		if *baseURL != "" || *model != "" {
			return errors.New("use either named --provider entries or the legacy --base-url/--model options")
		}
	} else if !interactiveMode && (*model == "" || *baseURL == "") {
		return errors.New("provide both --base-url and --model, or define named --provider entries")
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
		prices, err = configurePrices(flags, *baseURL, *model, *pricingCurrency, *pricingSource, *pricingVersion, *pricingInputCacheHit, *pricingInputCacheMiss, *pricingOutput)
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
		configuredModelScopes := make(map[string]struct{}, len(providerModelFlags))
		for _, value := range providerModelFlags {
			name, models, parseErr := parseProviderModelsFlag(value)
			if parseErr != nil {
				return parseErr
			}
			provider, exists := providersToConfigure[name]
			if !exists {
				return fmt.Errorf("--provider-models references unconfigured provider %q", name)
			}
			if _, duplicate := configuredModelScopes[name]; duplicate {
				return fmt.Errorf("--provider-models was specified more than once for provider %q", name)
			}
			configuredModelScopes[name] = struct{}{}
			provider.SupportedModels = models
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
				providerPrices, priceErr = configurePrices(flags, provider.BaseURL, provider.Model, *pricingCurrency, *pricingSource, *pricingVersion, *pricingInputCacheHit, *pricingInputCacheMiss, *pricingOutput)
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
			scope := "all models"
			if len(provider.SupportedModels) > 0 {
				scope = strings.Join(provider.SupportedModels, ", ")
			}
			fmt.Fprintf(stdout, "Provider %s (%s): %s, default model %s, supported models %s\n", name, provider.Protocol, provider.BaseURL, provider.Model, scope)
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
