package gateway

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// MacOSKeychain retrieves generic passwords without ever placing their values
// in a command line, log message, or configuration file.
type MacOSKeychain struct{}

func (MacOSKeychain) Lookup(ctx context.Context, reference KeychainReference) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("macOS Keychain is required (running on %s)", runtime.GOOS)
	}
	if err := reference.Validate("keychain reference"); err != nil {
		return "", err
	}
	output, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", reference.Service, "-a", reference.Account, "-w").Output()
	if err != nil {
		// Do not return command output: Keychain diagnostics must never reveal a secret.
		return "", fmt.Errorf("Keychain item not found or unavailable")
	}
	return strings.TrimSpace(string(output)), nil
}
