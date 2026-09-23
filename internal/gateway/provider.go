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

// resolveProviders returns the independent provider profiles used by the
// gateway. Legacy profiles still resolve their single API root exactly as
// before; named providers have a protocol supplied by their map key.
func resolveProviders(c Config, httpClient *http.Client) (map[string]Provider, error) {
	if len(c.Providers) != 0 {
		resolved := make(map[string]Provider, len(c.Providers))
		for protocol, provider := range c.Providers {
			if protocol == "anthropic" {
				if provider.APIVersion == "" {
					provider.APIVersion = defaultAnthropicAPIVersion
				}
				if _, err := time.Parse("2006-01-02", provider.APIVersion); err != nil {
					return nil, errors.New("providers.anthropic.anthropic_version must be YYYY-MM-DD")
				}
			}
			if provider.UpstreamID == "" {
				provider.UpstreamID = protocol
			}
			resolved[protocol] = provider
		}
		return resolved, nil
	}

	protocol, apiVersion, err := resolveProviderProtocol(c, httpClient)
	if err != nil {
		return nil, err
	}
	return map[string]Provider{
		protocol: {
			BaseURL:           c.BaseURL,
			APIVersion:        apiVersion,
			APIKey:            c.APIKey,
			Model:             c.Model,
			UpstreamID:        c.UpstreamID,
			ModelCapabilities: c.ModelCapabilities,
			Prices:            c.Prices,
		},
	}, nil
}

// discoverProviderModels makes a bounded, redirect-free GET /models request.
// A false second result means the endpoint does not expose a complete,
// recognizable model list, so callers must not claim model availability.
func discoverProviderModels(endpoint Provider, protocol string, httpClient *http.Client) ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, false
	}
	if protocol == "anthropic" {
		request.Header.Set("x-api-key", endpoint.APIKey)
		request.Header.Set("anthropic-version", endpoint.APIVersion)
	} else {
		request.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
	}
	client := http.Client{Timeout: 5 * time.Second}
	if httpClient != nil {
		client = *httpClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, false
	}
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
	} else {
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
	// The built-in DeepSeek preset is OpenAI-compatible unless its explicit
	// /anthropic/ path matched above. This keeps the preset usable even when a
	// provider deployment does not expose model discovery.
	if strings.Contains(haystack, "deepseek") {
		return "openai"
	}
	return ""
}

func probeProviderModels(ctx context.Context, baseURL, apiKey, apiVersion, auth string, httpClient *http.Client) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return "", err
	}
	switch auth {
	case "openai":
		request.Header.Set("Authorization", "Bearer "+apiKey)
	case "anthropic":
		request.Header.Set("x-api-key", apiKey)
		if apiVersion == "" {
			apiVersion = defaultAnthropicAPIVersion
		}
		request.Header.Set("anthropic-version", apiVersion)
	default:
		return "", errors.New("unknown provider probe authentication")
	}
	client := http.Client{Timeout: 5 * time.Second}
	if httpClient != nil {
		client = *httpClient
	}
	// Provider discovery must never forward the API key through a redirect.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
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
