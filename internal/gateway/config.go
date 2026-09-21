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
	Protocol                        string                   `json:"protocol"`
	BaseURL                         string                   `json:"base_url"`
	AnthropicBaseURL                string                   `json:"anthropic_base_url,omitempty"`
	Model                           string                   `json:"model"`
	UpstreamID                      string                   `json:"upstream_id"`
	APIVersion                      string                   `json:"anthropic_version,omitempty"`
	UpstreamKeychain                KeychainReference        `json:"upstream_keychain"`
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
	if err := c.ModelCapabilities.Validate(); err != nil {
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
	if c.Protocol != "openai" && c.Protocol != "anthropic" && c.Protocol != "both" {
		return errors.New("protocol must be openai, anthropic or both")
	}
	if err := validateBaseURL(c.BaseURL, "base_url"); err != nil {
		return err
	}
	if c.AnthropicBaseURL != "" {
		if c.Protocol != "both" {
			return errors.New("anthropic_base_url requires protocol both")
		}
		if err := validateBaseURL(c.AnthropicBaseURL, "anthropic_base_url"); err != nil {
			return err
		}
	}
	if strings.TrimSpace(c.Model) == "" || strings.TrimSpace(c.UpstreamID) == "" {
		return errors.New("model and upstream_id are required")
	}
	if c.Budget != (ledger.BudgetPolicy{}) {
		price, ok := c.Prices[c.Model]
		if !ok {
			return errors.New("budget requires pricing for configured model")
		}
		if price.Currency != c.Budget.Currency {
			return errors.New("budget currency must match model pricing currency")
		}
	}
	if len(c.UpstreamID) > 80 || strings.ContainsAny(c.UpstreamID, " /:@?\r\n") {
		return errors.New("upstream_id must be a short non-secret label")
	}
	if c.Protocol == "anthropic" || c.Protocol == "both" {
		if _, err := time.Parse("2006-01-02", c.APIVersion); err != nil {
			return errors.New("anthropic_version must be YYYY-MM-DD")
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
	if err := c.UpstreamKeychain.Validate("upstream_keychain"); err != nil {
		return err
	}
	if err := c.AccessTokenKeychain.Validate("access_token_keychain"); err != nil {
		return err
	}
	for model, p := range c.Prices {
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
	c.APIKey, err = lookup.Lookup(ctx, c.UpstreamKeychain)
	if err != nil {
		return Config{}, errors.New("upstream Keychain item unavailable")
	}
	c.AccessToken, err = lookup.Lookup(ctx, c.AccessTokenKeychain)
	if err != nil {
		return Config{}, errors.New("gateway Keychain item unavailable")
	}
	if strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.AccessToken) == "" || strings.ContainsAny(c.APIKey+c.AccessToken, "\r\n") {
		return Config{}, errors.New("invalid Keychain credential")
	}
	if c.APIKey == c.AccessToken {
		return Config{}, errors.New("use separate upstream and gateway credentials")
	}
	return c, nil
}
