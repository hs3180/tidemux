package main

import (
	"errors"
	"sort"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
)

type repeatedFlag []string

func (values *repeatedFlag) String() string { return strings.Join(*values, ",") }
func (values *repeatedFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func parseProviderFlag(value string) (string, gateway.Provider, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 3 && len(parts) != 4 {
		return "", gateway.Provider{}, errors.New("--provider must be NAME,BASE_URL,MODEL (automatic) or NAME,PROTOCOL,BASE_URL,MODEL (forced protocol)")
	}
	name, protocol := strings.TrimSpace(parts[0]), "auto"
	baseURL, model := strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
	if len(parts) == 4 {
		protocol = strings.ToLower(strings.TrimSpace(parts[1]))
		baseURL, model = strings.TrimSpace(parts[2]), strings.TrimSpace(parts[3])
	}
	return name, gateway.Provider{Protocol: protocol, BaseURL: baseURL, Model: model, UpstreamID: name}, nil
}

func parseDefaultProviderFlag(value string) (string, string, error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return "", "", errors.New("--default-provider must be PROTOCOL=NAME")
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func parseProviderModelsFlag(value string) (string, []string, error) {
	parts := strings.Split(value, ",")
	if len(parts) < 2 {
		return "", nil, errors.New("--provider-models must be NAME,MODEL[,MODEL...]")
	}
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return "", nil, errors.New("--provider-models requires a provider name")
	}
	models := make([]string, 0, len(parts)-1)
	seen := make(map[string]struct{}, len(parts)-1)
	for _, part := range parts[1:] {
		model := strings.TrimSpace(part)
		if model == "" {
			return "", nil, errors.New("--provider-models contains an empty model ID")
		}
		if _, exists := seen[model]; exists {
			return "", nil, errors.New("--provider-models contains a duplicate model ID")
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return name, models, nil
}

func sortedProviderNames(providers map[string]gateway.Provider) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func formatDefaultProviderRoutes(routes map[string]string) string {
	ordered := make([]string, 0, len(routes))
	for _, protocol := range []string{"openai", "anthropic"} {
		if name, ok := routes[protocol]; ok {
			ordered = append(ordered, protocol+"="+name)
		}
	}
	return strings.Join(ordered, ", ")
}
