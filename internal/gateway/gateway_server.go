package gateway

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
	"github.com/hs3180/tidemux/internal/observability"
	"github.com/hs3180/tidemux/internal/usage"
)

// NewHandler resolves providers and initializes the gateway's shared resources.
func NewHandler(c Config, httpClient *http.Client) (http.Handler, func() error, error) {
	return NewHandlerWithLogger(c, httpClient, nil)
}

// NewHandlerWithLogger resolves providers and initializes shared resources
// with a caller-owned structured runtime logger.
func NewHandlerWithLogger(c Config, httpClient *http.Client, logger *slog.Logger) (http.Handler, func() error, error) {
	logger = observability.LoggerOrDiscard(logger)
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
	providers, unavailableProviders, err := resolveProvidersPartial(c, httpClient)
	if err != nil {
		return nil, nil, err
	}
	unavailableNames := make([]string, 0, len(unavailableProviders))
	for name := range unavailableProviders {
		unavailableNames = append(unavailableNames, name)
	}
	sort.Strings(unavailableNames)
	if len(unavailableProviders) == len(providers) {
		return nil, nil, errors.New("no provider protocol could be resolved for " + strings.Join(unavailableNames, ", ") + "; set it with `tidemux provider update REF --protocol openai|anthropic`")
	}
	for _, name := range unavailableNames {
		logger.Warn("provider protocol could not be resolved",
			slog.Int("schema_version", observability.SchemaVersion),
			slog.String("event", "provider_unavailable"),
			slog.String("provider_ref", name),
			slog.String("recovery_command", "tidemux provider update REF --protocol openai|anthropic"),
		)
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
	l, err := ledger.OpenForGateway(c.LedgerPath)
	if err != nil {
		return nil, nil, errors.New("cannot open ledger")
	}
	stopReconciliation := startStatementSync(c, l, logger)
	var usageSigner *usage.Signer
	if c.UsageLog != nil && c.UsageLog.Enabled {
		usageSigner, _ = usage.OpenSigner(c.LedgerPath)
	}
	stopUsage := startUsageExport(c, usageSigner, logger)
	gate, _ := limiter.NewConcurrencyGate(c.MaxInFlight)
	idleTTL := time.Duration(c.ActiveSessionIdleTimeoutSeconds) * time.Second
	sessions, err := limiter.NewSessionLimiter(c.MaxActiveSessions, idleTTL)
	if err != nil {
		stopReconciliation()
		stopUsage()
		_ = l.Close()
		return nil, nil, err
	}
	autoChain := newAutoChainState(c.AutoChain, idleTTL)
	cache := adapter.NewPromptCache()
	clients := make(map[string]*adapter.Client, len(providers))
	keyPools := make(map[string]*providerKeyPool, len(providers))
	models := make(map[string][]string, len(providers))
	modelsKnown := make(map[string]bool, len(providers))
	runtime := &handlerRuntime{usageSigner: usageSigner, budgetBlocked: map[string]struct{}{}, gate: gate, cache: cache}
	providerGenerations := make(map[string]uint64, len(providers))
	for name, provider := range providers {
		if _, unavailable := unavailableProviders[name]; unavailable {
			continue
		}
		runtime.nextProviderGeneration++
		providerGenerations[name] = runtime.nextProviderGeneration
		clients[name] = &adapter.Client{Protocol: provider.Protocol, BaseURL: provider.BaseURL, APIKey: provider.APIKey, APIVersion: provider.APIVersion, Upstream: provider.UpstreamID, ProviderRef: name, Logger: logger, Prices: provider.Prices, ErrorCodeMappings: provider.ErrorCodeMappings, PromptCache: cache, CacheNamespace: providerCacheNamespace(providerGenerations[name]), Limits: c.Limits, MaxOutputTokens: provider.ModelCapabilities.MaxOutputTokens, HTTP: httpClient, Ledger: l, Gate: gate}
		keyPools[name] = newProviderKeyPool(provider.ResolvedAPIKeys())
		if !legacySingleProvider {
			models[name], modelsKnown[name] = discoverProviderModels(provider.BaseURL, provider, provider.Protocol, httpClient)
		}
	}
	h := &handler{config: c, ledger: l, handlerRuntime: runtime, sessions: sessions, autoChain: autoChain, providers: providers, clients: clients, keyPools: keyPools, models: models, modelsKnown: modelsKnown, unavailableProviders: unavailableProviders, logger: logger, providerGenerations: providerGenerations}
	return h, func() error {
		stopUsage()
		stopReconciliation()
		h.closeProviderSessions()
		sessions.Close()
		cache.Close()
		return l.Close()
	}, nil
}

func Open(c Config, client *http.Client) (net.Listener, *http.Server, func() error, error) {
	return OpenWithLogger(c, client, nil)
}

// OpenWithLogger opens the gateway server and routes all runtime diagnostics
// through the provided structured logger.
func OpenWithLogger(c Config, client *http.Client, logger *slog.Logger) (net.Listener, *http.Server, func() error, error) {
	logger = observability.LoggerOrDiscard(logger)
	h, closeLedger, err := NewHandlerWithLogger(c, client, logger)
	if err != nil {
		return nil, nil, nil, err
	}
	listener, err := net.Listen("tcp", c.ListenAddr)
	if err != nil {
		closeLedger()
		return nil, nil, nil, errors.New("cannot bind configured listener")
	}
	server := &http.Server{Handler: h, ErrorLog: observability.HTTPServerErrorLogger(logger), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	server.RegisterOnShutdown(h.(*handler).beginShutdown)
	return listener, server, func() error { listener.Close(); return closeLedger() }, nil
}
