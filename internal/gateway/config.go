package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
)

type KeychainReference struct {
	Service string `json:"service"`
	Account string `json:"account"`
}

// Provider is a named, single-protocol upstream API profile. APIKey is
// populated only after resolving its Keychain reference and is never serialized.
type Provider struct {
	Protocol          string                   `json:"protocol"`
	BaseURL           string                   `json:"base_url"`
	UpstreamKeychain  KeychainReference        `json:"upstream_keychain"`
	APIVersion        string                   `json:"anthropic_version,omitempty"`
	Model             string                   `json:"model"`
	SupportedModels   []string                 `json:"supported_models,omitempty"`
	UpstreamID        string                   `json:"upstream_id,omitempty"`
	ModelCapabilities ModelCapabilities        `json:"model_capabilities,omitempty"`
	Prices            map[string]adapter.Price `json:"prices,omitempty"`
	APIKey            string                   `json:"-"`
}

func (r KeychainReference) Validate(name string) error {
	if strings.TrimSpace(r.Service) == "" || strings.TrimSpace(r.Account) == "" {
		return errors.New(name + " requires service and account")
	}
	return nil
}

type SecretLookup interface {
	Lookup(context.Context, KeychainReference) (string, error)
}

type Config struct {
	Budget                          ledger.BudgetPolicy      `json:"budget,omitempty"`
	Reconciliation                  ReconciliationConfig     `json:"reconciliation,omitempty"`
	ReportSchedule                  ReportSchedule           `json:"report_schedule,omitempty"`
	ModelCapabilities               ModelCapabilities        `json:"model_capabilities,omitempty"`
	Limits                          adapter.Limits           `json:"limits,omitempty"`
	ListenAddr                      string                   `json:"listen_addr"`
	Protocol                        string                   `json:"protocol,omitempty"` // resolved provider protocol; empty/auto means detect; legacy values remain supported
	BaseURL                         string                   `json:"base_url,omitempty"`
	Providers                       map[string]Provider      `json:"providers,omitempty"`
	DefaultProviders                map[string]string        `json:"default_providers,omitempty"`
	Model                           string                   `json:"model,omitempty"`
	UpstreamID                      string                   `json:"upstream_id,omitempty"`
	APIVersion                      string                   `json:"anthropic_version,omitempty"`
	UpstreamKeychain                KeychainReference        `json:"upstream_keychain,omitempty"`
	AccessTokenKeychain             KeychainReference        `json:"access_token_keychain"`
	MaxInFlight                     int                      `json:"max_in_flight"`
	MaxActiveSessions               int                      `json:"max_active_sessions,omitempty"`
	ActiveSessionIdleTimeoutSeconds int                      `json:"active_session_idle_timeout_seconds,omitempty"`
	LedgerPath                      string                   `json:"ledger_path"`
	Prices                          map[string]adapter.Price `json:"prices,omitempty"`
	APIKey                          string                   `json:"-"`
	AccessToken                     string                   `json:"-"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, errors.New("cannot read config")
	}
	var c Config
	if adapter.StrictJSON(data, &c) != nil {
		var raw struct {
			Budget map[string]json.RawMessage `json:"budget"`
		}
		if json.Unmarshal(data, &raw) == nil {
			for _, key := range []string{"daily_limit", "monthly_limit", "timezone", "reserve_amount"} {
				if _, ok := raw.Budget[key]; ok {
					return c, errors.New("legacy budget fields conflict with the current schema; run tidemux budget or configure --replace")
				}
			}
		}
		return c, errors.New("invalid config: use the current example; plaintext and legacy DeepSeek fields are not supported")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if err := c.Budget.Validate(); err != nil {
		return err
	}
	if err := c.Reconciliation.Validate(); err != nil {
		return err
	}
	if err := c.ReportSchedule.Validate(); err != nil {
		return err
	}
	if err := c.Limits.Validate(); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(c.ListenAddr)
	ip := net.ParseIP(host)
	if err != nil || port == "" || ip == nil {
		return errors.New("listen_addr must use an IP address and port")
	}
	if !ip.IsLoopback() && host != "0.0.0.0" {
		return errors.New("listen_addr must use a loopback IP or 0.0.0.0")
	}
	if err := c.ModelCapabilities.Validate(); err != nil {
		return err
	}
	if len(c.Providers) == 0 {
		if err := validateBaseURL(c.BaseURL, "base_url"); err != nil {
			return err
		}
		if len(c.DefaultProviders) != 0 {
			return errors.New("default_providers requires named providers")
		}
		switch normalizeProviderProtocol(c.Protocol) {
		case "", "auto", "openai", "anthropic":
		default:
			return errors.New("protocol must be auto, openai or anthropic")
		}
		if err := c.UpstreamKeychain.Validate("upstream_keychain"); err != nil {
			return err
		}
		if normalizeProviderProtocol(c.Protocol) == "anthropic" {
			if _, err := time.Parse("2006-01-02", c.APIVersion); err != nil {
				return errors.New("anthropic_version must be YYYY-MM-DD")
			}
		}
		if strings.TrimSpace(c.Model) == "" || strings.TrimSpace(c.UpstreamID) == "" {
			return errors.New("model and upstream_id are required")
		}
	} else {
		if c.BaseURL != "" || c.Protocol != "" || c.APIVersion != "" || c.UpstreamKeychain != (KeychainReference{}) || c.Model != "" || c.UpstreamID != "" || c.ModelCapabilities != (ModelCapabilities{}) || len(c.Prices) != 0 || c.APIKey != "" {
			return errors.New("use either legacy single-provider fields or providers, not both")
		}
		if len(c.Providers) > 128 {
			return errors.New("providers may contain at most 128 named entries")
		}
		availableProtocols := make(map[string]bool, 2)
		protocolCounts := make(map[string]int, 2)
		hasAutoProvider := false
		for name, provider := range c.Providers {
			if !validProviderName(name) {
				return errors.New("provider names must be short non-secret labels using letters, numbers, dots, underscores or hyphens")
			}
			protocol := normalizeProviderProtocol(provider.Protocol)
			switch protocol {
			case "", "auto":
				hasAutoProvider = true
			case "openai", "anthropic":
				availableProtocols[protocol] = true
				protocolCounts[protocol]++
			default:
				return errors.New("providers." + name + ".protocol must be auto, openai or anthropic")
			}
			if err := validateBaseURL(provider.BaseURL, "providers."+name+".base_url"); err != nil {
				return err
			}
			if err := provider.UpstreamKeychain.Validate("providers." + name + ".upstream_keychain"); err != nil {
				return err
			}
			if strings.TrimSpace(provider.Model) == "" {
				return errors.New("providers." + name + ".model is required")
			}
			seenModels := make(map[string]struct{}, len(provider.SupportedModels))
			for _, model := range provider.SupportedModels {
				if strings.TrimSpace(model) == "" || model != strings.TrimSpace(model) {
					return errors.New("providers." + name + ".supported_models must contain non-empty model IDs without surrounding whitespace")
				}
				if _, exists := seenModels[model]; exists {
					return errors.New("providers." + name + ".supported_models must not contain duplicate model IDs")
				}
				seenModels[model] = struct{}{}
			}
			if len(provider.SupportedModels) > 0 && !containsModel(provider.SupportedModels, provider.Model) {
				return errors.New("providers." + name + ".model must be included in supported_models")
			}
			if protocol == "openai" && provider.APIVersion != "" {
				return errors.New("anthropic_version is valid only for the anthropic endpoint")
			}
			if (protocol == "anthropic" || protocol == "auto") && provider.APIVersion != "" {
				if _, err := time.Parse("2006-01-02", provider.APIVersion); err != nil {
					return errors.New("providers." + name + ".anthropic_version must be YYYY-MM-DD")
				}
			}
			if err := provider.ModelCapabilities.Validate(); err != nil {
				return err
			}
			if err := validatePrices(provider.Prices); err != nil {
				return err
			}
			if c.Budget != (ledger.BudgetPolicy{}) {
				price, ok := provider.Prices[provider.Model]
				if !ok {
					return errors.New("budget requires pricing for each configured provider model, including " + name)
				}
				if price.Currency != c.Budget.Currency {
					return errors.New("budget currency must match each provider model pricing currency, including " + name)
				}
			}
		}
		for protocol, name := range c.DefaultProviders {
			if protocol != "openai" && protocol != "anthropic" {
				return errors.New("default_providers keys must be openai or anthropic")
			}
			provider, ok := c.Providers[name]
			if !ok {
				return errors.New("default_providers." + protocol + " must reference a configured provider name")
			}
			providerProtocol := normalizeProviderProtocol(provider.Protocol)
			if providerProtocol != "" && providerProtocol != "auto" && providerProtocol != protocol {
				return errors.New("default_providers." + protocol + " must reference a provider with the same protocol")
			}
		}
		for protocol := range availableProtocols {
			if _, ok := c.DefaultProviders[protocol]; !ok && protocolCounts[protocol] > 1 && !hasAutoProvider {
				return errors.New("default_providers must select a default for the " + protocol + " protocol")
			}
		}
	}
	if len(c.Providers) == 0 && c.Budget != (ledger.BudgetPolicy{}) {
		price, ok := c.Prices[c.Model]
		if !ok {
			return errors.New("budget requires pricing for configured model")
		}
		if price.Currency != c.Budget.Currency {
			return errors.New("budget currency must match model pricing currency")
		}
	}
	if len(c.Providers) == 0 && (len(c.UpstreamID) > 80 || strings.ContainsAny(c.UpstreamID, " /:@?\r\n")) {
		return errors.New("upstream_id must be a short non-secret label")
	}
	for name, provider := range c.Providers {
		if len(provider.UpstreamID) > 80 || strings.ContainsAny(provider.UpstreamID, " /:@?\r\n") {
			return errors.New("providers." + name + ".upstream_id must be a short non-secret label")
		}
	}
	if c.MaxInFlight < 1 || c.MaxInFlight > 1024 {
		return errors.New("max_in_flight must be 1..1024")
	}
	if c.MaxActiveSessions < 0 || c.MaxActiveSessions > 4096 {
		return errors.New("max_active_sessions must be 0..4096")
	}
	if c.ActiveSessionIdleTimeoutSeconds < 0 || c.ActiveSessionIdleTimeoutSeconds > 86400 {
		return errors.New("active_session_idle_timeout_seconds must be 0..86400; zero uses the five-minute default")
	}
	if strings.TrimSpace(c.LedgerPath) == "" {
		return errors.New("ledger_path is required")
	}
	if err := c.AccessTokenKeychain.Validate("access_token_keychain"); err != nil {
		return err
	}
	if len(c.Providers) == 0 {
		if err := validatePrices(c.Prices); err != nil {
			return err
		}
	}
	return nil
}

func validProviderName(name string) bool {
	if name == "" || name != strings.TrimSpace(name) || len(name) > 80 {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// ValidateProviderBaseURL validates an upstream API root before discovery or
// configuration. Credentials, query parameters and fragments are never allowed.
func ValidateProviderBaseURL(value string) error {
	return validateBaseURL(value, "base_url")
}

func validatePrices(prices map[string]adapter.Price) error {
	for model, p := range prices {
		if strings.TrimSpace(model) == "" {
			return errors.New("empty pricing model")
		}
		if err := p.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateBaseURL(value, name string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New(name + " must be an API root without credentials, query or fragment")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return errors.New(name + " requires HTTPS, except loopback HTTP")
		}
	}
	return nil
}
func (c Config) ResolveCredentials(ctx context.Context, lookup SecretLookup) (Config, error) {
	if err := c.Validate(); err != nil {
		return c, err
	}
	if lookup == nil {
		return c, errors.New("Keychain lookup required")
	}
	var err error
	c.AccessToken, err = lookup.Lookup(ctx, c.AccessTokenKeychain)
	if err != nil {
		return Config{}, errors.New("gateway Keychain item unavailable")
	}
	providerKeys := make([]string, 0, len(c.Providers))
	if len(c.Providers) == 0 {
		c.APIKey, err = lookup.Lookup(ctx, c.UpstreamKeychain)
		if err != nil {
			return Config{}, errors.New("upstream Keychain item unavailable")
		}
		providerKeys = append(providerKeys, c.APIKey)
	} else {
		for name, provider := range c.Providers {
			provider.APIKey, err = lookup.Lookup(ctx, provider.UpstreamKeychain)
			if err != nil {
				return Config{}, errors.New("upstream Keychain item unavailable")
			}
			providerKeys = append(providerKeys, provider.APIKey)
			c.Providers[name] = provider
		}
	}
	if strings.TrimSpace(c.AccessToken) == "" || strings.ContainsAny(c.AccessToken, "\r\n") {
		return Config{}, errors.New("invalid Keychain credential")
	}
	for _, key := range providerKeys {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
			return Config{}, errors.New("invalid Keychain credential")
		}
		if key == c.AccessToken {
			return Config{}, errors.New("use separate upstream and gateway credentials")
		}
	}
	return c, nil
}
