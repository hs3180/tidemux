// Package gateway implements TideMux's loopback-only OpenAI-compatible edge.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
)

// KeychainReference identifies a secret stored in the user's macOS Keychain.
// It intentionally contains no secret value and is safe to put in config.
type KeychainReference struct {
	Service string `json:"service"`
	Account string `json:"account"`
}

func (r KeychainReference) Validate(name string) error {
	if strings.TrimSpace(r.Service) == "" || strings.TrimSpace(r.Account) == "" {
		return fmt.Errorf("%s must include keychain service and account", name)
	}
	return nil
}

// SecretLookup is the narrow runtime boundary for retrieving secrets. A
// lookup implementation must not log secret values.
type SecretLookup interface {
	Lookup(context.Context, KeychainReference) (string, error)
}

// Config is deliberately explicit and local. Its JSON form stores only
// Keychain references; resolved credentials are runtime-only fields.
type Config struct {
	ListenAddr          string            `json:"listen_addr"`
	DeepSeekKeychain    KeychainReference `json:"deepseek_keychain"`
	AccessTokenKeychain KeychainReference `json:"access_token_keychain"`
	DeepSeekBaseURL     string            `json:"deepseek_base_url,omitempty"`
	MaxInFlight         int               `json:"max_in_flight"`
	LedgerPath          string            `json:"ledger_path"`
	DeepSeekAPIKey      string            `json:"-"`
	AccessToken         string            `json:"-"`
}

// LoadConfig reads and validates the local gateway configuration.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read gateway config: %w", err)
	}
	var config Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode gateway config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// ResolveCredentials retrieves the two runtime credentials after validating
// their references. Returned Config never serializes those values.
func (c Config) ResolveCredentials(ctx context.Context, lookup SecretLookup) (Config, error) {
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	if lookup == nil {
		return Config{}, fmt.Errorf("keychain lookup is required")
	}
	deepSeekKey, err := lookup.Lookup(ctx, c.DeepSeekKeychain)
	if err != nil {
		return Config{}, fmt.Errorf("read DeepSeek key from Keychain: %w", err)
	}
	accessToken, err := lookup.Lookup(ctx, c.AccessTokenKeychain)
	if err != nil {
		return Config{}, fmt.Errorf("read gateway access token from Keychain: %w", err)
	}
	if strings.TrimSpace(deepSeekKey) == "" || strings.TrimSpace(accessToken) == "" {
		return Config{}, fmt.Errorf("Keychain returned an empty credential")
	}
	c.DeepSeekAPIKey, c.AccessToken = deepSeekKey, accessToken
	return c, nil
}

// Validate rejects non-loopback listeners before a server can be created.
func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil || port == "" {
		return fmt.Errorf("listen_addr must be host:port: %q", c.ListenAddr)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen_addr must use a loopback IP: %q", c.ListenAddr)
	}
	if err := c.DeepSeekKeychain.Validate("deepseek_keychain"); err != nil {
		return err
	}
	if err := c.AccessTokenKeychain.Validate("access_token_keychain"); err != nil {
		return err
	}
	if c.MaxInFlight < 1 {
		return fmt.Errorf("max_in_flight must be at least 1")
	}
	if strings.TrimSpace(c.LedgerPath) == "" {
		return fmt.Errorf("ledger_path is required")
	}
	return nil
}
