package gateway

import (
	"errors"
	"strings"
)

const maxAutoChainEntries = 64

// AutoChainEntry names one explicit provider/model pair in the instance chain.
type AutoChainEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (c Config) validateAutoChain() error {
	if len(c.AutoChain) > maxAutoChainEntries {
		return errors.New("auto_chain may contain at most 64 provider/model entries")
	}
	seen := make(map[AutoChainEntry]struct{}, len(c.AutoChain))
	for _, entry := range c.AutoChain {
		provider, exists := c.Providers[entry.Provider]
		if !exists {
			return errors.New("auto_chain entries must reference configured named providers")
		}
		if strings.TrimSpace(entry.Model) == "" || entry.Model != strings.TrimSpace(entry.Model) || strings.EqualFold(entry.Model, "auto") || strings.ContainsAny(entry.Model, "\r\n\x00") {
			return errors.New("auto_chain entries require explicit non-empty model IDs other than auto")
		}
		if len(provider.SupportedModels) > 0 && !containsModel(provider.SupportedModels, entry.Model) {
			return errors.New("auto_chain models must be allowed by their provider's supported_models")
		}
		if _, exists := seen[entry]; exists {
			return errors.New("auto_chain must not contain duplicate provider/model entries")
		}
		seen[entry] = struct{}{}
	}
	return nil
}
