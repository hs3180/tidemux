package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
	"golang.org/x/term"
)

type interactiveProviderSetup struct {
	Name     string
	Provider gateway.Provider
	APIKey   []byte
}

func providerNameFromBaseURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "provider"
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || net.ParseIP(host) != nil {
		return "local-provider"
	}
	labels := strings.Split(host, ".")
	for len(labels) > 1 && (labels[0] == "api" || labels[0] == "www") {
		labels = labels[1:]
	}
	candidate := labels[0]
	var slug strings.Builder
	lastHyphen := false
	for _, r := range candidate {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			slug.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && slug.Len() > 0 {
			slug.WriteByte('-')
			lastHyphen = true
		}
	}
	name := strings.Trim(slug.String(), "-")
	if name == "" {
		return "provider"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

const visibleProviderModels = 20

func chooseProviderModel(in, out *os.File, models []string, modelsKnown bool) (string, error) {
	if modelsKnown {
		if len(models) == 1 {
			fmt.Fprintf(out, "Only one model was discovered; selecting %s.\n", models[0])
			return models[0], nil
		}
		if len(models) == 0 {
			return "", errors.New("the provider returned an empty model list")
		}
		fmt.Fprintln(out, "Available models:")
		for i, model := range models {
			if i == visibleProviderModels {
				fmt.Fprintf(out, "  ... and %d more (enter a model ID to use it)\n", len(models)-visibleProviderModels)
				break
			}
			fmt.Fprintf(out, "  %2d. %s\n", i+1, model)
		}
		for {
			fmt.Fprint(out, "Default model (number or exact ID): ")
			value, err := readTerminalLine(in)
			if err != nil {
				return "", errors.New("could not read default model")
			}
			value = strings.TrimSpace(value)
			for _, model := range models {
				if value == model {
					return model, nil
				}
			}
			if index, parseErr := strconv.Atoi(value); parseErr == nil && index > 0 && index <= visibleProviderModels && index <= len(models) {
				return models[index-1], nil
			}
			fmt.Fprintln(out, "Choose a listed model number or exact model ID.")
		}
	}

	fmt.Fprint(out, "Default model ID (not available from endpoint): ")
	model, err := readTerminalLine(in)
	if err != nil || strings.TrimSpace(model) == "" {
		return "", errors.New("a default model ID is required")
	}
	return strings.TrimSpace(model), nil
}

func chooseProviderModels(in, out *os.File, models []string, modelsKnown bool, defaultModel string) ([]string, error) {
	if modelsKnown && len(models) == 0 {
		return nil, errors.New("the provider returned an empty model list")
	}
	if modelsKnown && len(models) == 1 {
		return nil, nil
	}
	if modelsKnown {
		fmt.Fprintln(out, "Supported-model scope defaults to all models.")
	} else {
		fmt.Fprintln(out, "Model discovery is unavailable; you may enter an optional model allowlist.")
	}
	for {
		if modelsKnown {
			fmt.Fprintf(out, "Supported models [all] (comma-separated numbers or IDs; include default model %q): ", defaultModel)
		} else {
			fmt.Fprintf(out, "Supported model IDs [all] (comma-separated IDs; include default model %q): ", defaultModel)
		}
		value, err := readTerminalLine(in)
		if err != nil {
			return nil, errors.New("could not read supported model scope")
		}
		value = strings.TrimSpace(value)
		if value == "" || strings.EqualFold(value, "all") {
			return nil, nil
		}
		selected, err := parseSupportedModelSelection(value, models, modelsKnown)
		if err != nil {
			fmt.Fprintf(out, "%v\n", err)
			continue
		}
		if !containsConfiguredModel(selected, defaultModel) {
			fmt.Fprintf(out, "The supported-model list must include default model %q.\n", defaultModel)
			continue
		}
		return selected, nil
	}
}

func containsConfiguredModel(models []string, model string) bool {
	for _, candidate := range models {
		if candidate == model {
			return true
		}
	}
	return false
}

func parseSupportedModelSelection(value string, models []string, modelsKnown bool) ([]string, error) {
	selected := make(map[string]struct{})
	for _, raw := range strings.Split(value, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			return nil, errors.New("enter model IDs separated by commas")
		}
		model := ""
		if modelsKnown {
			for _, candidate := range models {
				if candidate == item {
					model = candidate
					break
				}
			}
			if model == "" {
				if index, err := strconv.Atoi(item); err == nil && index > 0 && index <= visibleProviderModels && index <= len(models) {
					model = models[index-1]
				}
			}
			if model == "" {
				return nil, fmt.Errorf("%q is not a discovered model number or ID", item)
			}
		} else {
			model = item
		}
		selected[model] = struct{}{}
	}
	if modelsKnown {
		if len(selected) == len(models) {
			return nil, nil
		}
		ordered := make([]string, 0, len(selected))
		for _, model := range models {
			if _, ok := selected[model]; ok {
				ordered = append(ordered, model)
			}
		}
		return ordered, nil
	}
	ordered := make([]string, 0, len(selected))
	for _, raw := range strings.Split(value, ",") {
		model := strings.TrimSpace(raw)
		if _, ok := selected[model]; ok {
			ordered = append(ordered, model)
			delete(selected, model)
		}
	}
	return ordered, nil
}

func promptProviderProtocol(in, out *os.File) (string, error) {
	for {
		fmt.Fprint(out, "Provider protocol could not be inferred. Enter openai or anthropic: ")
		value, err := readTerminalLine(in)
		if err != nil {
			return "", errors.New("could not read provider protocol")
		}
		protocol := strings.ToLower(strings.TrimSpace(value))
		if protocol == "openai" || protocol == "anthropic" {
			return protocol, nil
		}
		fmt.Fprintln(out, "Provider protocol must be openai or anthropic.")
	}
}

func promptNewProvider(in, out *os.File, anthropicVersion string, httpClient *http.Client) (interactiveProviderSetup, error) {
	var baseURL string
	for {
		fmt.Fprint(out, "Provider API Base URL: ")
		value, err := readTerminalLine(in)
		if err != nil {
			return interactiveProviderSetup{}, errors.New("could not read provider API Base URL")
		}
		baseURL = strings.TrimSpace(value)
		if baseURL == "" {
			fmt.Fprintln(out, "Provider API Base URL is required.")
			continue
		}
		if err := gateway.ValidateProviderBaseURL(baseURL); err != nil {
			fmt.Fprintf(out, "Invalid provider API Base URL: %v\n", err)
			continue
		}
		break
	}
	name := providerNameFromBaseURL(baseURL)
	fmt.Fprintf(out, "Provider name inferred from endpoint: %s\n", name)
	var apiKey []byte
	for {
		fmt.Fprint(out, "Provider API key (hidden): ")
		var err error
		apiKey, err = term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return interactiveProviderSetup{}, errors.New("could not read provider API key")
		}
		if strings.TrimSpace(string(apiKey)) != "" && !strings.ContainsAny(string(apiKey), "\r\n\x00") {
			break
		}
		for i := range apiKey {
			apiKey[i] = 0
		}
		fmt.Fprintln(out, "A valid provider API key is required.")
	}

	var err error
	info, inspectErr := gateway.InspectProviderEndpoint(context.Background(), baseURL, string(apiKey), anthropicVersion, httpClient)
	protocol := ""
	models := []string(nil)
	modelsKnown := false
	if inspectErr != nil {
		fmt.Fprintf(out, "Automatic protocol detection was unavailable: %v\n", inspectErr)
		protocol, err = promptProviderProtocol(in, out)
		if err != nil {
			for i := range apiKey {
				apiKey[i] = 0
			}
			return interactiveProviderSetup{}, err
		}
		models, modelsKnown = gateway.DiscoverProviderModels(baseURL, string(apiKey), anthropicVersion, protocol, httpClient)
	} else {
		protocol, models, modelsKnown = info.Protocol, info.Models, info.ModelsKnown
		fmt.Fprintf(out, "Detected upstream protocol: %s\n", protocol)
		if !modelsKnown {
			fmt.Fprintln(out, "Model discovery is unavailable; the model ID will be entered manually.")
		}
	}
	model, err := chooseProviderModel(in, out, models, modelsKnown)
	if err != nil {
		for i := range apiKey {
			apiKey[i] = 0
		}
		return interactiveProviderSetup{}, err
	}
	supportedModels, err := chooseProviderModels(in, out, models, modelsKnown, model)
	if err != nil {
		for i := range apiKey {
			apiKey[i] = 0
		}
		return interactiveProviderSetup{}, err
	}
	version := ""
	if protocol == "anthropic" {
		version = anthropicVersion
	}
	return interactiveProviderSetup{
		Name: name,
		Provider: gateway.Provider{
			Protocol: protocol, BaseURL: baseURL, APIVersion: version,
			Model: model, SupportedModels: supportedModels, UpstreamID: name,
		},
		APIKey: apiKey,
	}, nil
}
