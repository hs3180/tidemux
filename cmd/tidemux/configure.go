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
	preset := flags.String("preset", "", "optional preset: deepseek")
	protocol := flags.String("protocol", "openai", "openai or anthropic")
	baseURL := flags.String("base-url", "", "API root including version prefix")
	model := flags.String("model", "", "default model ID")
	configPath := flags.String("config", defaultConfigPath(), "configuration path")
	listen := flags.String("listen", "127.0.0.1:8787", "loopback IP:port; use different ports for simultaneous protocol profiles")
	max := flags.Int("max-in-flight", 1, "maximum simultaneous upstream requests")
	contextTokens := flags.Int64("context-tokens", 0, "verified upstream context window; zero means unknown")
	outputTokens := flags.Int64("output-tokens", 0, "verified upstream output ceiling; zero means unknown")
	budget := flags.Float64("budget", 0, "daily and monthly budget in the pricing currency; zero disables budget")
	budgetCurrency := flags.String("budget-currency", "USD", "budget currency")
	budgetTimezone := flags.String("budget-timezone", "UTC", "IANA timezone used for budget periods")
	budgetMode := flags.String("budget-mode", "hard", "budget mode: alert, soft or hard")
	budgetThreshold := flags.Float64("budget-alert-threshold", 0.8, "budget alert threshold from 0 to 1")
	budgetReserve := flags.Float64("budget-reserve", 0, "worst-case reservation per request; defaults to --budget")
	pricingFile := flags.String("pricing-file", "", "JSON pricing fragment containing a prices object")
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
	if *preset != "" && *preset != "deepseek" {
		return errors.New("unknown preset; use --base-url and --model for any compatible provider")
	}
	if *preset == "deepseek" {
		if *baseURL == "" {
			*baseURL = "https://api.deepseek.com"
			if *protocol == "anthropic" {
				*baseURL = "https://api.deepseek.com/anthropic/v1"
			}
		}
		if *model == "" {
			*model = "deepseek-flash"
		}
	}
	if *baseURL == "" || *model == "" {
		return errors.New("use configure --preset deepseek, or provide --protocol, --base-url and --model")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("configure requires macOS Keychain")
	}
	abs, err := filepath.Abs(*configPath)
	if err != nil {
		return errors.New("invalid config path")
	}
	prices, err := loadPricingFile(*pricingFile)
	if err != nil {
		return err
	}
	budgetPolicy := ledger.BudgetPolicy{}
	if *budget > 0 {
		reserve := *budgetReserve
		if reserve == 0 {
			reserve = *budget
		}
		budgetPolicy = ledger.BudgetPolicy{Currency: *budgetCurrency, Timezone: *budgetTimezone, DailyLimit: *budget, MonthlyLimit: *budget, AlertThreshold: *budgetThreshold, Mode: *budgetMode, ReserveAmount: reserve}
	} else if *budgetReserve > 0 {
		return errors.New("--budget-reserve requires --budget")
	}
	c := gateway.Config{Budget: budgetPolicy, Prices: prices, ModelCapabilities: gateway.ModelCapabilities{ContextTokens: *contextTokens, MaxOutputTokens: *outputTokens}, ListenAddr: *listen, Protocol: *protocol, BaseURL: *baseURL, Model: *model, UpstreamID: *protocol + "-primary", APIVersion: "2023-06-01", MaxInFlight: *max, LedgerPath: filepath.Join(filepath.Dir(abs), "ledger.db"), UpstreamKeychain: gateway.KeychainReference{Service: "pending", Account: "pending"}, AccessTokenKeychain: gateway.KeychainReference{Service: "pending", Account: "pending"}}
	if err = c.Validate(); err != nil {
		return err
	}
	if _, err = os.Lstat(abs); err == nil && !*replace {
		return errors.New("config already exists; use --replace to create new credentials or --config for another profile")
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
	fmt.Fprintf(stdout, "Protocol: %s\nAPI root: %s\nModel: %s\nConfig: %s\n", c.Protocol, c.BaseURL, c.Model, abs)
	if c.Budget != (ledger.BudgetPolicy{}) {
		fmt.Fprintf(stdout, "Budget: %g %s daily/monthly (%s mode)\n", c.Budget.DailyLimit, c.Budget.Currency, c.Budget.Mode)
	}
	fmt.Fprint(tty, "API key (hidden; paste then press Enter): ")
	secret, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return errors.New("could not read API key")
	}
	defer func() {
		for i := range secret {
			secret[i] = 0
		}
	}()
	if err = saveConfiguration(abs, c, string(secret), *replace, gateway.MacOSKeychain{}); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Configured. API key and generated gateway token are stored in macOS Keychain.")
	if *replace {
		fmt.Fprintln(stdout, "If a config was replaced, its exact backup is beside it as <config>.backup-<id>; old Keychain items are retained.")
	}
	fmt.Fprintln(stdout, "Local checks passed; no upstream request was sent. Prices remain unknown until configured.")
	fmt.Fprintf(stdout, "Next: tidemux serve --config %q\nInspect: tidemux ledger --config %q\n", abs, abs)
	return nil
}

func loadPricingFile(path string) (map[string]adapter.Price, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read pricing file")
	}
	var fragment struct {
		Prices map[string]adapter.Price `json:"prices"`
	}
	if err := adapter.StrictJSON(data, &fragment); err != nil || fragment.Prices == nil {
		return nil, errors.New("pricing file must contain a prices object")
	}
	return fragment.Prices, nil
}
func saveConfiguration(path string, c gateway.Config, secret string, replace bool, store secretWriter) error {
	if strings.TrimSpace(secret) == "" || strings.ContainsAny(secret, "\r\n\x00") {
		return errors.New("API key is empty or contains invalid characters")
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
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return errors.New("cannot generate credential reference")
	}
	account := hex.EncodeToString(nonce)
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return errors.New("cannot generate gateway token")
	}
	c.UpstreamKeychain = gateway.KeychainReference{Service: "com.tidemux." + c.Protocol, Account: account}
	c.AccessTokenKeychain = gateway.KeychainReference{Service: "com.tidemux.gateway", Account: account}
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
	if err = store.StoreNew(ctx, c.UpstreamKeychain, secret); err != nil {
		return errors.New("cannot save API key in Keychain; unlock your login keychain and try again")
	}
	created = append(created, c.UpstreamKeychain)
	if err = store.StoreNew(ctx, c.AccessTokenKeychain, hex.EncodeToString(token)); err != nil {
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
		backup, backupErr := os.OpenFile(path+".backup-"+account, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
