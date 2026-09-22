package adapter

import "strings"

// SessionIDHeader is the stable gateway/provider boundary for a logical
// conversation identifier. Providers may ignore the header, but compatible
// upstreams can use it for their own session affinity or accounting.
const SessionIDHeader = "X-TideMux-Session-ID"

// CallOptions contains client request metadata used for forwarding and local
// accounting. SessionID is forwarded upstream using SessionIDHeader.
// Credentials and API version always come from the gateway configuration.
type CallOptions struct {
	AnthropicBeta string
	SessionID     string
}

func (o CallOptions) Validate(protocol string) error {
	if len(o.SessionID) > 256 || strings.ContainsAny(o.SessionID, "\r\n\x00") {
		return validationError("invalid_session_id", SessionIDHeader)
	}
	if o.AnthropicBeta == "" {
		return nil
	}
	if protocol != "anthropic" || len(o.AnthropicBeta) > 4096 || strings.ContainsAny(o.AnthropicBeta, "\r\n\x00") {
		return validationError("invalid_beta_header", "anthropic-beta")
	}
	for _, feature := range strings.Split(o.AnthropicBeta, ",") {
		feature = strings.TrimSpace(feature)
		if feature == "" {
			return validationError("invalid_beta_header", "anthropic-beta")
		}
		for _, c := range feature {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return validationError("invalid_beta_header", "anthropic-beta")
			}
		}
	}
	return nil
}
