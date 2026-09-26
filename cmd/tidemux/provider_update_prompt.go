package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
)

type providerUpdateChanges struct {
	Endpoint       string
	Protocol       string
	Model          string
	ModelRequested bool
	Changed        bool
}

func providerModelScopeLabel(models []string) string {
	if len(models) == 0 {
		return "all models"
	}
	if len(models) == 1 {
		return "1 allowed model"
	}
	return fmt.Sprintf("%d allowed models", len(models))
}

func promptProviderUpdate(in, out *os.File, current gateway.Provider) (providerUpdateChanges, error) {
	fmt.Fprintf(out, "Current endpoint: %s\nCurrent protocol: %s\nModel scope: %s\n", current.BaseURL, current.Protocol, providerModelScopeLabel(current.SupportedModels))
	fmt.Fprintln(out, "Manage API keys separately with `provider key add REF`, `provider key list REF`, and `provider key remove REF INDEX`.")
	fmt.Fprintln(out, "Changing the endpoint clears the model allowlist unless you enter a new one below.")
	fmt.Fprint(out, "New endpoint (Enter keeps current): ")
	endpoint, err := readTerminalLine(in)
	if err != nil {
		return providerUpdateChanges{}, errors.New("could not read provider endpoint")
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint != "" {
		if err := gateway.ValidateProviderBaseURL(endpoint); err != nil {
			return providerUpdateChanges{}, fmt.Errorf("invalid provider endpoint: %w", err)
		}
	}
	endpointChanged := endpoint != "" && endpoint != current.BaseURL

	protocolPrompt := "Protocol override (openai or anthropic; Enter keeps current): "
	if endpoint != "" && endpoint != current.BaseURL {
		protocolPrompt = "Protocol override (openai or anthropic; Enter auto-detects): "
	}
	fmt.Fprint(out, protocolPrompt)
	protocol, err := readTerminalLine(in)
	if err != nil {
		return providerUpdateChanges{}, errors.New("could not read provider protocol")
	}
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol != "" && protocol != "openai" && protocol != "anthropic" {
		return providerUpdateChanges{}, errors.New("protocol override must be openai or anthropic")
	}

	modelPrompt := "Allowed models (comma-separated IDs, 'all' for every model; Enter keeps current): "
	if endpointChanged {
		modelPrompt = "Allowed models (comma-separated IDs, 'all' for every model; Enter allows all models): "
	}
	fmt.Fprint(out, modelPrompt)
	modelInput, err := readTerminalLine(in)
	if err != nil {
		return providerUpdateChanges{}, errors.New("could not read provider model scope")
	}
	modelInput = strings.TrimSpace(modelInput)
	changes := providerUpdateChanges{}
	if endpointChanged {
		changes.Endpoint = endpoint
	}
	if protocol != "" && (endpointChanged || protocol != current.Protocol) {
		changes.Protocol = protocol
	}
	if modelInput != "" {
		if isAllProviderModels(modelInput) {
			if len(current.SupportedModels) > 0 {
				changes.Model = "all"
				changes.ModelRequested = true
			}
		} else {
			models, err := parseProviderModelList(modelInput)
			if err != nil || len(models) == 0 {
				if err == nil {
					err = errors.New("enter model IDs or 'all'")
				}
				return providerUpdateChanges{}, fmt.Errorf("invalid model scope: %w", err)
			}
			if endpointChanged || !sameModelScope(models, current.SupportedModels) {
				changes.Model = strings.Join(models, ",")
				changes.ModelRequested = true
			}
		}
	}
	changes.Changed = changes.Endpoint != "" || changes.Protocol != "" || changes.ModelRequested
	return changes, nil
}

func isAllProviderModels(value string) bool {
	value = strings.TrimSpace(value)
	return strings.EqualFold(value, "all") || value == "*"
}
