package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
)

type secretWriter interface {
	gateway.SecretLookup
	StoreNew(context.Context, gateway.KeychainReference, string) error
	Delete(context.Context, gateway.KeychainReference) error
}

type keychainEntry struct {
	reference gateway.KeychainReference
	secret    string
}

func saveConfiguration(path string, c gateway.Config, secret string, gatewaySecret []byte, replace bool, store secretWriter) error {
	return saveConfigurationWithProviderKeys(path, c, map[string]string{"legacy": secret}, gatewaySecret, replace, store)
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
