package gateway

import (
	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/diagnostics"
)

func configureProviderErrors(client *adapter.Client, provider Provider, pool *providerKeyPool, config Config) {
	if provider.ForwardErrorMessages != nil {
		enabled := *provider.ForwardErrorMessages
		client.ForwardErrorMessages = &enabled
	}
	if pool != nil {
		client.BackendID = pool.backendID
	}
	client.RedactValues = append([]string{config.AccessToken, config.APIKey}, provider.ResolvedAPIKeys()...)
	for _, p := range config.Providers {
		client.RedactValues = append(client.RedactValues, p.ResolvedAPIKeys()...)
	}
}

func addProviderErrorSemantics(detail map[string]any, callErr *adapter.CallError, protocol string) {
	f := callErr.Failure.Normalize()
	if f == nil || (f.Source != "http_error" && f.Source != "stream_error") || callErr.Code == "audit_failed_do_not_retry_blindly" {
		return
	}
	if f.ProviderMessage != "" {
		detail["message"] = f.ProviderMessage
		if callErr.Category == adapter.ProviderErrorFairUseThrottled {
			_, _, guidance, _ := callErr.Category.ClientError(protocol)
			detail["message"] = f.ProviderMessage + " " + guidance
		}
	}
	if f.ProviderCode != "" {
		detail["provider_code"] = f.ProviderCode
		if !f.ConfiguredCode && callErr.Param == "" && diagnostics.StandardErrorCode(f.ProviderCode) {
			detail["gateway_code"], detail["code"] = detail["code"], f.ProviderCode
		}
	}
	// Category translation takes priority across protocols and over vendor
	// identifiers. These native types retain their more precise meaning.
	if !f.ConfiguredCode && protocol == "anthropic" && (f.ProviderType == "overloaded_error" && callErr.Category == adapter.ProviderErrorTemporarilyUnavailable || f.ProviderType == "request_too_large" && callErr.Category == adapter.ProviderErrorInvalidRequest) {
		detail["type"] = f.ProviderType
	}
	if f.ActualTokens > 0 {
		detail["actual_tokens"], detail["limit_tokens"] = f.ActualTokens, f.LimitTokens
	}
}
