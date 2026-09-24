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
	"path/filepath"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
	"golang.org/x/term"
)

func configureReportWebhook(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("report webhook", flag.ContinueOnError)
	flags.SetOutput(stderr)
	provider := flags.String("provider", "", "webhook provider: generic, telegram, discord or lark")
	configPath := flags.String("config", defaultConfigPath(), "configuration path")
	disable := flags.Bool("disable", false, "remove the configured webhook and disable webhook scheduling")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*configPath) == "" {
		return errors.New(usage)
	}
	path, err := filepath.Abs(*configPath)
	if err != nil {
		return errors.New("invalid config path")
	}
	current, err := gateway.LoadConfig(path)
	if err != nil {
		return err
	}
	if *disable {
		previousKeychain := current.ReportWebhook.Keychain
		if current.ReportSchedule.Channel == "webhook" {
			if err := replaceReportScheduleConfig(path, gateway.ReportSchedule{}); err != nil {
				return err
			}
			if _, err := syncReportSchedule(path, gateway.ReportSchedule{}); err != nil {
				return fmt.Errorf("webhook removed from configuration, but schedule cleanup failed: %w", err)
			}
		}
		if err := replaceReportWebhookConfig(path, gateway.ReportWebhookConfig{}); err != nil {
			return err
		}
		if err := deleteStaleReportWebhookEndpoint(path, previousKeychain, gateway.MacOSKeychain{}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Report webhook disabled: %s\n", path)
		return nil
	}
	if err := validateWebhookProvider(*provider); err != nil {
		return err
	}
	candidate := current
	candidate.ReportWebhook = gateway.ReportWebhookConfig{Provider: *provider, Keychain: gateway.KeychainReference{Service: "pending", Account: "pending"}}
	if err := candidate.Validate(); err != nil {
		return err
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return errors.New("run report webhook in an interactive terminal; endpoints are never accepted as command arguments")
	}
	defer tty.Close()
	if !term.IsTerminal(int(tty.Fd())) {
		return errors.New("interactive terminal required")
	}
	if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Webhook provider: %s\n", *provider)
	fmt.Fprint(tty, "Webhook endpoint (hidden; paste then press Enter): ")
	secret, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return errors.New("could not read webhook endpoint")
	}
	defer func() {
		for i := range secret {
			secret[i] = 0
		}
	}()
	endpoint := strings.TrimSpace(string(secret))
	if err := validateReportWebhookEndpoint(endpoint); err != nil {
		return err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return errors.New("cannot generate webhook credential reference")
	}
	candidate.ReportWebhook.Keychain = gateway.KeychainReference{Service: "com.tidemux.report-webhook", Account: hex.EncodeToString(nonce)}
	store := gateway.MacOSKeychain{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.StoreNew(ctx, candidate.ReportWebhook.Keychain, endpoint); err != nil {
		return errors.New("cannot save report webhook endpoint in Keychain")
	}
	committed := false
	defer func() {
		if !committed {
			_ = store.Delete(context.Background(), candidate.ReportWebhook.Keychain)
		}
	}()
	stored, lookupErr := store.Lookup(ctx, candidate.ReportWebhook.Keychain)
	if lookupErr != nil || stored != endpoint {
		return errors.New("report webhook Keychain read-back verification failed")
	}
	if err := replaceReportWebhookConfig(path, candidate.ReportWebhook); err != nil {
		return err
	}
	committed = true
	if err := deleteStaleReportWebhookEndpoint(path, current.ReportWebhook.Keychain, store); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Report webhook configured for %s. The endpoint is stored in macOS Keychain.\n", *provider)
	return nil
}

type reportWebhookKeychainDeleter interface {
	Delete(context.Context, gateway.KeychainReference) error
}

func deleteStaleReportWebhookEndpoint(path string, reference gateway.KeychainReference, store reportWebhookKeychainDeleter) error {
	if reference == (gateway.KeychainReference{}) {
		return nil
	}
	if store == nil {
		return errors.New("report webhook configuration was updated, but the old Keychain item could not be deleted")
	}
	c, err := gateway.LoadConfig(path)
	if err != nil {
		return errors.New("report webhook configuration was updated, but the old Keychain item could not be safely removed")
	}
	if c.ReportWebhook.Keychain == reference || c.AccessTokenKeychain == reference || c.UpstreamKeychain == reference {
		return nil
	}
	for _, provider := range c.Providers {
		if provider.UpstreamKeychain == reference {
			return nil
		}
	}
	if err := store.Delete(context.Background(), reference); err != nil {
		return errors.New("report webhook configuration was updated, but the old endpoint could not be deleted from Keychain")
	}
	return nil
}

func validateWebhookProvider(provider string) error {
	switch provider {
	case "generic", "telegram", "discord", "lark":
		return nil
	default:
		return errors.New("--provider must be generic, telegram, discord or lark")
	}
}

func replaceReportWebhookConfig(path string, webhook gateway.ReportWebhookConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read config")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("invalid config JSON")
	}
	if webhook == (gateway.ReportWebhookConfig{}) {
		delete(raw, "report_webhook")
	} else {
		encoded, err := json.Marshal(webhook)
		if err != nil {
			return errors.New("cannot encode report webhook")
		}
		raw["report_webhook"] = encoded
	}
	updated, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return errors.New("cannot encode config")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tidemux-webhook-*")
	if err != nil {
		return errors.New("cannot prepare config")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("cannot protect config")
	}
	if _, err := tmp.Write(append(updated, '\n')); err != nil {
		tmp.Close()
		return errors.New("cannot write config")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("cannot sync config")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("cannot close config")
	}
	if _, err := gateway.LoadConfig(tmpName); err != nil {
		return err
	}
	backup := fmt.Sprintf("%s.backup-webhook-%d", path, time.Now().UnixNano())
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return errors.New("cannot create config backup")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("cannot install configuration")
	}
	return nil
}
