package main

import (
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

// interactiveProviderSetup holds the provider profile and its in-memory key
// until the Keychain/config transaction commits.
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

func chooseProviderModels(in, out *os.File, models []string, modelsKnown bool) ([]string, error) {
	if modelsKnown {
		if len(models) == 0 {
			return nil, errors.New("the provider returned an empty model list")
		}
		fmt.Fprintln(out, "Available models:")
		for i, model := range models {
			if i == visibleProviderModels {
				fmt.Fprintf(out, "  ... and %d more (enter an exact model ID to allow it)\n", len(models)-visibleProviderModels)
				break
			}
			fmt.Fprintf(out, "  %2d. %s\n", i+1, model)
		}
		fmt.Fprintln(out, "Choose the model IDs this provider may use; leave blank to allow all models.")
		for {
			fmt.Fprint(out, "Allowed models (comma-separated numbers or exact IDs, or all): ")
			value, err := readTerminalLine(in)
			if err != nil {
				return nil, errors.New("could not read allowed models")
			}
			value = strings.TrimSpace(value)
			if value == "" || strings.EqualFold(value, "all") || value == "*" {
				fmt.Fprintln(out, "All models are allowed.")
				return nil, nil
			}
			selected, err := selectDiscoveredProviderModels(value, models)
			if err == nil {
				return selected, nil
			}
			fmt.Fprintf(out, "Invalid model selection: %v. Choose listed numbers or exact model IDs.\n", err)
		}
	}

	for {
		fmt.Fprint(out, "Allowed model IDs (comma-separated, leave blank or enter * for all): ")
		value, err := readTerminalLine(in)
		if err != nil {
			return nil, errors.New("could not read allowed model IDs")
		}
		value = strings.TrimSpace(value)
		if value == "" || strings.EqualFold(value, "all") || value == "*" {
			fmt.Fprintln(out, "All models are allowed.")
			return nil, nil
		}
		allowed, err := parseProviderModelList(value)
		if err == nil && len(allowed) > 0 {
			return allowed, nil
		}
		if err == nil {
			err = errors.New("enter one or more model IDs, or * for all")
		}
		fmt.Fprintf(out, "Invalid model selection: %v. Enter one or more model IDs.\n", err)
	}
}

func parseProviderModelList(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	var models []string
	for _, item := range strings.Split(value, ",") {
		model := strings.TrimSpace(item)
		if model == "" {
			return nil, errors.New("model list contains an empty ID")
		}
		if _, exists := seen[model]; exists {
			return nil, fmt.Errorf("model %q is listed more than once", model)
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return models, nil
}

func selectDiscoveredProviderModels(value string, models []string) ([]string, error) {
	items := strings.Split(value, ",")
	selected := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		selector := strings.TrimSpace(item)
		if selector == "" {
			return nil, errors.New("model selection contains an empty entry")
		}
		var model string
		for _, candidate := range models {
			if selector == candidate {
				model = candidate
				break
			}
		}
		if model == "" {
			index, err := strconv.Atoi(selector)
			if err != nil || index < 1 || index > visibleProviderModels || index > len(models) {
				return nil, fmt.Errorf("%q is not a listed model", selector)
			}
			model = models[index-1]
		}
		if _, exists := seen[model]; exists {
			return nil, fmt.Errorf("model %q is listed more than once", model)
		}
		seen[model] = struct{}{}
		selected = append(selected, model)
	}
	return selected, nil
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

func promptNewProvider(in, out *os.File, anthropicVersion string, httpClient *http.Client, forcedProtocol string, allowedModels []string, promptForModels bool, preferredName string) (interactiveProviderSetup, error) {
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
	if preferredName != "" {
		name = preferredName
	}
	fmt.Fprintf(out, "Provider reference: %s\n", name)
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

	setup, err := inspectAndCompleteProviderWithModels(in, baseURL, string(apiKey), forcedProtocol, allowedModels, promptForModels, anthropicVersion, httpClient)
	if err != nil {
		zeroBytes(apiKey)
		return interactiveProviderSetup{}, err
	}
	setup.Name = name
	setup.Provider.UpstreamID = name
	setup.APIKey = apiKey
	return setup, nil
}
