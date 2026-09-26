package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/hs3180/tidemux/internal/gateway"
)

func providerValidate(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	if ref == "" {
		return errors.New("usage: tidemux provider validate REF [--config PATH]")
	}
	flags := flag.NewFlagSet("provider validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: tidemux provider validate REF [--config PATH]")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("provider credential validation requires macOS Keychain")
	}
	tty, err := openControlTTY("provider validation requires a terminal to access Keychain safely")
	if err != nil {
		return err
	}
	defer tty.Close()
	if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
		return err
	}
	config, _, _, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	config, err = migrateLegacyProvider(config, nil)
	if err != nil {
		return err
	}
	provider, ok := config.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	count, err := validateProviderKeychainGroup(provider, config.AccessTokenKeychain, gateway.MacOSKeychain{})
	if err != nil {
		return fmt.Errorf("provider %s credentials are invalid: %w", ref, err)
	}
	fmt.Fprintf(stdout, "Provider %s is valid: %d API keys are available in Keychain; %s. No upstream request was made.\n", ref, count, providerModelScopeLabel(provider.SupportedModels))
	return nil
}

func validateProviderKeychainGroup(provider gateway.Provider, gatewayRef gateway.KeychainReference, lookup gateway.SecretLookup) (int, error) {
	if lookup == nil {
		return 0, errors.New("Keychain access is required")
	}
	gatewayKey, err := lookup.Lookup(context.Background(), gatewayRef)
	if err != nil || !validSecret([]byte(gatewayKey)) {
		return 0, errors.New("gateway Keychain item unavailable or invalid")
	}
	refs, err := provider.KeychainReferences()
	if err != nil {
		return 0, errors.New("provider Keychain references are invalid")
	}
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		key, lookupErr := lookup.Lookup(context.Background(), ref)
		if lookupErr != nil || !validSecret([]byte(key)) {
			return 0, errors.New("provider Keychain item unavailable or invalid")
		}
		if key == gatewayKey {
			return 0, errors.New("provider API key must differ from the gateway API key")
		}
		if _, exists := seen[key]; exists {
			return 0, errors.New("provider API keys must be unique within the group")
		}
		seen[key] = struct{}{}
	}
	return len(refs), nil
}
