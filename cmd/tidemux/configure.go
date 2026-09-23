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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	preset := flags.String("preset", "", "optional preset: deepseek-flash")
	baseURL := flags.String("base-url", "", "API root including version prefix")
	openAIBaseURL := flags.String("openai-base-url", "", "OpenAI-compatible upstream API root")
	anthropicBaseURL := flags.String("anthropic-base-url", "", "Anthropic-compatible upstream API root")
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
		return errors.New("unknown preset; use --base-url or protocol-specific endpoint flags with --model")
	}
	protocolEndpointConfig := *openAIBaseURL != "" || *anthropicBaseURL != ""
	if *preset == "deepseek-flash" {
		if !protocolEndpointConfig && *baseURL == "" {
			*baseURL = "https://api.deepseek.com"
		}
		if *model == "" {
			*model = "deepseek-flash"
		}
	}
	if protocolEndpointConfig && *baseURL != "" {
		return errors.New("use --base-url or protocol-specific endpoint flags, not both")
	}
	if *model == "" || (!protocolEndpointConfig && *baseURL == "") {
		return errors.New("use configure --preset deepseek-flash, or provide --base-url/--model or protocol-specific endpoint flags/--model")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("configure requires macOS Keychain")
	}
	abs, err := filepath.Abs(*configPath)
	if err != nil {
		return errors.New("invalid config path")
	}
	pricingBaseURL := *baseURL
	if protocolEndpointConfig {
		pricingBaseURL = *openAIBaseURL
		if pricingBaseURL == "" {
			pricingBaseURL = *anthropicBaseURL
		}
	}
	prices, err := configurePrices(flags, *preset, pricingBaseURL, *model, *pricingCurrency, *pricingSource, *pricingVersion, *pricingInputCacheHit, *pricingInputCacheMiss, *pricingOutput)
	if err != nil {
		return err
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
	c := gateway.Config{Budget: budgetPolicy, ReportSchedule: schedule, Prices: prices, ModelCapabilities: gateway.ModelCapabilities{ContextTokens: *contextTokens, MaxOutputTokens: *outputTokens}, ListenAddr: *listen, BaseURL: *baseURL, Model: *model, UpstreamID: "provider-primary", MaxInFlight: *max, MaxActiveSessions: *maxSessions, ActiveSessionIdleTimeoutSeconds: *sessionIdleTimeout, LedgerPath: filepath.Join(filepath.Dir(abs), "ledger.db"), UpstreamKeychain: gateway.KeychainReference{Service: "pending", Account: "pending"}, AccessTokenKeychain: gateway.KeychainReference{Service: "pending", Account: "pending"}}
	if protocolEndpointConfig {
		c.BaseURL = ""
		c.UpstreamKeychain = gateway.KeychainReference{}
		c.Endpoints = make(map[string]gateway.ProviderEndpoint, 2)
		pending := gateway.KeychainReference{Service: "pending", Account: "pending"}
		if *anthropicVersion != "" && *anthropicBaseURL == "" {
			return errors.New("--anthropic-version requires --anthropic-base-url")
		}
		if *openAIBaseURL != "" {
			c.Endpoints["openai"] = gateway.ProviderEndpoint{BaseURL: *openAIBaseURL, UpstreamKeychain: pending}
		}
		if *anthropicBaseURL != "" {
			c.Endpoints["anthropic"] = gateway.ProviderEndpoint{BaseURL: *anthropicBaseURL, UpstreamKeychain: pending, APIVersion: *anthropicVersion}
		}
	} else if *anthropicVersion != "" {
		c.APIVersion = *anthropicVersion
	}
	if err = c.Validate(); err != nil {
		return err
	}
	if _, err = os.Lstat(abs); err == nil && !*replace {
		return errors.New("config already exists; use --replace to update it with new credentials")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return errors.New("run configure in an interactive terminal; API keys are never accepted as command arguments")
	}
	defer tty.Close()
	if !term.IsTerminal(int(tty.Fd())) {
		return errors.New("interactive terminal required")
	}
	if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
		return err
	}
	if protocolEndpointConfig {
		for _, protocol := range []string{"openai", "anthropic"} {
			if endpoint, ok := c.Endpoints[protocol]; ok {
				name := "OpenAI"
				if protocol == "anthropic" {
					name = "Anthropic"
				}
				fmt.Fprintf(stdout, "%s provider endpoint: %s\n", name, endpoint.BaseURL)
			}
		}
		fmt.Fprintf(stdout, "Provider routing: match the client protocol when an endpoint is configured; otherwise translate through the available endpoint\nModel: %s\nConfig: %s\n", c.Model, abs)
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
	if protocolEndpointConfig {
		var openAISecret []byte
		if _, ok := c.Endpoints["openai"]; ok {
			fmt.Fprint(tty, "OpenAI provider API key (hidden; paste then press Enter): ")
			openAISecret, err = term.ReadPassword(int(tty.Fd()))
			fmt.Fprintln(tty)
			if err != nil {
				return errors.New("could not read OpenAI provider API key")
			}
			defer func() {
				for i := range openAISecret {
					openAISecret[i] = 0
				}
			}()
			providerSecrets["openai"] = string(openAISecret)
		}
		if _, ok := c.Endpoints["anthropic"]; ok {
			prompt := "Anthropic provider API key (hidden; paste then press Enter): "
			if len(openAISecret) != 0 {
				prompt = "Anthropic provider API key (hidden; press Enter to reuse the OpenAI key): "
			}
			fmt.Fprint(tty, prompt)
			anthropicSecret, readErr := term.ReadPassword(int(tty.Fd()))
			fmt.Fprintln(tty)
			if readErr != nil {
				return errors.New("could not read Anthropic provider API key")
			}
			if len(anthropicSecret) == 0 && len(openAISecret) != 0 {
				providerSecrets["anthropic"] = providerSecrets["openai"]
			} else {
				providerSecrets["anthropic"] = string(anthropicSecret)
			}
			defer func() {
				for i := range anthropicSecret {
					anthropicSecret[i] = 0
				}
			}()
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
	fmt.Fprintln(stdout, "Local checks passed; no upstream request was sent. Pricing is stored with this provider profile.")
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
func saveConfiguration(path string, c gateway.Config, secret string, gatewaySecret []byte, replace bool, store secretWriter) error {
	return saveConfigurationWithProviderKeys(path, c, map[string]string{"legacy": secret}, gatewaySecret, replace, store)
}

type keychainEntry struct {
	reference gateway.KeychainReference
	secret    string
}

func saveConfigurationWithProviderKeys(path string, c gateway.Config, providerSecrets map[string]string, gatewaySecret []byte, replace bool, store secretWriter) error {
	if len(c.Endpoints) == 0 {
		if len(providerSecrets) != 1 || providerSecrets["legacy"] == "" {
			return errors.New("API key is empty or contains invalid characters")
		}
	} else if len(providerSecrets) != len(c.Endpoints) {
		return errors.New("an API key is required for each configured provider endpoint")
	}
	if len(c.Endpoints) > 0 {
		for protocol := range c.Endpoints {
			if _, ok := providerSecrets[protocol]; !ok {
				return fmt.Errorf("API key required for %s endpoint", protocol)
			}
		}
	}
	for name, secret := range providerSecrets {
		if strings.TrimSpace(secret) == "" || strings.ContainsAny(secret, "\r\n\x00") {
			return fmt.Errorf("%s API key is empty or contains invalid characters", name)
		}
		if len(c.Endpoints) == 0 && name != "legacy" {
			return errors.New("API key supplied for an unconfigured endpoint")
		}
		if len(c.Endpoints) > 0 {
			if _, ok := c.Endpoints[name]; !ok {
				return errors.New("API key supplied for an unconfigured endpoint")
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
	if len(c.Endpoints) == 0 {
		ref, err := newReference("com.tidemux.provider")
		if err != nil {
			return err
		}
		c.UpstreamKeychain = ref
		providerEntries = append(providerEntries, keychainEntry{reference: ref, secret: providerSecrets["legacy"]})
	} else {
		for _, protocol := range []string{"openai", "anthropic"} {
			endpoint, ok := c.Endpoints[protocol]
			if !ok {
				continue
			}
			secret := providerSecrets[protocol]
			ref, shared := sharedReferences[secret]
			if !shared {
				ref, err = newReference("com.tidemux.provider")
				if err != nil {
					return err
				}
				sharedReferences[secret] = ref
				providerEntries = append(providerEntries, keychainEntry{reference: ref, secret: secret})
			}
			endpoint.UpstreamKeychain = ref
			c.Endpoints[protocol] = endpoint
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
