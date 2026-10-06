package gateway

import "errors"

// startupError attaches an internal stage without changing the error text used
// by non-serve callers. Its cause is never copied into a runtime event.
type startupError struct {
	stage string
	err   error
}

func (e *startupError) Error() string { return e.err.Error() }
func (e *startupError) Unwrap() error { return e.err }

func startupFailure(stage string, err error) error {
	return &startupError{stage: stage, err: err}
}

// StartupFailure returns only fixed stage/code pairs for gateway initialization
// failures. Unknown errors receive a bounded fallback; error text is not parsed.
func StartupFailure(err error) (stage, code string) {
	var failure *startupError
	if errors.As(err, &failure) {
		switch failure.stage {
		case "config":
			return "config", "config_load_failed"
		case "credentials":
			return "credentials", "credential_resolution_failed"
		case "providers":
			return "providers", "provider_initialization_failed"
		case "ledger":
			return "ledger", "ledger_initialization_failed"
		case "listener":
			return "listener", "listener_bind_failed"
		}
	}
	return "gateway", "gateway_initialization_failed"
}
