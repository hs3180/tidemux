package adapter

import (
	"encoding/json"
	"errors"
	"strings"
)

func resolveErrorCategory(body []byte, status int, mappings []ProviderErrorMapping) (ProviderErrorCategory, bool) {
	code := providerErrorCode(body)
	if category, ok := resolveProviderErrorCategory(code, status, mappings); ok {
		return category, true
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	for _, value := range []string{code, envelope.Error.Type} {
		switch value {
		case "insufficient_quota":
			return ProviderErrorQuotaExhausted, true
		case "invalid_request_error", "context_length_exceeded", "request_too_large":
			return ProviderErrorInvalidRequest, true
		case "rate_limit_error", "rate_limit_exceeded":
			return ProviderErrorRateLimited, true
		case "authentication_error", "invalid_api_key":
			return ProviderErrorAuthentication, true
		case "permission_error":
			return ProviderErrorPermissionDenied, true
		case "not_found_error", "model_not_found":
			return ProviderErrorModelNotFound, true
		case "billing_error":
			return ProviderErrorInsufficientBalance, true
		case "overloaded_error", "server_error", "api_error", "internal_error", "timeout_error":
			if status == 0 || status >= 500 {
				return ProviderErrorTemporarilyUnavailable, true
			}
		}
	}
	return "", false
}

// ProviderErrorCategory is a closed set of safe, client-facing upstream
// classifications. Provider messages and arbitrary upstream strings are never
// used to infer one of these categories.
type ProviderErrorCategory string

const (
	ProviderErrorInsufficientBalance    ProviderErrorCategory = "insufficient_balance"
	ProviderErrorQuotaExhausted         ProviderErrorCategory = "quota_exhausted"
	ProviderErrorAccessDenied           ProviderErrorCategory = "provider_access_denied"
	ProviderErrorFairUseThrottled       ProviderErrorCategory = "fair_use_throttled"
	ProviderErrorRateLimited            ProviderErrorCategory = "rate_limited"
	ProviderErrorAuthentication         ProviderErrorCategory = "authentication"
	ProviderErrorPermissionDenied       ProviderErrorCategory = "permission_denied"
	ProviderErrorPolicyDenied           ProviderErrorCategory = "policy_denied"
	ProviderErrorInvalidRequest         ProviderErrorCategory = "invalid_request"
	ProviderErrorModelNotFound          ProviderErrorCategory = "model_not_found"
	ProviderErrorTemporarilyUnavailable ProviderErrorCategory = "temporarily_unavailable"
)

// ProviderErrorMapping maps an exact upstream error code, optionally scoped to
// an HTTP status, to one canonical TideMux category.
type ProviderErrorMapping struct {
	UpstreamCode string                `json:"upstream_code"`
	HTTPStatus   int                   `json:"http_status,omitempty"`
	Category     ProviderErrorCategory `json:"category"`
}

func (m ProviderErrorMapping) Validate() error {
	if !safeProviderErrorCode(m.UpstreamCode) {
		return errors.New("upstream_code must contain 1..128 ASCII letters, digits, '.', '_' , ':' or '-'")
	}
	if m.HTTPStatus != 0 && (m.HTTPStatus < 400 || m.HTTPStatus > 599) {
		return errors.New("http_status must be 400..599 when supplied")
	}
	switch m.Category {
	case ProviderErrorInsufficientBalance, ProviderErrorQuotaExhausted, ProviderErrorAccessDenied, ProviderErrorFairUseThrottled, ProviderErrorRateLimited, ProviderErrorAuthentication, ProviderErrorPermissionDenied, ProviderErrorPolicyDenied, ProviderErrorInvalidRequest, ProviderErrorModelNotFound, ProviderErrorTemporarilyUnavailable:
		return nil
	default:
		return errors.New("category must be insufficient_balance, quota_exhausted, provider_access_denied, fair_use_throttled, rate_limited, authentication, permission_denied, policy_denied, invalid_request, model_not_found or temporarily_unavailable")
	}
}

func ValidateProviderErrorMappings(mappings []ProviderErrorMapping) error {
	if len(mappings) > 256 {
		return errors.New("error_code_mappings may contain at most 256 entries")
	}
	for i, mapping := range mappings {
		if err := mapping.Validate(); err != nil {
			return err
		}
		for j := 0; j < i; j++ {
			if mappings[j].UpstreamCode == mapping.UpstreamCode && mappings[j].HTTPStatus == mapping.HTTPStatus {
				return errors.New("error_code_mappings must not contain duplicate upstream_code/http_status pairs")
			}
		}
	}
	return nil
}

func safeProviderErrorCode(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}

func resolveProviderErrorCategory(code string, status int, mappings []ProviderErrorMapping) (ProviderErrorCategory, bool) {
	var fallback ProviderErrorCategory
	for _, mapping := range mappings {
		if mapping.UpstreamCode != code {
			continue
		}
		if mapping.HTTPStatus == status {
			return mapping.Category, true
		}
		if mapping.HTTPStatus == 0 {
			fallback = mapping.Category
		}
	}
	return fallback, fallback != ""
}

// ClientError returns a stable code, safe message, protocol-native error type
// and HTTP status for this canonical category.
func (c ProviderErrorCategory) ClientError(protocol string) (status int, code, message, kind string) {
	switch c {
	case ProviderErrorInsufficientBalance:
		code = "provider_insufficient_balance"
	case ProviderErrorQuotaExhausted:
		code = "provider_quota_exhausted"
	case ProviderErrorAccessDenied:
		code = "provider_access_denied"
	case ProviderErrorFairUseThrottled:
		code = "provider_fair_use_throttled"
	case ProviderErrorRateLimited:
		code = "upstream_rate_limited"
	case ProviderErrorAuthentication:
		code = "upstream_authentication_failed"
	case ProviderErrorPermissionDenied:
		code = "upstream_permission_denied"
	case ProviderErrorPolicyDenied:
		code = "upstream_policy_denied"
	case ProviderErrorInvalidRequest:
		code = "upstream_invalid_request"
	case ProviderErrorModelNotFound:
		code = "provider_model_not_found"
	case ProviderErrorTemporarilyUnavailable:
		code = "provider_temporarily_unavailable"
	default:
		code = "upstream_error"
	}
	switch c {
	case ProviderErrorInsufficientBalance:
		message = "The provider reports insufficient balance. Check its account balance or billing status, then retry."
	case ProviderErrorAccessDenied:
		message = "The provider subscription does not permit the selected model. Check the subscription entitlement or select an eligible alternative provider."
	case ProviderErrorFairUseThrottled:
		message = "The provider restricted this account under its fair use policy. Check account activity; if the unexpected load is not yours, investigate possible API key exposure and rotate the provider key. Request manual removal of the restriction in the provider account center; recovery time is unknown."
	case ProviderErrorQuotaExhausted:
		message = "The provider reports an exhausted quota. Wait for its quota reset or select an eligible alternative provider."
	case ProviderErrorRateLimited:
		message = "The provider is rate limiting requests. Wait for the indicated interval before retrying."
	case ProviderErrorAuthentication:
		message = "The provider rejected its credentials. Check the API key configured for this provider."
	case ProviderErrorPermissionDenied:
		message = "The provider denied access to this model or operation. Check the provider-side permissions."
	case ProviderErrorPolicyDenied:
		message = "The provider blocked this request under its usage or content policy."
	case ProviderErrorInvalidRequest:
		message = "The provider rejected the request as invalid."
	case ProviderErrorModelNotFound:
		message = "The provider does not have access to the selected model."
	case ProviderErrorTemporarilyUnavailable:
		message = "The provider is temporarily unavailable. TideMux may prefer another model:auto entry for new sessions."
	default:
		message = "The provider request failed."
	}
	if protocol == "anthropic" {
		switch c {
		case ProviderErrorInsufficientBalance:
			kind = "billing_error"
		case ProviderErrorRateLimited, ProviderErrorQuotaExhausted, ProviderErrorFairUseThrottled:
			kind = "rate_limit_error"
		case ProviderErrorAuthentication:
			kind = "authentication_error"
		case ProviderErrorPermissionDenied, ProviderErrorAccessDenied:
			kind = "permission_error"
		case ProviderErrorPolicyDenied:
			kind = "permission_error"
		case ProviderErrorInvalidRequest:
			kind = "invalid_request_error"
		case ProviderErrorModelNotFound:
			kind = "not_found_error"
		case ProviderErrorTemporarilyUnavailable:
			kind = "api_error"
		}
	} else {
		switch c {
		case ProviderErrorInsufficientBalance:
			kind = "insufficient_quota"
		case ProviderErrorQuotaExhausted:
			kind = "insufficient_quota"
		case ProviderErrorRateLimited, ProviderErrorFairUseThrottled:
			kind = "rate_limit_error"
		case ProviderErrorAuthentication:
			kind = "authentication_error"
		case ProviderErrorPermissionDenied, ProviderErrorAccessDenied:
			kind = "permission_error"
		case ProviderErrorPolicyDenied:
			kind = "permission_error"
		case ProviderErrorInvalidRequest:
			kind = "invalid_request_error"
		case ProviderErrorModelNotFound:
			kind = "not_found_error"
		case ProviderErrorTemporarilyUnavailable:
			kind = "server_error"
		default:
			kind = "api_error"
		}
	}
	switch c {
	case ProviderErrorInsufficientBalance:
		status = 402
	case ProviderErrorRateLimited, ProviderErrorQuotaExhausted, ProviderErrorFairUseThrottled:
		status = 429
	case ProviderErrorAuthentication:
		status = 502
	case ProviderErrorPermissionDenied:
		status = 502
	case ProviderErrorAccessDenied:
		status = 403
	case ProviderErrorPolicyDenied:
		status = 403
	case ProviderErrorInvalidRequest:
		status = 400
	case ProviderErrorModelNotFound:
		status = 404
	case ProviderErrorTemporarilyUnavailable:
		status = 503
	default:
		status = 502
	}
	return status, code, message, kind
}

// ProviderScopedTerminal marks operator-confirmed deterministic failures that
// pause the whole provider. Automatic recovery may select only a healthy
// alternative on a new shared/auto request; an explicit route stays terminal.
func (c ProviderErrorCategory) ProviderScopedTerminal() bool {
	switch c {
	case ProviderErrorInsufficientBalance, ProviderErrorQuotaExhausted, ProviderErrorAccessDenied, ProviderErrorFairUseThrottled:
		return true
	}
	return false
}
