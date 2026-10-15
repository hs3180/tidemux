// Package diagnostics defines content-free upstream failure metadata.
package diagnostics

import (
	"regexp"
	"strings"
)

var (
	codePattern        = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	numericCodePattern = regexp.MustCompile(`^[0-9]{1,8}$`)
	requestIDPattern   = regexp.MustCompile(`^(req_[A-Za-z0-9_-]{8,80}|[A-Fa-f0-9]{16,64}|[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12})$`)
)

// Failure is shared by the client error, terminal log and existing audit JSON.
// HTTPStatus is the actual upstream HTTP status; ErrorStatus is a status inside
// an SSE error. Neither is the downstream status of an already-open stream.
type Failure struct {
	Source                  string `json:"source"`
	Phase                   string `json:"phase"`
	HTTPStatus              int    `json:"http_status,omitempty"`
	ErrorStatus             int    `json:"error_status,omitempty"`
	ProviderCode            string `json:"provider_code,omitempty"`
	CodeState               string `json:"provider_code_state"`
	ConfiguredCode          bool   `json:"provider_code_configured,omitempty"`
	ProviderRequestID       string `json:"provider_request_id,omitempty"`
	Category                string `json:"category"`
	Retryability            string `json:"retryability"`
	CooldownScope           string `json:"cooldown_scope,omitempty"`
	CooldownSeconds         int64  `json:"cooldown_seconds,omitempty"`
	CooldownUntilMS         int64  `json:"cooldown_until_ms,omitempty"`
	ProviderRateLimitStreak uint16 `json:"provider_rate_limit_streak,omitempty"`
}

// Normalize accepts numeric provider codes, standard protocol error types, and
// exact codes explicitly trusted by the operator's error mappings. Other
// arbitrary provider strings are represented by their presence, never their text.
func (f *Failure) Normalize() *Failure {
	if f == nil {
		return nil
	}
	n := *f
	n.Source = enum(n.Source, "http_error", "stream_error", "transport", "stream", "response", "conversion", "downstream", "canceled")
	n.Phase = enum(n.Phase, "request", "dns", "connect", "tls", "response_headers", "response_body", "stream", "conversion")
	if n.HTTPStatus < 100 || n.HTTPStatus > 599 {
		n.HTTPStatus = 0
	}
	if n.ErrorStatus < 400 || n.ErrorStatus > 599 {
		n.ErrorStatus = 0
	}
	n.CodeState = enum(n.CodeState, "missing", "retained", "redacted", "invalid")
	if n.ProviderCode != "" {
		if codePattern.MatchString(n.ProviderCode) && (n.ConfiguredCode || numericCodePattern.MatchString(n.ProviderCode) || standardCode(n.ProviderCode)) {
			n.CodeState = "retained"
		} else {
			n.ProviderCode, n.CodeState, n.ConfiguredCode = "", "redacted", false
		}
	} else if n.CodeState == "retained" || n.CodeState == "unknown" {
		n.CodeState, n.ConfiguredCode = "missing", false
	}
	if n.ProviderCode == "" {
		n.ConfiguredCode = false
	}
	if !requestIDPattern.MatchString(n.ProviderRequestID) {
		n.ProviderRequestID = ""
	}
	n.Category = enum(n.Category, "insufficient_balance", "rate_limited", "authentication", "permission_denied", "policy_denied", "invalid_request", "model_not_found", "temporarily_unavailable", "transport_error", "invalid_response", "downstream_error", "timeout", "canceled", "upstream_error")
	n.Retryability = enum(n.Retryability, "retryable", "terminal")
	if n.CooldownScope != "" {
		n.CooldownScope = enum(n.CooldownScope, "key", "provider")
	}
	if n.CooldownSeconds < 0 || n.CooldownSeconds > 86400 {
		n.CooldownSeconds = 0
	}
	if n.CooldownUntilMS < 0 || n.CooldownUntilMS > 253402300799999 {
		n.CooldownUntilMS = 0
	}
	return &n
}

// Redact excludes a credential or raw session even if an upstream echoes it in
// a syntactically valid code or request-id field. Values never leave the caller.
func (f *Failure) Redact(values ...string) *Failure {
	n := f.Normalize()
	if n == nil {
		return nil
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		if strings.Contains(n.ProviderCode, value) {
			n.ProviderCode, n.CodeState, n.ConfiguredCode = "", "redacted", false
		}
		if strings.Contains(n.ProviderRequestID, value) {
			n.ProviderRequestID = ""
		}
	}
	return n
}

func standardCode(value string) bool {
	switch value {
	case "api_error", "internal_error", "server_error", "overloaded_error", "rate_limit_error", "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "billing_error", "timeout_error", "insufficient_quota":
		return true
	}
	return false
}

func enum(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return "unknown"
}
