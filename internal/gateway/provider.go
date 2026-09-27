package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
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

// resolveProviders returns named profiles. Requests select a profile through
// their explicit provider/model ID rather than a protocol-level default.
func resolveProviders(c Config, httpClient *http.Client) (map[string]Provider, error) {
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
					return nil, errors.New("cannot determine protocol for providers." + name + ": " + err.Error())
				}
			}
			provider.Protocol = protocol
			if protocol == "anthropic" {
				if provider.APIVersion == "" {
					provider.APIVersion = defaultAnthropicAPIVersion
				}
				if _, err := time.Parse("2006-01-02", provider.APIVersion); err != nil {
					return nil, errors.New("providers." + name + ".anthropic_version must be YYYY-MM-DD")
				}
			} else if provider.APIVersion != "" {
				return nil, errors.New("providers." + name + ".anthropic_version is valid only for the anthropic endpoint")
			}
			if provider.UpstreamID == "" {
				provider.UpstreamID = name
			}
			resolved[name] = provider
		}
		return resolved, nil
	}
	if c.BaseURL == "" {
		return nil, errors.New("no upstream provider is configured; run `tidemux provider add`")
	}

	protocol, apiVersion, err := resolveProviderProtocol(c, httpClient)
	if err != nil {
		return nil, err
	}
	name := "legacy"
	provider := Provider{
		Protocol:          protocol,
		BaseURL:           c.BaseURL,
		APIVersion:        apiVersion,
		APIKey:            c.APIKey,
		UpstreamID:        c.UpstreamID,
		ModelCapabilities: c.ModelCapabilities,
		Prices:            c.Prices,
	}
	return map[string]Provider{name: provider}, nil
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
	if ctx == nil {
		ctx = context.Background()
	}
	protocol, models, known, err := detectProviderEndpoint(ctx, baseURL, apiKey, apiVersion, httpClient)
	if err != nil {
		return ProviderEndpointInfo{}, err
	}
	return ProviderEndpointInfo{Protocol: protocol, Models: models, ModelsKnown: known}, nil
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

type providerModelsResponse struct {
	status      int
	contentType string
	body        []byte
}

func requestProviderModels(ctx context.Context, baseURL, apiKey, apiVersion, protocol string, httpClient *http.Client) (providerModelsResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return providerModelsResponse{}, err
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
		return providerModelsResponse{}, errors.New("unknown provider protocol")
	}
	client := http.Client{Timeout: 5 * time.Second}
	if httpClient != nil {
		client = *httpClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return providerModelsResponse{}, err
	}
	defer response.Body.Close()
	result := providerModelsResponse{status: response.StatusCode, contentType: response.Header.Get("Content-Type")}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return result, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return providerModelsResponse{}, errors.New("provider model response is unreadable or too large")
	}
	result.body = body
	return result, nil
}

func fetchProviderModels(ctx context.Context, baseURL, apiKey, apiVersion, protocol string, httpClient *http.Client) ([]byte, error) {
	response, err := requestProviderModels(ctx, baseURL, apiKey, apiVersion, protocol, httpClient)
	if err != nil || response.status < http.StatusOK || response.status >= http.StatusMultipleChoices {
		return nil, err
	}
	return response.body, nil
}

// detectProviderProtocol identifies the upstream wire format from bounded,
// protocol-authenticated capability requests without generating billable text.
func detectProviderProtocol(ctx context.Context, baseURL, apiKey, apiVersion string, httpClient *http.Client) (string, error) {
	protocol, _, _, err := detectProviderEndpoint(ctx, baseURL, apiKey, apiVersion, httpClient)
	return protocol, err
}

func detectProviderEndpoint(ctx context.Context, baseURL, apiKey, apiVersion string, httpClient *http.Client) (string, []string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	type evidence struct {
		protocol string
		models   []string
		known    bool
	}
	confirmed := make(map[string]evidence)
	for _, auth := range []string{"openai", "anthropic"} {
		response, err := requestProviderModels(probeContext, baseURL, apiKey, apiVersion, auth, httpClient)
		if err != nil || response.status < http.StatusOK || response.status >= http.StatusMultipleChoices || len(response.body) == 0 || !isJSONMediaType(response.contentType) {
			continue
		}
		openAI, anthropic := classifyProviderModelSemantics(response.body)
		if openAI != anthropic {
			protocol := "anthropic"
			if openAI {
				protocol = "openai"
			}
			models, known := parseProviderModels(response.body, protocol)
			confirmed[protocol] = evidence{protocol: protocol, models: models, known: known}
		}
	}
	if len(confirmed) == 1 {
		for _, result := range confirmed {
			return result.protocol, result.models, result.known, nil
		}
	}
	return "", nil, false, errors.New("cannot determine one provider API protocol from authenticated GET /models probes; set --protocol openai or --protocol anthropic")
}

func isJSONMediaType(value string) bool {
	if strings.TrimSpace(value) == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"))
}

func classifyProviderModelSemantics(body []byte) (openAI, anthropic bool) {
	var envelope struct {
		Object  string            `json:"object"`
		HasMore *bool             `json:"has_more"`
		Data    []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false, false
	}
	if envelope.Data == nil {
		return false, false
	}
	// Anthropic model-list pages carry has_more and model-typed records;
	// OpenAI lists use object=list and model objects without has_more.
	openAIItemsValid, anthropicItemsValid := true, true
	for _, raw := range envelope.Data {
		var item struct {
			ID     string `json:"id"`
			Object string `json:"object"`
			Type   string `json:"type"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return false, false
		}
		if item.ID == "" {
			return false, false
		}
		if item.Object != "model" {
			openAIItemsValid = false
		}
		if item.Type != "model" {
			anthropicItemsValid = false
		}
	}
	openAI = envelope.Object == "list" && envelope.HasMore == nil && openAIItemsValid
	anthropic = envelope.HasMore != nil && anthropicItemsValid
	return openAI, anthropic
}
