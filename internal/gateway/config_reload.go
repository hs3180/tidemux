package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/observability"
)

const configPollInterval = time.Second
const configReloadTimeout = 10 * time.Second

type configReload struct {
	active          atomic.Pointer[handler]
	mu              sync.Mutex
	path            string
	appliedRevision string
	checkedAt       time.Time
	errorCode       string
}

// ConfigRevision hashes only persisted configuration (never resolved secrets).
func ConfigRevision(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return configDataRevision(data)
}

func (s *configReload) applicationStatus() map[string]any {
	desired := ConfigRevision(s.path)
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "pending"
	if desired != "" && desired == s.appliedRevision && s.errorCode == "" {
		status = "applied"
	}
	return map[string]any{"status": status, "applied_revision": s.appliedRevision, "error_code": s.errorCode,
		"credentials_checked_at": s.checkedAt.UTC().Format(time.RFC3339Nano), "poll_interval_seconds": 1, "reload_timeout_seconds": 10}
}

func configDataRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// WatchConfig prepares a complete view, then swaps one pointer. The caller must
// stop the watcher before closing the handler's shared resources.
func WatchConfig(h http.Handler, path string, original Config, lookup SecretLookup, client *http.Client) func() {
	root := h.(*handler)
	state := &configReload{path: path, checkedAt: time.Now()}
	root.reload = state
	state.active.Store(root)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(configPollInterval)
		defer ticker.Stop()
		activeRaw := original
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			data, err := os.ReadFile(path)
			revision := ""
			var candidate Config
			if err == nil {
				revision = configDataRevision(data)
				candidate, err = decodeValidatedConfig(data)
			}
			code := "config_validation_failed"
			loadCtx, loadCancel := context.WithTimeout(ctx, configReloadTimeout)
			if err == nil {
				code = "config_credentials_unavailable"
				candidate, err = candidate.ResolveCredentials(loadCtx, lookup)
			}
			var view *handler
			if err == nil {
				code = "config_requires_restart"
				if !reflect.DeepEqual(runtimeSettings(candidate), runtimeSettings(activeRaw)) {
					err = errors.New(code)
				}
			}
			if err == nil && !reflect.DeepEqual(candidate, activeRaw) {
				code = "config_provider_probe_failed"
				view, err = root.prepareConfigView(loadCtx, candidate, activeRaw, state.active.Load(), client)
			}
			if err == nil && (loadCtx.Err() != nil || revision == "" || revision != ConfigRevision(path)) {
				code = "config_changed_during_load"
				err = errors.New(code)
			}
			loadCancel()
			if ctx.Err() != nil {
				return
			}
			state.mu.Lock()
			previousError := state.errorCode
			if err != nil {
				state.errorCode = code
			} else {
				if view != nil {
					view.reload = state
					view.activateRouting()
					state.active.Store(view)
				}
				activeRaw = candidate
				state.appliedRevision = revision
				state.checkedAt = time.Now()
				state.errorCode = ""
			}
			state.mu.Unlock()
			if err != nil && previousError != code {
				root.logger.Warn("configuration remains pending; last valid view retained", slog.Int("schema_version", observability.SchemaVersion), slog.String("event", "config_reload"), slog.String("outcome", "error"), slog.String("error_code", code))
			} else if err == nil && (view != nil || previousError != "") {
				root.logger.Info("configuration applied", slog.Int("schema_version", observability.SchemaVersion), slog.String("event", "config_reload"), slog.String("outcome", "success"))
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}

// Process-wide settings keep their existing startup semantics. Provider,
// routing and auto-chain data belong to each request's immutable view.
func runtimeSettings(c Config) Config {
	c.Providers = nil
	c.Routing = nil
	c.AutoChain = nil
	c.BaseURL = ""
	c.Protocol = ""
	c.APIKey = ""
	c.UpstreamKeychain = KeychainReference{}
	c.APIVersion = ""
	c.UpstreamID = ""
	c.Prices = nil
	c.ModelCapabilities = ModelCapabilities{}
	c.Model = ""
	return c
}

func (root *handler) prepareConfigView(ctx context.Context, c, previousRaw Config, previous *handler, client *http.Client) (*handler, error) {
	view := &handler{config: c, ledger: root.ledger, handlerRuntime: root.handlerRuntime, sessions: root.sessions, logger: root.logger,
		autoChain: previous.autoChain, providers: map[string]Provider{}, clients: map[string]*adapter.Client{}, keyPools: map[string]*providerKeyPool{}, models: map[string][]string{}, modelsKnown: map[string]bool{}, unavailableProviders: map[string]error{},
		providerGenerations: map[string]uint64{}, reusedConnections: map[string]bool{}, routingEpoch: previous.routingEpoch + 1}
	rawProviders := c.Providers
	if len(rawProviders) == 0 && c.BaseURL != "" {
		rawProviders = map[string]Provider{"legacy": {Protocol: c.Protocol, BaseURL: c.BaseURL, APIKey: c.APIKey, UpstreamKeychain: c.UpstreamKeychain, APIVersion: c.APIVersion, UpstreamID: c.UpstreamID, ModelCapabilities: c.ModelCapabilities, Prices: c.Prices}}
	}
	previousProviders := previousRaw.Providers
	if len(previousProviders) == 0 && previousRaw.BaseURL != "" {
		previousProviders = map[string]Provider{"legacy": {Protocol: previousRaw.Protocol, BaseURL: previousRaw.BaseURL, APIKey: previousRaw.APIKey, UpstreamKeychain: previousRaw.UpstreamKeychain, APIVersion: previousRaw.APIVersion, UpstreamID: previousRaw.UpstreamID, ModelCapabilities: previousRaw.ModelCapabilities, Prices: previousRaw.Prices}}
	}
	for name, raw := range rawProviders {
		if before, ok := previousProviders[name]; ok && reflect.DeepEqual(raw, before) && previous.clients[name] != nil {
			view.providers[name] = previous.providers[name]
			view.clients[name] = previous.clients[name]
			view.keyPools[name] = previous.keyPools[name]
			view.models[name] = previous.models[name]
			view.modelsKnown[name] = previous.modelsKnown[name]
			view.providerGenerations[name] = previous.providerGenerations[name]
			view.reusedConnections[name] = true
			continue
		}
		p := raw
		protocol := normalizeProviderProtocol(p.Protocol)
		var models []string
		var known bool
		var err error
		before, existed := previousProviders[name]
		prior := previous.providers[name]
		sameConnection := existed && previous.clients[name] != nil && before.BaseURL == raw.BaseURL && normalizeProviderProtocol(before.Protocol) == normalizeProviderProtocol(raw.Protocol) && before.APIVersion == raw.APIVersion && reflect.DeepEqual(before.ResolvedAPIKeys(), raw.ResolvedAPIKeys())
		if sameConnection {
			protocol = prior.Protocol
			p.APIVersion = prior.APIVersion
			models, known = previous.models[name], previous.modelsKnown[name]
			view.providerGenerations[name] = previous.providerGenerations[name]
			view.reusedConnections[name] = true
		} else if protocol == "" || protocol == "auto" {
			protocol, models, known, err = detectProviderEndpoint(ctx, p.BaseURL, p.APIKey, p.APIVersion, client)
		} else {
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			response, probeErr := requestProviderModels(probeCtx, p.BaseURL, p.APIKey, p.APIVersion, protocol, client)
			cancel()
			if probeErr != nil {
				err = errors.New("provider probe failed")
			} else if response.status == http.StatusNotFound || response.status == http.StatusMethodNotAllowed {
				// Explicit protocol profiles already support endpoints without
				// model discovery. Configured scope remains authoritative.
			} else if response.status < 200 || response.status >= 300 {
				err = errors.New("provider probe failed")
			} else {
				models, known = parseProviderModels(response.body, protocol)
			}
		}
		if err != nil || ctx.Err() != nil {
			return nil, errors.New("provider probe failed")
		}
		p.Protocol = protocol
		if protocol == "anthropic" && p.APIVersion == "" {
			p.APIVersion = defaultAnthropicAPIVersion
		}
		if p.UpstreamID == "" {
			p.UpstreamID = name
		}
		view.providers[name] = p
		if !sameConnection {
			root.nextProviderGeneration++
			view.providerGenerations[name] = root.nextProviderGeneration
		}
		view.models[name], view.modelsKnown[name] = models, known
		pool := newProviderKeyPool(p.ResolvedAPIKeys())
		if old, ok := previous.providers[name]; ok && old.BaseURL == p.BaseURL && old.Protocol == p.Protocol && reflect.DeepEqual(old.ResolvedAPIKeys(), p.ResolvedAPIKeys()) {
			pool = previous.keyPools[name]
		}
		view.keyPools[name] = pool
		view.clients[name] = &adapter.Client{Protocol: p.Protocol, BaseURL: p.BaseURL, APIKey: p.APIKey, APIVersion: p.APIVersion, Upstream: p.UpstreamID, ProviderRef: name, Logger: root.logger, UsageLog: root.usageLog, Prices: p.Prices, ErrorCodeMappings: p.ErrorCodeMappings, AssistantToolResultPolicy: p.AssistantToolResultPolicy, PromptCache: root.cache, CacheNamespace: providerCacheNamespace(view.providerGenerations[name]), Limits: c.Limits, MaxOutputTokens: p.ModelCapabilities.MaxOutputTokens, HTTP: client, Ledger: root.ledger, Gate: root.gate}
	}
	if len(c.Providers) > 0 {
		view.config.Providers = view.providers
	} else if c.BaseURL != "" {
		view.config.Protocol = view.providers["legacy"].Protocol
		view.config.APIVersion = view.providers["legacy"].APIVersion
	}
	if err := view.config.Validate(); err != nil {
		return nil, err
	}
	view.prepareRouting(previous, !reflect.DeepEqual(c.AutoChain, previousRaw.AutoChain))
	return view, nil
}
