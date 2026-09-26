package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const defaultAnthropicAPIVersion = "2023-06-01"

// normalizeProviderProtocol accepts the values used by current and legacy
// configuration files. New configurations leave the field empty and resolve
// it at gateway startup.
func normalizeProviderProtocol(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func resolveProviderProtocol(c Config, httpClient *http.Client) (string, string, error) {
	protocol := normalizeProviderProtocol(c.Protocol)
	if protocol == "openai" || protocol == "anthropic" {
		return protocol, c.APIVersion, nil
	}
	if protocol != "" && protocol != "auto" {
		return "", "", errors.New("protocol must be auto, openai or anthropic")
	}

	detected, err := detectProviderProtocol(context.Background(), c.BaseURL, c.APIKey, c.APIVersion, httpClient)
	if err != nil {
		return "", "", err
	}
	apiVersion := c.APIVersion
	if detected == "anthropic" {
		if apiVersion == "" {
			apiVersion = defaultAnthropicAPIVersion
		}
		if _, err := time.Parse("2006-01-02", apiVersion); err != nil {
			return "", "", errors.New("anthropic_version must be YYYY-MM-DD")
		}
	}
	return detected, apiVersion, nil
}

// resolveProviders returns named profiles plus the selected profile for each
// client protocol. Legacy configurations keep their historical single-profile
// protocol detection and translation behavior.
func resolveProviders(c Config, httpClient *http.Client) (map[string]Provider, map[string]string, error) {
	if len(c.Providers) != 0 {
		resolved := make(map[string]Provider, len(c.Providers))
		for name, provider := range c.Providers {
			if keys := provider.ResolvedAPIKeys(); len(keys) > 0 {
				provider.APIKey = keys[0]
			}
			protocol := normalizeProviderProtocol(provider.Protocol)
			if protocol == "" || protocol == "auto" {
				var err error
				protocol, err = detectProviderProtocol(context.Background(), provider.BaseURL, provider.APIKey, provider.APIVersion, httpClient)
				if err != nil {
					return nil, nil, errors.New("cannot determine protocol for providers." + name + ": " + err.Error())
				}
			}
			provider.Protocol = protocol
			if protocol == "anthropic" {
				if provider.APIVersion == "" {
					provider.APIVersion = defaultAnthropicAPIVersion
				}
				if _, err := time.Parse("2006-01-02", provider.APIVersion); err != nil {
					return nil, nil, errors.New("providers." + name + ".anthropic_version must be YYYY-MM-DD")
				}
			} else if provider.APIVersion != "" {
				return nil, nil, errors.New("providers." + name + ".anthropic_version is valid only for the anthropic endpoint")
			}
			if provider.UpstreamID == "" {
				provider.UpstreamID = name
			}
			resolved[name] = provider
		}
		routes, err := resolveDefaultProviderRoutes(resolved, c.DefaultProviders)
		if err != nil {
			return nil, nil, err
		}
		return resolved, routes, nil
	}
	if c.BaseURL == "" {
		return nil, nil, errors.New("no upstream provider is configured; run `tidemux provider add`")
	}

	protocol, apiVersion, err := resolveProviderProtocol(c, httpClient)
	if err != nil {
		return nil, nil, err
	}
	name := "legacy"
	provider := Provider{
		Protocol:          protocol,
		BaseURL:           c.BaseURL,
		APIVersion:        apiVersion,
		APIKey:            c.APIKey,
		Model:             c.Model,
		UpstreamID:        c.UpstreamID,
		ModelCapabilities: c.ModelCapabilities,
		Prices:            c.Prices,
	}
	return map[string]Provider{name: provider}, map[string]string{"openai": name, "anthropic": name}, nil
}

func resolveDefaultProviderRoutes(providers map[string]Provider, configured map[string]string) (map[string]string, error) {
	routes := make(map[string]string, 2)
	for protocol, name := range configured {
		if protocol != "openai" && protocol != "anthropic" {
			return nil, errors.New("default_providers keys must be openai or anthropic")
		}
		provider, ok := providers[name]
		if !ok {
			return nil, errors.New("default_providers." + protocol + " must reference a configured provider name")
		}
		if provider.Protocol != protocol {
			return nil, errors.New("default_providers." + protocol + " must reference a provider with the same protocol")
		}
		routes[protocol] = name
	}

	providersByProtocol := map[string][]string{"openai": {}, "anthropic": {}}
	for name, provider := range providers {
		providersByProtocol[provider.Protocol] = append(providersByProtocol[provider.Protocol], name)
	}
	for protocol, names := range providersByProtocol {
		if _, ok := routes[protocol]; ok || len(names) == 0 {
			continue
		}
		if len(names) != 1 {
			sort.Strings(names)
			return nil, errors.New("default_providers." + protocol + " must select one of the providers: " + strings.Join(names, ", "))
		}
		routes[protocol] = names[0]
	}

	// A client without a same-protocol route uses the configured default of the
	// other protocol. This is a single route choice, not request-time failover.
	if routes["openai"] == "" {
		routes["openai"] = routes["anthropic"]
	}
	if routes["anthropic"] == "" {
		routes["anthropic"] = routes["openai"]
	}
	return routes, nil
}

// discoverProviderModels makes a bounded, redirect-free GET /models request.
// A false second result means the endpoint does not expose a complete,
// recognizable model list, so callers must not claim model availability.
func discoverProviderModels(baseURL string, provider Provider, protocol string, httpClient *http.Client) ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := fetchProviderModels(ctx, baseURL, provider.APIKey, provider.APIVersion, protocol, httpClient)
	if err != nil || len(body) == 0 {
		return nil, false
	}
	return parseProviderModels(body, protocol)
}

// ProviderEndpointInfo contains non-secret details inferred from an upstream
// API root. ModelsKnown is false when discovery is unsupported or incomplete.
type ProviderEndpointInfo struct {
	Protocol    string
	Models      []string
	ModelsKnown bool
}

// InspectProviderEndpoint infers the upstream protocol and, when possible,
// returns its complete model list. It only sends GET /models requests; redirects
// are disabled so provider credentials cannot be forwarded to another host.
func InspectProviderEndpoint(ctx context.Context, baseURL, apiKey, apiVersion string, httpClient *http.Client) (ProviderEndpointInfo, error) {
	if err := validateBaseURL(baseURL, "base_url"); err != nil {
		return ProviderEndpointInfo{}, err
	}
	if strings.TrimSpace(apiKey) == "" {
		return ProviderEndpointInfo{}, errors.New("provider API key is required for endpoint inspection")
	}
	if protocol := providerProtocolHint(baseURL); protocol != "" {
		provider := Provider{APIKey: apiKey, APIVersion: apiVersion}
		if protocol == "anthropic" && provider.APIVersion == "" {
			provider.APIVersion = defaultAnthropicAPIVersion
		}
		models, known := discoverProviderModels(baseURL, provider, protocol, httpClient)
		return ProviderEndpointInfo{Protocol: protocol, Models: models, ModelsKnown: known}, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}
	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, protocol := range []string{"openai", "anthropic"} {
		body, err := fetchProviderModels(probeContext, baseURL, apiKey, apiVersion, protocol, httpClient)
		if err != nil {
			continue
		}
		if detected := classifyProviderModels(body); detected != "" {
			models, known := parseProviderModels(body, detected)
			return ProviderEndpointInfo{Protocol: detected, Models: models, ModelsKnown: known}, nil
		}
	}
	return ProviderEndpointInfo{}, errors.New("cannot determine provider API protocol from endpoint or GET /models; choose openai or anthropic explicitly")
}

// DiscoverProviderModels fetches the model catalog using an explicitly chosen
// provider protocol. A false second result means the list is unsupported,
// incomplete or not recognizable.
func DiscoverProviderModels(baseURL, apiKey, apiVersion, protocol string, httpClient *http.Client) ([]string, bool) {
	if protocol == "anthropic" && apiVersion == "" {
		apiVersion = defaultAnthropicAPIVersion
	}
	return discoverProviderModels(baseURL, Provider{APIKey: apiKey, APIVersion: apiVersion}, protocol, httpClient)
}

func parseProviderModels(body []byte, protocol string) ([]string, bool) {
	var envelope struct {
		Object  string `json:"object"`
		HasMore *bool  `json:"has_more"`
		Data    []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return nil, false
	}
	known := false
	if protocol == "openai" {
		known = envelope.Object == "list"
	} else if protocol == "anthropic" {
		known = envelope.HasMore != nil
		for _, item := range envelope.Data {
			if item.Type == "model" {
				known = true
				break
			}
		}
	}
	if !known {
		return nil, false
	}
	// Do not treat a truncated page as an authoritative model set. An
	// incomplete list could incorrectly hide a model or advertise one on the
	// wrong route until the provider's pagination contract is followed.
	if envelope.HasMore != nil && *envelope.HasMore {
		return nil, false
	}
	unique := make(map[string]struct{}, len(envelope.Data))
	models := make([]string, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || len(id) > 256 || strings.ContainsAny(id, "\r\n\x00") {
			continue
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		models = append(models, id)
	}
	sort.Strings(models)
	return models, true
}

func fetchProviderModels(ctx context.Context, baseURL, apiKey, apiVersion, protocol string, httpClient *http.Client) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	switch protocol {
	case "openai":
		request.Header.Set("Authorization", "Bearer "+apiKey)
	case "anthropic":
		request.Header.Set("x-api-key", apiKey)
		if apiVersion == "" {
			apiVersion = defaultAnthropicAPIVersion
		}
		request.Header.Set("anthropic-version", apiVersion)
	default:
		return nil, errors.New("unknown provider protocol")
	}
	client := http.Client{Timeout: 5 * time.Second}
	if httpClient != nil {
		client = *httpClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("provider model response is unreadable or too large")
	}
	return body, nil
}

// detectProviderProtocol identifies the upstream wire format without making a
// billable model request. Official provider roots and explicit compatibility
// paths are resolved locally; generic roots are inspected through GET /models.
func detectProviderProtocol(ctx context.Context, baseURL, apiKey, apiVersion string, httpClient *http.Client) (string, error) {
	if protocol := providerProtocolHint(baseURL); protocol != "" {
		return protocol, nil
	}

	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, auth := range []string{"openai", "anthropic"} {
		protocol, err := probeProviderModels(probeContext, baseURL, apiKey, apiVersion, auth, httpClient)
		if err != nil {
			continue
		}
		if protocol != "" {
			return protocol, nil
		}
	}
	return "", errors.New("cannot determine provider API protocol; the API root must identify an OpenAI/Anthropic endpoint or expose a compatible GET /models response")
}

func providerProtocolHint(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	haystack := strings.ToLower(u.Hostname() + " " + u.Path)
	// A path such as /anthropic/v1 is more specific than a provider hostname
	// and is common for OpenAI-compatible services exposing both APIs.
	if strings.Contains(haystack, "anthropic") {
		return "anthropic"
	}
	if strings.Contains(haystack, "openai") {
		return "openai"
	}
	// DeepSeek's base endpoint is OpenAI-compatible unless its explicit
	// /anthropic/ path matched above. This keeps the endpoint usable even when a
	// provider deployment does not expose model discovery.
	if strings.Contains(haystack, "deepseek") {
		return "openai"
	}
	return ""
}

func probeProviderModels(ctx context.Context, baseURL, apiKey, apiVersion, auth string, httpClient *http.Client) (string, error) {
	body, err := fetchProviderModels(ctx, baseURL, apiKey, apiVersion, auth, httpClient)
	if err != nil {
		return "", err
	}
	if len(body) == 0 {
		return "", nil
	}
	return classifyProviderModels(body), nil
}

func classifyProviderModels(body []byte) string {
	var envelope struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	for _, raw := range envelope.Data {
		var item struct {
			ID          string `json:"id"`
			Object      string `json:"object"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.Type == "model" || item.DisplayName != "" {
			return "anthropic"
		}
		if item.Object == "model" || (item.ID != "" && item.Type == "") {
			return "openai"
		}
	}
	if envelope.Object == "list" {
		return "openai"
	}
	return ""
}
