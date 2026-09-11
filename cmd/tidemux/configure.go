package main

import (
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

	"github.com/hs3180/tidemux/internal/gateway"
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
	max := flags.Int("max-in-flight", 1, "maximum simultaneous upstream requests")
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
	c := gateway.Config{ListenAddr: "127.0.0.1:8787", Protocol: *protocol, BaseURL: *baseURL, Model: *model, UpstreamID: *protocol + "-primary", APIVersion: "2023-06-01", MaxInFlight: *max, LedgerPath: filepath.Join(filepath.Dir(abs), "ledger.db"), UpstreamKeychain: gateway.KeychainReference{Service: "pending", Account: "pending"}, AccessTokenKeychain: gateway.KeychainReference{Service: "pending", Account: "pending"}}
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
	if err := unlockKeychainIfNeeded(tty); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Protocol: %s\nAPI root: %s\nModel: %s\nConfig: %s\n", c.Protocol, c.BaseURL, c.Model, abs)
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
	fmt.Fprintln(stdout, "Local checks passed; no upstream request was sent. Prices remain unknown until configured.")
	fmt.Fprintf(stdout, "Next: tidemux serve --config %q\nInspect: tidemux ledger --config %q\n", abs, abs)
	return nil
}
func saveConfiguration(path string, c gateway.Config, secret string, replace bool, store secretWriter) error {
	if strings.TrimSpace(secret) == "" || strings.ContainsAny(secret, "\r\n\x00") {
		return errors.New("API key is empty or contains invalid characters")
	}
	if _, err := os.Lstat(path); err == nil && !replace {
		return errors.New("config already exists")
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
	if replace {
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
func unlockKeychainIfNeeded(tty *os.File) error {
	if _, err := exec.Command("security", "show-keychain-info").CombinedOutput(); err == nil {
		return nil
	}
	fmt.Fprintln(tty, "登录钥匙串需要解锁。接下来由 macOS security 请求钥匙串密码（通常是 Mac 登录密码，不是 API key）；输入不会回显，按 Control-C 取消。")
	command := exec.Command("security", "unlock-keychain")
	command.Stdin, command.Stdout, command.Stderr = tty, tty, tty
	if err := command.Run(); err != nil {
		return errors.New("keychain unlock failed or was canceled; no API key was requested and no configuration was changed")
	}
	if _, err := exec.Command("security", "show-keychain-info").CombinedOutput(); err != nil {
		return errors.New("keychain is still unavailable after unlock; no configuration was changed")
	}
	fmt.Fprintln(tty, "钥匙串已解锁，继续配置 API key。")
	return nil
}
