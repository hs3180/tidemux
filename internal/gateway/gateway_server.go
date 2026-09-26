package gateway

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

// NewHandler resolves providers and initializes the gateway's shared resources.
func NewHandler(c Config, httpClient *http.Client) (http.Handler, func() error, error) {
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	if len(c.Providers) == 0 && c.BaseURL == "" {
		return nil, nil, errors.New("no upstream provider is configured; run `tidemux provider add`")
	}
	if c.AccessToken == "" {
		return nil, nil, errors.New("distinct resolved credentials are required")
	}
	if len(c.Providers) == 0 {
		if c.APIKey == "" || c.APIKey == c.AccessToken {
			return nil, nil, errors.New("distinct resolved credentials are required")
		}
	} else {
		for _, provider := range c.Providers {
			keys := provider.ResolvedAPIKeys()
			if len(keys) == 0 {
				return nil, nil, errors.New("distinct resolved credentials are required")
			}
			seen := make(map[string]struct{}, len(keys))
			for _, key := range keys {
				if key == "" || key == c.AccessToken {
					return nil, nil, errors.New("distinct resolved credentials are required")
				}
				if _, exists := seen[key]; exists {
					return nil, nil, errors.New("provider API keys must be unique within a key group")
				}
				seen[key] = struct{}{}
			}
		}
	}
	legacySingleProvider := len(c.Providers) == 0
	providers, providerRoutes, err := resolveProviders(c, httpClient)
	if err != nil {
		return nil, nil, err
	}
	if legacySingleProvider {
		for _, provider := range providers {
			c.Protocol = provider.Protocol
			c.APIVersion = provider.APIVersion
		}
	} else {
		c.Providers = providers
	}
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	l, err := ledger.Open(c.LedgerPath)
	if err != nil {
		return nil, nil, errors.New("cannot open ledger")
	}
	stopReconciliation := startStatementSync(c, l)
	gate, _ := limiter.NewConcurrencyGate(c.MaxInFlight)
	idleTTL := time.Duration(c.ActiveSessionIdleTimeoutSeconds) * time.Second
	sessions, err := limiter.NewSessionLimiter(c.MaxActiveSessions, idleTTL)
	if err != nil {
		stopReconciliation()
		_ = l.Close()
		return nil, nil, err
	}
	cache := adapter.NewPromptCache()
	clients := make(map[string]*adapter.Client, len(providers))
	keyPools := make(map[string]*providerKeyPool, len(providers))
	models := make(map[string][]string, len(providers))
	modelsKnown := make(map[string]bool, len(providers))
	activeProviders := make(map[string]bool, len(providerRoutes))
	for _, name := range providerRoutes {
		activeProviders[name] = true
	}
	for name, provider := range providers {
		clients[name] = &adapter.Client{Protocol: provider.Protocol, BaseURL: provider.BaseURL, APIKey: provider.APIKey, APIVersion: provider.APIVersion, Upstream: provider.UpstreamID, Prices: provider.Prices, PromptCache: cache, Limits: c.Limits, MaxOutputTokens: provider.ModelCapabilities.MaxOutputTokens, HTTP: httpClient, Ledger: l, Gate: gate}
		keyPools[name] = newProviderKeyPool(provider.ResolvedAPIKeys())
		if !legacySingleProvider && activeProviders[name] {
			models[name], modelsKnown[name] = discoverProviderModels(provider.BaseURL, provider, provider.Protocol, httpClient)
		}
		if legacySingleProvider && !modelsKnown[name] {
			models[name] = []string{provider.Model}
		}
	}
	return &handler{config: c, ledger: l, sessions: sessions, providers: providers, providerRoutes: providerRoutes, clients: clients, keyPools: keyPools, models: models, modelsKnown: modelsKnown}, func() error {
		stopReconciliation()
		sessions.Close()
		return l.Close()
	}, nil
}

func Open(c Config, client *http.Client) (net.Listener, *http.Server, func() error, error) {
	h, closeLedger, err := NewHandler(c, client)
	if err != nil {
		return nil, nil, nil, err
	}
	listener, err := net.Listen("tcp", c.ListenAddr)
	if err != nil {
		closeLedger()
		return nil, nil, nil, errors.New("cannot bind configured listener")
	}
	return listener, &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}, func() error { listener.Close(); return closeLedger() }, nil
}
