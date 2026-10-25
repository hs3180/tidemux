package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/hs3180/tidemux/internal/observability"
)

// Capabilities come from explicit configuration or a validated model catalog,
// never from the model name. Zero means unknown and is omitted from discovery.
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

// Catalogs belong to immutable config views. They are fetched at startup and on
// connection changes, reused for policy-only reloads, and reset on restart.
type providerCatalog struct {
	models       []string
	known        bool
	capabilities map[string]ModelCapabilities
	warnings     map[string]bool
}

func parseProviderCatalog(body []byte, protocol string) providerCatalog {
	models, known := parseProviderModels(body, protocol)
	catalog := providerCatalog{models: models, known: known, capabilities: map[string]ModelCapabilities{}, warnings: map[string]bool{}}
	if !known {
		return catalog // Never learn limits from an incomplete/unrecognized catalog.
	}
	var envelope struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return catalog
	}
	allowed := make(map[string]struct{}, len(models))
	for _, model := range models {
		allowed[model] = struct{}{}
	}
	for _, item := range envelope.Data {
		var id string
		if json.Unmarshal(item["id"], &id) != nil {
			continue
		}
		id = strings.TrimSpace(id)
		if _, exists := allowed[id]; !exists {
			continue
		}
		contextFields := []string{"context_length", "max_model_len"}
		outputFields := []string{"max_output_tokens"}
		if protocol == "anthropic" {
			contextFields = append(contextFields, "max_input_tokens")
			outputFields = append(outputFields, "max_tokens")
		}
		context, contextBad := catalogTokenLimit(item, contextFields)
		output, outputBad := catalogTokenLimit(item, outputFields)
		caps := ModelCapabilities{ContextTokens: context, MaxOutputTokens: output}
		if caps.Validate() != nil {
			caps, contextBad = ModelCapabilities{}, true
		}
		if before, exists := catalog.capabilities[id]; exists && before != caps {
			// Duplicate IDs with contradictory records are not authoritative.
			caps, contextBad = ModelCapabilities{}, true
		}
		if contextBad || outputBad {
			catalog.warnings[id] = true
		}
		catalog.capabilities[id] = caps
	}
	return catalog
}

func catalogTokenLimit(item map[string]json.RawMessage, fields []string) (int64, bool) {
	var value int64
	for _, field := range fields {
		raw := item[field]
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > 1_000_000_000 {
			return 0, true
		}
		if n == 0 {
			continue
		}
		if value != 0 && value != n {
			return 0, true
		}
		value = n
	}
	return value, false
}

func effectiveModelCapabilities(provider Provider, catalog providerCatalog, model string) (ModelCapabilities, bool) {
	resolved := catalog.capabilities[provider.UpstreamModel(model)]
	declared := provider.ModelCapabilities
	conflict := declared.ContextTokens > 0 && resolved.ContextTokens > 0 && declared.ContextTokens != resolved.ContextTokens ||
		declared.MaxOutputTokens > 0 && resolved.MaxOutputTokens > 0 && declared.MaxOutputTokens != resolved.MaxOutputTokens
	if declared.ContextTokens > 0 {
		resolved.ContextTokens = declared.ContextTokens
	}
	if declared.MaxOutputTokens > 0 {
		resolved.MaxOutputTokens = declared.MaxOutputTokens
	}
	if resolved.Validate() != nil {
		// Preserve the operator's field; do not combine it with a contradictory
		// discovered field or manufacture a replacement limit.
		conflict = true
		if declared.ContextTokens > 0 {
			resolved.MaxOutputTokens = 0
		} else {
			resolved.ContextTokens = 0
		}
	}
	return resolved, conflict
}

func (h *handler) capabilitiesForModel(provider, model string) ModelCapabilities {
	caps, _ := effectiveModelCapabilities(h.providers[provider], h.catalogs[provider], model)
	return caps
}

func providerOutputLimits(provider Provider, catalog providerCatalog) map[string]int64 {
	limits := make(map[string]int64, len(catalog.capabilities))
	for model := range catalog.capabilities {
		caps, _ := effectiveModelCapabilities(provider, catalog, model)
		limits[model] = caps.MaxOutputTokens
	}
	return limits
}

func (h *handler) warnModelCapabilities(name string) {
	catalog := h.catalogs[name]
	models, known := h.modelsForProvider(name)
	unknown := (!known || len(models) == 0) && h.providers[name].ModelCapabilities.ContextTokens == 0
	invalid, conflict := len(catalog.warnings) > 0, false
	for _, model := range models {
		caps, differs := effectiveModelCapabilities(h.providers[name], catalog, model)
		unknown, conflict = unknown || caps.ContextTokens == 0, conflict || differs
	}
	for _, warning := range []struct {
		present bool
		code    string
		message string
	}{
		{invalid, "model_capabilities_invalid", "model catalog has invalid or conflicting limits; affected values remain unknown"},
		{conflict, "model_capabilities_conflict", "configured model limits override conflicting catalog values"},
		{unknown, "model_capabilities_unknown", "model context limits are unknown; configure verified model_capabilities for this provider"},
	} {
		if warning.present {
			h.logger.Warn(warning.message, slog.Int("schema_version", observability.SchemaVersion), slog.String("event", "model_capabilities"), slog.String("provider_ref", name), slog.String("error_code", warning.code))
		}
	}
}
