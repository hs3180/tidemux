package adapter

import (
	"errors"
	"strings"
)

// CallOptions contains the only client-supplied protocol header forwarded.
// Credentials and API version always come from the gateway configuration.
type CallOptions struct{ AnthropicBeta string }

func (o CallOptions) Validate(protocol string) error {
	if o.AnthropicBeta == "" {
		return nil
	}
	if protocol != "anthropic" || len(o.AnthropicBeta) > 4096 || strings.ContainsAny(o.AnthropicBeta, "\r\n\x00") {
		return errors.New("invalid_beta_header")
	}
	for _, feature := range strings.Split(o.AnthropicBeta, ",") {
		feature = strings.TrimSpace(feature)
		if feature == "" {
			return errors.New("invalid_beta_header")
		}
		for _, c := range feature {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return errors.New("invalid_beta_header")
			}
		}
	}
	return nil
}
