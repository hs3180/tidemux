// Package diagnostics defines bounded upstream failure metadata.
package diagnostics

import (
	"regexp"
	"strings"
)

var (
	codePattern        = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	numericCodePattern = regexp.MustCompile(`^[0-9]{1,8}$`)
	requestIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	keyLabelPattern    = regexp.MustCompile(`^key-([a-f0-9]{32}|local-[a-f0-9]+)$`)
)

// Failure is shared by the client error, terminal log and existing audit JSON.
// HTTPStatus is the actual upstream HTTP status; ErrorStatus is a status inside
// an SSE error. Neither is the downstream status of an already-open stream.
type Failure struct {
	Source                  string        `json:"source"`
	Phase                   string        `json:"phase"`
	HTTPStatus              int           `json:"http_status,omitempty"`
	ErrorStatus             int           `json:"error_status,omitempty"`
	ProviderCode            string        `json:"provider_code,omitempty"`
	CodeState               string        `json:"provider_code_state"`
	ConfiguredCode          bool          `json:"provider_code_configured,omitempty"`
	ProviderRequestID       string        `json:"provider_request_id,omitempty"`
	ProviderType            string        `json:"provider_type,omitempty"`
	ProviderMessage         string        `json:"provider_message,omitempty"`
	ActualTokens            int64         `json:"actual_tokens,omitempty"`
	LimitTokens             int64         `json:"limit_tokens,omitempty"`
	KeyLabel                string        `json:"key_label,omitempty"`
	BackendID               string        `json:"backend_id,omitempty"`
	Category                string        `json:"category"`
	Retryability            string        `json:"retryability"`
	CooldownScope           string        `json:"cooldown_scope,omitempty"`
	CooldownSeconds         int64         `json:"cooldown_seconds,omitempty"`
	CooldownUntilMS         int64         `json:"cooldown_until_ms,omitempty"`
	ProviderRateLimitStreak uint16        `json:"provider_rate_limit_streak,omitempty"`
	ResponseRetryHeaders    *RetryHeaders `json:"response_retry_headers,omitempty"`
}

// RetryHeaders is a generated downstream response snapshot, never a copy of
// arbitrary provider headers. An empty object means no retry headers were sent
// (including failures after SSE headers were committed).
type RetryHeaders struct {
	ShouldRetry       string `json:"x_should_retry,omitempty"`
	RetryAfterSeconds *int64 `json:"retry_after_seconds,omitempty"`
	RetryAfterMS      *int64 `json:"retry_after_ms,omitempty"`
}

func (h *RetryHeaders) Normalize() *RetryHeaders {
	if h == nil {
		return nil
	}
	n := &RetryHeaders{}
	if h.ShouldRetry == "true" || h.ShouldRetry == "false" {
		n.ShouldRetry = h.ShouldRetry
	}
	if h.RetryAfterSeconds != nil && *h.RetryAfterSeconds > 0 && *h.RetryAfterSeconds <= 30 {
		seconds := *h.RetryAfterSeconds
		n.RetryAfterSeconds = &seconds
	}
	if h.RetryAfterMS != nil && n.RetryAfterSeconds != nil && *h.RetryAfterMS == *n.RetryAfterSeconds*1000 {
		milliseconds := *h.RetryAfterMS
		n.RetryAfterMS = &milliseconds
	}
	if n.ShouldRetry == "false" {
		n.RetryAfterSeconds, n.RetryAfterMS = nil, nil
	}
	return n
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
		if codePattern.MatchString(n.ProviderCode) && (n.ConfiguredCode || numericCodePattern.MatchString(n.ProviderCode) || StandardErrorCode(n.ProviderCode)) {
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
	if !StandardErrorCode(n.ProviderType) {
		n.ProviderType = ""
	}
	if credentialShaped(n.ProviderCode) {
		n.ProviderCode, n.CodeState, n.ConfiguredCode = "", "redacted", false
	}
	if strings.HasPrefix(strings.ToLower(n.ProviderRequestID), "sk-") || credentialPatterns[3].MatchString(n.ProviderRequestID) {
		n.ProviderRequestID = ""
	}
	n.ProviderMessage = cleanMessage(n.ProviderMessage)
	if n.ActualTokens <= n.LimitTokens || n.LimitTokens <= 0 {
		n.ActualTokens, n.LimitTokens = 0, 0
	}
	n.KeyLabel, n.BackendID = KeyLabel(n.KeyLabel), KeyLabel(n.BackendID)
	n.Category = enum(n.Category, "insufficient_balance", "quota_exhausted", "rate_limited", "authentication", "permission_denied", "policy_denied", "invalid_request", "model_not_found", "temporarily_unavailable", "transport_error", "invalid_response", "downstream_error", "timeout", "canceled", "upstream_error")
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
	n.ResponseRetryHeaders = n.ResponseRetryHeaders.Normalize()
	return &n
}

// Redact excludes a credential or raw session even if an upstream echoes it in
// a syntactically valid code or request-id field. Values never leave the caller.
func (f *Failure) Redact(values ...string) *Failure {
	if f == nil {
		return nil
	}
	n := *f
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
		if strings.Contains(n.ProviderType, value) {
			n.ProviderType = ""
		}
		n.ProviderMessage = strings.ReplaceAll(n.ProviderMessage, value, "[redacted]")
	}
	n.ProviderMessage = redactMessageShapes(n.ProviderMessage)
	n.ActualTokens, n.LimitTokens = TokenFacts(n.ProviderMessage)
	return n.Normalize()
}

// KeyLabel accepts only independently assigned labels, never secret hashes.
func KeyLabel(value string) string {
	if keyLabelPattern.MatchString(value) {
		return value
	}
	return ""
}

// StandardErrorCode reports an explicitly recognized API identifier.
func StandardErrorCode(value string) bool {
	switch value {
	case "api_error", "internal_error", "server_error", "overloaded_error", "rate_limit_error", "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "billing_error", "timeout_error", "insufficient_quota", "context_length_exceeded", "rate_limit_exceeded", "invalid_api_key", "model_not_found", "request_too_large":
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
