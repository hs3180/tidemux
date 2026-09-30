package adapter

import (
	"errors"
	"strings"
)

// ProviderErrorCategory is a closed set of safe, client-facing upstream
// classifications. Provider messages and arbitrary upstream strings are never
// used to infer one of these categories.
type ProviderErrorCategory string

const (
	ProviderErrorInsufficientBalance    ProviderErrorCategory = "insufficient_balance"
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
	case ProviderErrorInsufficientBalance, ProviderErrorRateLimited, ProviderErrorAuthentication, ProviderErrorPermissionDenied, ProviderErrorPolicyDenied, ProviderErrorInvalidRequest, ProviderErrorModelNotFound, ProviderErrorTemporarilyUnavailable:
		return nil
	default:
		return errors.New("category must be insufficient_balance, rate_limited, authentication, permission_denied, policy_denied, invalid_request, model_not_found or temporarily_unavailable")
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
		message = "The provider is temporarily unavailable. TideMux may try another configured route before the response starts."
	default:
		message = "The provider request failed."
	}
	if protocol == "anthropic" {
		switch c {
		case ProviderErrorInsufficientBalance:
			kind = "billing_error"
		case ProviderErrorRateLimited:
			kind = "rate_limit_error"
		case ProviderErrorAuthentication:
			kind = "authentication_error"
		case ProviderErrorPermissionDenied:
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
		case ProviderErrorRateLimited:
			kind = "rate_limit_error"
		case ProviderErrorAuthentication:
			kind = "authentication_error"
		case ProviderErrorPermissionDenied:
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
	case ProviderErrorRateLimited:
		status = 429
	case ProviderErrorAuthentication:
		status = 502
	case ProviderErrorPermissionDenied:
		status = 502
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
