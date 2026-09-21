package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const maxModelDiscoveryBytes = 1 << 20

// ModelInfo is the provider's stable model identifier and optional display
// name. It intentionally omits provider-specific fields.
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

// DiscoverModels fetches a provider's model list without exposing response
// bodies or credentials in returned errors.
func DiscoverModels(ctx context.Context, protocol, baseURL, apiKey, apiVersion string, httpClient *http.Client) ([]ModelInfo, error) {
	if protocol != "openai" && protocol != "anthropic" {
		return nil, errors.New("model discovery requires openai or anthropic protocol")
	}
	parsed, err := validateDiscoveryURL(baseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(apiKey) == "" || strings.ContainsAny(apiKey, "\r\n\x00") {
		return nil, errors.New("model discovery credential is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(parsed.String(), "/")+"/models", nil)
	if err != nil {
		return nil, errors.New("cannot prepare model discovery request")
	}
	if protocol == "anthropic" {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", apiVersion)
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := http.Client{}
	if httpClient != nil {
		client = *httpClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("model discovery request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("model discovery returned a non-success status")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxModelDiscoveryBytes+1))
	if err != nil || len(raw) > maxModelDiscoveryBytes {
		return nil, errors.New("model discovery response is too large")
	}
	var envelope struct {
		Data []ModelInfo `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, errors.New("model discovery response is invalid")
	}
	models := make([]ModelInfo, 0, len(envelope.Data))
	seen := make(map[string]struct{}, len(envelope.Data))
	for _, model := range envelope.Data {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		if _, ok := seen[model.ID]; ok {
			continue
		}
		seen[model.ID] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, errors.New("model discovery returned no models")
	}
	return models, nil
}

func validateDiscoveryURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, errors.New("model discovery base URL must be an API root without credentials, query or fragment")
	}
	if parsed.Scheme != "https" {
		ip := net.ParseIP(parsed.Hostname())
		if parsed.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("model discovery base URL requires HTTPS, except loopback HTTP")
		}
	}
	return parsed, nil
}
