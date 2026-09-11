package gateway

import "errors"

// Capabilities are supplied by the operator, not inferred from a model name.
// Zero means unknown and is omitted from discovery and client configuration.
type ModelCapabilities struct {
	ContextTokens   int64 `json:"context_tokens,omitempty"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
}

func (c ModelCapabilities) Validate() error {
	if c.ContextTokens < 0 || c.ContextTokens > 1_000_000_000 || c.MaxOutputTokens < 0 || c.MaxOutputTokens > 1_000_000_000 {
		return errors.New("model token capabilities must be 0..1000000000")
	}
	if c.ContextTokens > 0 && c.MaxOutputTokens > c.ContextTokens {
		return errors.New("model output limit exceeds context")
	}
	return nil
}
func (c ModelCapabilities) addToModel(model map[string]any) {
	if c.ContextTokens > 0 {
		model["context_length"] = c.ContextTokens
	}
	if c.MaxOutputTokens > 0 {
		model["max_output_tokens"] = c.MaxOutputTokens
	}
}
