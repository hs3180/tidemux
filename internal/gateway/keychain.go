package gateway

import (
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// MacOSKeychain retrieves generic passwords without ever placing their values
// in a command line, log message, or configuration file.
type MacOSKeychain struct{ path string }

func (k MacOSKeychain) Lookup(ctx context.Context, reference KeychainReference) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("macOS Keychain is required (running on %s)", runtime.GOOS)
	}
	if err := reference.Validate("keychain reference"); err != nil {
		return "", err
	}
	args := []string{"find-generic-password", "-s", reference.Service, "-a", reference.Account, "-w"}
	if k.path != "" {
		args = append(args, k.path)
	}
	output, err := exec.CommandContext(ctx, "security", args...).Output()
	if err != nil {
		// Do not return command output: Keychain diagnostics must never reveal a secret.
		return "", fmt.Errorf("Keychain item not found or unavailable")
	}
	return strings.TrimSpace(string(output)), nil
}

// StoreNew sends the secret as hex over security's stdin command channel.
// The password never enters argv, shell history, a temporary file, or output.
// Existing items are not updated. Callers use unique accounts and roll back new items.
func (k MacOSKeychain) StoreNew(ctx context.Context, reference KeychainReference, value string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("macOS Keychain is required")
	}
	if err := reference.Validate("keychain reference"); err != nil {
		return err
	}
	for _, label := range []string{reference.Service, reference.Account} {
		if strings.ContainsAny(label, "\r\n\x00") {
			return fmt.Errorf("invalid Keychain reference")
		}
	}
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("invalid credential")
	}
	command := exec.CommandContext(ctx, "security", "-i")
	line := "add-generic-password -s " + strconv.Quote(reference.Service) + " -a " + strconv.Quote(reference.Account) + " -X " + hex.EncodeToString([]byte(value))
	if k.path != "" {
		line += " " + strconv.Quote(k.path)
	}
	command.Stdin = strings.NewReader(line + "\n")
	// Interactive security may return zero even if its command failed. Verify by lookup.
	if _, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("cannot write Keychain item")
	}
	actual, err := k.Lookup(ctx, reference)
	if err != nil || actual != value {
		return fmt.Errorf("Keychain write could not be verified")
	}
	return nil
}
func (k MacOSKeychain) Delete(ctx context.Context, reference KeychainReference) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("macOS Keychain is required")
	}
	args := []string{"delete-generic-password", "-s", reference.Service, "-a", reference.Account}
	if k.path != "" {
		args = append(args, k.path)
	}
	if err := exec.CommandContext(ctx, "security", args...).Run(); err != nil {
		return fmt.Errorf("cannot remove newly created Keychain item")
	}
	return nil
}
