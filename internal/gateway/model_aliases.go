package gateway

import (
	"errors"
	"strings"
)

// UpstreamModel resolves one provider-local alias. Configuration validation
// forbids chains, so there is no recursive resolution or cross-provider route.
func (p Provider) UpstreamModel(model string) string {
	if upstream, ok := p.ModelAliases[model]; ok {
		return upstream
	}
	return model
}

func validAliasModelID(model string) bool {
	return model != "auto" && safeAvailabilityModel(model)
}

func (p Provider) validateModelAliases() error {
	if len(p.ModelAliases) > 128 {
		return errors.New("at most 128 aliases are allowed")
	}
	for alias, upstream := range p.ModelAliases {
		if !validAliasModelID(alias) || strings.Contains(alias, "/") || !validAliasModelID(upstream) {
			return errors.New("use model labels of at most 128 ASCII bytes with letters, numbers or ._/-:@; aliases must be unqualified and neither name may be auto")
		}
		if _, chained := p.ModelAliases[upstream]; chained {
			return errors.New("alias targets must be upstream IDs, not another alias or the alias itself")
		}
	}
	return nil
}

// Availability is keyed by the dispatched model. Aliases of the same target
// share failures, while changing a target cannot inherit the old model's health.
func (h *handler) upstreamScopeAllows(provider, model string) bool {
	p, ok := h.providers[provider]
	if !ok {
		return false
	}
	if p.UpstreamModel(model) == model && h.supportScopeAllows(provider, model) {
		return true
	}
	for alias, upstream := range p.ModelAliases {
		if upstream == model && h.supportScopeAllows(provider, alias) {
			return true
		}
	}
	return false
}
