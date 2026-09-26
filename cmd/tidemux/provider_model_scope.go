package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
)

func providerModelsSelect(ref, configPath string, stdout *os.File) error {
	if runtime.GOOS != "darwin" {
		return errors.New("model catalog selection requires macOS Keychain")
	}
	config, abs, before, err := loadCommandConfig(configPath)
	if err != nil {
		return err
	}
	config, err = migrateLegacyProvider(config, nil)
	if err != nil {
		return err
	}
	provider, ok := config.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	tty, err := openControlTTY("interactive model selection requires a terminal")
	if err != nil {
		return err
	}
	defer tty.Close()
	if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
		return err
	}
	refs, err := provider.KeychainReferences()
	if err != nil {
		return errors.New("provider Keychain references are invalid")
	}
	key, err := (gateway.MacOSKeychain{}).Lookup(context.Background(), refs[0])
	if err != nil {
		return errors.New("provider Keychain item unavailable")
	}
	var models []string
	var known bool
	if provider.Protocol == "" || provider.Protocol == "auto" {
		info, inspectErr := gateway.InspectProviderEndpoint(context.Background(), provider.BaseURL, key, provider.APIVersion, nil)
		if inspectErr == nil {
			models, known = info.Models, info.ModelsKnown
		}
	} else {
		models, known = gateway.DiscoverProviderModels(provider.BaseURL, key, provider.APIVersion, provider.Protocol, nil)
	}
	if !known {
		fmt.Fprintln(tty, "Model discovery is unavailable; you can enter model IDs manually.")
	} else if len(models) == 0 {
		fmt.Fprintln(tty, "The provider returned an empty model catalog; you can still enter model IDs manually.")
	}
	selected, changed, err := promptModelScope(tty, tty, models, known, provider.SupportedModels)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintln(stdout, "Model scope unchanged.")
		return nil
	}
	if err := saveProviderModelScope(abs, config, before, ref, selected); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Updated provider %s model scope: %s.\n", ref, providerModelScopeLabel(selected))
	return nil
}

func saveProviderModelScope(path string, config gateway.Config, before []byte, ref string, models []string) error {
	provider, ok := config.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	provider.SupportedModels = append([]string(nil), models...)
	config.Providers[ref] = provider
	return writeCommandConfig(path, config, before)
}

func promptModelScope(in, out *os.File, models []string, known bool, current []string) ([]string, bool, error) {
	if known && len(models) > 0 {
		fmt.Fprintln(out, "Discovered models (enter #numbers or exact model IDs):")
		visible := len(models)
		if visible > visibleProviderModels {
			visible = visibleProviderModels
		}
		for i := 0; i < visible; i++ {
			fmt.Fprintf(out, "  %2d. %s\n", i+1, models[i])
		}
		if len(models) > visible {
			fmt.Fprintf(out, "  ... and %d more; enter their exact IDs.\n", len(models)-visible)
		}
	}
	if len(current) == 0 {
		fmt.Fprintln(out, "Current scope: all models")
	} else {
		fmt.Fprintf(out, "Current allowed models: %s\n", strings.Join(current, ", "))
	}
	for {
		fmt.Fprint(out, "Allowed models (#numbers or exact IDs, comma-separated; 'all' allows every model; Enter cancels): ")
		value, err := readTerminalLine(in)
		if err != nil {
			return nil, false, errors.New("could not read model scope")
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return append([]string(nil), current...), false, nil
		}
		if strings.EqualFold(value, "all") {
			return nil, len(current) != 0, nil
		}
		selected, err := parseModelScopeSelection(value, models, known)
		if err != nil {
			fmt.Fprintf(out, "Invalid model selection: %v\n", err)
			continue
		}
		if len(selected) == 0 {
			fmt.Fprintln(out, "Enter at least one model ID, 'all' or Enter to cancel.")
			continue
		}
		return selected, !sameModelScope(selected, current), nil
	}
}

func parseModelScopeSelection(value string, models []string, known bool) ([]string, error) {
	selected := make([]string, 0)
	seen := make(map[string]struct{})
	for _, part := range strings.Split(value, ",") {
		item := strings.TrimSpace(part)
		if item == "" {
			return nil, errors.New("model selection contains an empty entry")
		}
		model := item
		if strings.HasPrefix(item, "#") {
			if !known {
				return nil, errors.New("numbered selection requires a discovered catalog; enter exact model IDs instead")
			}
			index, err := strconv.Atoi(strings.TrimPrefix(item, "#"))
			if err != nil || index < 1 || index > visibleProviderModels || index > len(models) {
				if len(models) == 0 {
					return nil, errors.New("the discovered catalog is empty; enter exact model IDs instead")
				}
				return nil, fmt.Errorf("choose a listed number from #1 to #%d, or enter an exact model ID", min(visibleProviderModels, len(models)))
			}
			model = models[index-1]
		}
		if model == "" || model != strings.TrimSpace(model) || strings.ContainsAny(model, "\r\n\x00") {
			return nil, errors.New("model IDs must be non-empty, trimmed and contain no line breaks")
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		selected = append(selected, model)
	}
	return selected, nil
}

func sameModelScope(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
