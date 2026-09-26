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
	UpstreamKeychain  KeychainReference        `json:"upstream_keychain,omitempty,omitzero"` // legacy one-key form
	UpstreamKeychains []KeychainReference      `json:"upstream_keychains,omitempty"`
	APIVersion        string                   `json:"anthropic_version,omitempty"`
	Model             string                   `json:"model"`
	SupportedModels   []string                 `json:"supported_models,omitempty"`
	UpstreamID        string                   `json:"upstream_id,omitempty"`
	ModelCapabilities ModelCapabilities        `json:"model_capabilities,omitempty"`
	Prices            map[string]adapter.Price `json:"prices,omitempty"`
	Budget            *ledger.BudgetPolicy     `json:"budget,omitempty"`
	APIKey            string                   `json:"-"`
	APIKeys           []string                 `json:"-"`
}

// KeychainReferences returns the configured keys for this provider profile.
// The singular field remains readable for existing configurations.
func (p Provider) KeychainReferences() ([]KeychainReference, error) {
	legacy := !credentialReferenceEmpty(p.UpstreamKeychain)
	if legacy && len(p.UpstreamKeychains) > 0 {
		return nil, errors.New("use either upstream_keychain or upstream_keychains, not both")
	}
	refs := p.UpstreamKeychains
	if legacy {
		refs = []KeychainReference{p.UpstreamKeychain}
	}
	if len(refs) == 0 || len(refs) > 128 {
		return nil, errors.New("provider must have between 1 and 128 upstream Keychain references")
	}
	seen := make(map[KeychainReference]struct{}, len(refs))
	for _, ref := range refs {
		if err := ref.Validate("upstream_keychains"); err != nil {
			return nil, err
		}
		if _, exists := seen[ref]; exists {
			return nil, errors.New("upstream_keychains must not contain duplicate references")
		}
		seen[ref] = struct{}{}
	}
	return append([]KeychainReference(nil), refs...), nil
}

func (p Provider) ResolvedAPIKeys() []string {
	if len(p.APIKeys) > 0 {
		return append([]string(nil), p.APIKeys...)
	}
	if p.APIKey != "" {
		return []string{p.APIKey}
	}
	return nil
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
	LegacyBudget                    *ledger.BudgetPolicy     `json:"budget,omitempty"` // accepted only by the provider-budget migration command
	Reconciliation                  ReconciliationConfig     `json:"reconciliation,omitempty"`
	ReportSchedule                  ReportSchedule           `json:"report_schedule,omitempty"`
	ReportWebhook                   ReportWebhookConfig      `json:"report_webhook,omitempty"`
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
	ReportWebhookURL                string                   `json:"-"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, errors.New("cannot read config")
	}
	c, err := decodeConfig(data)
	if err != nil {
		return Config{}, err
	}
	if c.LegacyBudget != nil && *c.LegacyBudget != (ledger.BudgetPolicy{}) {
		return Config{}, errors.New("global budget settings are no longer supported; assign them with `tidemux provider budget REF`")
	}
	return c, c.Validate()
}

// LoadConfigForProviderBudgetMigration reads a config while allowing the
// development-era global budget field to be moved to one named provider.
func LoadConfigForProviderBudgetMigration(path string) (Config, ledger.BudgetPolicy, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, ledger.BudgetPolicy{}, false, errors.New("cannot read config")
	}
	c, err := decodeConfig(data)
	legacyFieldsReplaced := false
	if err != nil {
		clean, replaced, stripErr := stripLegacyBudgetSection(data)
		if stripErr != nil || !replaced {
			return Config{}, ledger.BudgetPolicy{}, false, err
		}
		c, err = decodeConfig(clean)
		if err != nil {
			return Config{}, ledger.BudgetPolicy{}, false, err
		}
		legacyFieldsReplaced = true
	}
	var legacyBudget ledger.BudgetPolicy
	if c.LegacyBudget != nil {
		legacyBudget = *c.LegacyBudget
	}
	c.LegacyBudget = nil
	if err := c.Validate(); err != nil {
		return Config{}, ledger.BudgetPolicy{}, false, err
	}
	if err := legacyBudget.Validate(); err != nil {
		return Config{}, ledger.BudgetPolicy{}, false, errors.New("invalid global budget configuration")
	}
	return c, legacyBudget, legacyFieldsReplaced, nil
}

func stripLegacyBudgetSection(data []byte) ([]byte, bool, error) {
	var config map[string]json.RawMessage
	if err := adapter.StrictJSON(data, &config); err != nil {
		return nil, false, err
	}
	for key, raw := range config {
		if !strings.EqualFold(key, "budget") {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, false, err
		}
		legacy := false
		for _, name := range []string{"daily_limit", "monthly_limit", "timezone", "reserve_amount"} {
			if _, ok := fields[name]; ok {
				legacy = true
				break
			}
		}
		if !legacy {
			return nil, false, nil
		}
		delete(config, key)
		clean, err := json.Marshal(config)
		return clean, true, err
	}
	return nil, false, nil
}

func decodeConfig(data []byte) (Config, error) {
	var c Config
	if adapter.StrictJSON(data, &c) != nil {
		var raw struct {
			Budget map[string]json.RawMessage `json:"budget"`
		}
		if json.Unmarshal(data, &raw) == nil {
			for _, key := range []string{"daily_limit", "monthly_limit", "timezone", "reserve_amount"} {
				if _, ok := raw.Budget[key]; ok {
					return c, errors.New("legacy budget fields cannot be migrated automatically; configure a new policy with `tidemux provider budget REF`")
				}
			}
		}
		return c, errors.New("invalid config: use the current example; plaintext and legacy fields are not supported")
	}
	if c.LegacyBudget != nil && *c.LegacyBudget == (ledger.BudgetPolicy{}) {
		c.LegacyBudget = nil
	}
	for name, provider := range c.Providers {
		if provider.Budget != nil && *provider.Budget == (ledger.BudgetPolicy{}) {
			provider.Budget = nil
			c.Providers[name] = provider
		}
	}
	return c, nil
}
func (c Config) Validate() error {
	if c.LegacyBudget != nil {
		return errors.New("global budget settings are no longer supported; assign them with `tidemux provider budget REF`")
	}
	if err := c.Reconciliation.Validate(); err != nil {
		return err
	}
	if err := c.ReportSchedule.Validate(); err != nil {
		return err
	}
	if err := c.ReportWebhook.Validate(); err != nil {
		return err
	}
	if c.ReportSchedule.Channel == "webhook" && c.ReportWebhook == (ReportWebhookConfig{}) {
		return errors.New("webhook report schedule requires report_webhook configuration")
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
		if len(c.DefaultProviders) != 0 {
			return errors.New("default_providers requires named providers")
		}
		if c.BaseURL == "" {
			if c.Protocol != "" || c.APIVersion != "" || !credentialReferenceEmpty(c.UpstreamKeychain) || c.Model != "" || c.UpstreamID != "" || c.ModelCapabilities != (ModelCapabilities{}) || len(c.Prices) != 0 || c.APIKey != "" {
				return errors.New("incomplete legacy provider configuration")
			}
		} else {
			if err := validateBaseURL(c.BaseURL, "base_url"); err != nil {
				return err
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
			if _, err := provider.KeychainReferences(); err != nil {
				return errors.New("providers." + name + "." + err.Error())
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
			if provider.Budget != nil && *provider.Budget != (ledger.BudgetPolicy{}) {
				if err := provider.Budget.Validate(); err != nil {
					return errors.New("providers." + name + ".budget: " + err.Error())
				}
				price, ok := provider.Prices[provider.Model]
				if !ok {
					price, ok = adapter.BuiltInPrice(provider.BaseURL, provider.Model, time.Now())
				}
				if !ok {
					return errors.New("providers." + name + ".budget requires pricing for its default model")
				}
				if price.Currency != provider.Budget.Currency {
					return errors.New("providers." + name + ".budget currency must match the provider model pricing currency")
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

func credentialReferenceEmpty(ref KeychainReference) bool {
	return ref.Service == "" && ref.Account == ""
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
	if len(c.Providers) == 0 && c.BaseURL != "" {
		c.APIKey, err = lookup.Lookup(ctx, c.UpstreamKeychain)
		if err != nil {
			return Config{}, errors.New("upstream Keychain item unavailable")
		}
		providerKeys = append(providerKeys, c.APIKey)
	} else if len(c.Providers) > 0 {
		for name, provider := range c.Providers {
			refs, referenceErr := provider.KeychainReferences()
			if referenceErr != nil {
				return Config{}, errors.New("providers." + name + "." + referenceErr.Error())
			}
			keys := make([]string, 0, len(refs))
			for _, ref := range refs {
				key, lookupErr := lookup.Lookup(ctx, ref)
				if lookupErr != nil {
					return Config{}, errors.New("upstream Keychain item unavailable")
				}
				keys = append(keys, key)
			}
			provider.APIKeys = keys
			provider.APIKey = keys[0]
			providerKeys = append(providerKeys, keys...)
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
	for _, provider := range c.Providers {
		seen := make(map[string]struct{}, len(provider.APIKeys))
		for _, key := range provider.APIKeys {
			if _, exists := seen[key]; exists {
				return Config{}, errors.New("provider API keys must be unique within a key group")
			}
			seen[key] = struct{}{}
		}
	}
	return c, nil
}
