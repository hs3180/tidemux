package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
	"golang.org/x/term"
)

func providerKeyCommand(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New("usage: tidemux provider key <add|list|remove>")
	}
	switch args[0] {
	case "add":
		return providerKeyAdd(args[1:], stdout, stderr)
	case "list":
		return providerKeyList(args[1:], stdout, stderr)
	case "remove":
		return providerKeyRemove(args[1:], stdout, stderr)
	default:
		return errors.New("usage: tidemux provider key <add|list|remove>")
	}
}

func configuredProviderKeyCount(provider gateway.Provider) int {
	refs, err := provider.KeychainReferences()
	if err != nil {
		return 0
	}
	return len(refs)
}

func providerKeyAdd(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	if ref == "" {
		return errors.New("usage: tidemux provider key add REF [--config PATH]")
	}
	flags := flag.NewFlagSet("provider key add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("provider key add takes one provider reference")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("provider credentials require macOS Keychain")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	store := gateway.MacOSKeychain{}
	c, err = migrateLegacyProvider(c, store)
	if err != nil {
		return err
	}
	if _, ok := c.Providers[ref]; !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	tty, err := openControlTTY("adding a provider API key requires an interactive terminal; keys are never accepted as command arguments")
	if err != nil {
		return err
	}
	defer tty.Close()
	if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
		return err
	}
	fmt.Fprint(tty, "Additional provider API key (hidden): ")
	secret, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return errors.New("could not read provider API key")
	}
	defer zeroBytes(secret)
	if err := appendProviderKey(abs, c, before, ref, string(secret), store, store); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Added an API key to provider %s. Keys are selected round-robin.\n", ref)
	return nil
}

func appendProviderKey(path string, c gateway.Config, before []byte, providerName, value string, lookup gateway.SecretLookup, store secretWriter) error {
	if !validSecret([]byte(value)) {
		return errors.New("a valid provider API key is required")
	}
	provider, ok := c.Providers[providerName]
	if !ok {
		return fmt.Errorf("provider %q not found", providerName)
	}
	if lookup == nil || store == nil {
		return errors.New("Keychain access is required")
	}
	if credentialRefEmpty(c.AccessTokenKeychain) {
		return errors.New("gateway credential is not configured")
	}
	accessToken, err := lookup.Lookup(context.Background(), c.AccessTokenKeychain)
	if err != nil {
		return errors.New("gateway Keychain item unavailable")
	}
	if value == accessToken {
		return errors.New("provider API key must differ from the gateway API key")
	}
	refs, err := provider.KeychainReferences()
	if err != nil {
		return errors.New("provider Keychain references are invalid")
	}
	for _, existingRef := range refs {
		key, lookupErr := lookup.Lookup(context.Background(), existingRef)
		if lookupErr != nil {
			return errors.New("provider Keychain item unavailable")
		}
		if key == value {
			return errors.New("provider API key is already present in this group")
		}
	}
	newRef, err := newKeychainReference("com.tidemux.provider")
	if err != nil {
		return err
	}
	if err := store.StoreNew(context.Background(), newRef, value); err != nil {
		return errors.New("cannot save provider API key in Keychain")
	}
	committed := false
	defer func() {
		if !committed {
			_ = store.Delete(context.Background(), newRef)
		}
	}()
	stored, err := lookup.Lookup(context.Background(), newRef)
	if err != nil || stored != value {
		return errors.New("provider API key Keychain read-back verification failed")
	}
	provider.UpstreamKeychain = gateway.KeychainReference{}
	provider.UpstreamKeychains = append(refs, newRef)
	c.Providers[providerName] = provider
	if err := writeCommandConfig(path, c, before); err != nil {
		return err
	}
	committed = true
	return nil
}

func providerKeyList(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	if ref == "" {
		return errors.New("usage: tidemux provider key list REF [--config PATH]")
	}
	flags := flag.NewFlagSet("provider key list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("provider key list takes one provider reference")
	}
	c, err := gateway.LoadConfig(*path)
	if err != nil {
		return err
	}
	if len(c.Providers) == 0 && c.BaseURL != "" {
		c, err = migrateLegacyProvider(c, nil)
		if err != nil {
			return err
		}
	}
	provider, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	refs, err := provider.KeychainReferences()
	if err != nil {
		return errors.New("provider Keychain references are invalid")
	}
	fmt.Fprintln(stdout, "KEY  STATUS")
	for i := range refs {
		fmt.Fprintf(stdout, "%d    stored in Keychain\n", i+1)
	}
	return nil
}

func providerKeyRemove(args []string, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	if ref == "" {
		return errors.New("usage: tidemux provider key remove REF INDEX [--yes] [--config PATH]")
	}
	flags := flag.NewFlagSet("provider key remove", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	yes := flags.Bool("yes", false, "confirm removal")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tidemux provider key remove REF INDEX [--yes] [--config PATH]")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("provider credentials require macOS Keychain")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	provider, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	refs, err := provider.KeychainReferences()
	if err != nil {
		return errors.New("provider Keychain references are invalid")
	}
	if len(refs) <= 1 {
		return errors.New("a provider group must retain at least one API key")
	}
	index, err := strconv.Atoi(flags.Arg(0))
	if err != nil || index < 1 || index > len(refs) {
		return errors.New("provider key index must identify an existing key")
	}
	if !*yes {
		tty, openErr := openControlTTY("provider key removal requires a terminal; use --yes for non-interactive use")
		if openErr != nil {
			return openErr
		}
		defer tty.Close()
		fmt.Fprintf(tty, "Remove API key %d from provider %s? [y/N] ", index, ref)
		answer, readErr := readTerminalLine(tty)
		if readErr != nil || !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
			return errors.New("provider key removal canceled")
		}
		if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
			return err
		}
	}
	if err := removeProviderKey(abs, c, before, ref, index, gateway.MacOSKeychain{}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Removed API key %d from provider %s.\n", index, ref)
	return nil
}

func removeProviderKey(path string, c gateway.Config, before []byte, providerName string, index int, store secretWriter) error {
	if store == nil {
		return errors.New("Keychain access is required")
	}
	provider, ok := c.Providers[providerName]
	if !ok {
		return fmt.Errorf("provider %q not found", providerName)
	}
	refs, err := provider.KeychainReferences()
	if err != nil {
		return errors.New("provider Keychain references are invalid")
	}
	if len(refs) <= 1 {
		return errors.New("a provider group must retain at least one API key")
	}
	if index < 1 || index > len(refs) {
		return errors.New("provider key index must identify an existing key")
	}
	target := refs[index-1]
	if _, err := store.Lookup(context.Background(), target); err != nil {
		return errors.New("provider Keychain item unavailable")
	}
	remaining := append([]gateway.KeychainReference(nil), refs[:index-1]...)
	remaining = append(remaining, refs[index:]...)
	provider.UpstreamKeychain = gateway.KeychainReference{}
	provider.UpstreamKeychains = remaining
	c.Providers[providerName] = provider
	if err := writeCommandConfig(path, c, before); err != nil {
		return err
	}
	if err := deleteUnreferencedProviderKeys(c, []gateway.KeychainReference{target}, store); err != nil {
		return err
	}
	return nil
}
