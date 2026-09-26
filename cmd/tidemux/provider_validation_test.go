package main

import (
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestValidateProviderKeychainGroupChecksAvailabilityAndUniqueness(t *testing.T) {
	gatewayRef := gateway.KeychainReference{Service: "gateway", Account: "local"}
	first := gateway.KeychainReference{Service: "provider", Account: "first"}
	second := gateway.KeychainReference{Service: "provider", Account: "second"}
	provider := gateway.Provider{UpstreamKeychains: []gateway.KeychainReference{first, second}}
	validStore := func() *memorySecrets {
		return &memorySecrets{values: map[string]string{
			"gateway/local":   "gateway-secret",
			"provider/first":  "provider-secret-one",
			"provider/second": "provider-secret-two",
		}}
	}

	store := validStore()
	if count, err := validateProviderKeychainGroup(provider, gatewayRef, store); err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}

	tests := []struct {
		name      string
		store     *memorySecrets
		provider  gateway.Provider
		wantError string
	}{
		{name: "missing gateway key", store: &memorySecrets{values: map[string]string{}}, provider: provider, wantError: "gateway Keychain item unavailable"},
		{name: "missing provider key", store: &memorySecrets{values: map[string]string{"gateway/local": "gateway-secret", "provider/first": "provider-secret-one"}}, provider: provider, wantError: "provider Keychain item unavailable"},
		{name: "gateway key reused", store: &memorySecrets{values: map[string]string{"gateway/local": "same-secret", "provider/first": "same-secret", "provider/second": "provider-secret-two"}}, provider: provider, wantError: "must differ"},
		{name: "duplicate group secret", store: &memorySecrets{values: map[string]string{"gateway/local": "gateway-secret", "provider/first": "duplicate-secret", "provider/second": "duplicate-secret"}}, provider: provider, wantError: "must be unique"},
		{name: "invalid secret", store: &memorySecrets{values: map[string]string{"gateway/local": "gateway-secret", "provider/first": "provider-secret\n", "provider/second": "provider-secret-two"}}, provider: provider, wantError: "unavailable or invalid"},
		{name: "duplicate reference", store: validStore(), provider: gateway.Provider{UpstreamKeychains: []gateway.KeychainReference{first, first}}, wantError: "references are invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateProviderKeychainGroup(test.provider, gatewayRef, test.store)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error=%v, want substring %q", err, test.wantError)
			}
			for _, secret := range []string{"gateway-secret", "provider-secret", "duplicate-secret", "same-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("validation error disclosed secret %q: %v", secret, err)
				}
			}
		})
	}
	if _, err := validateProviderKeychainGroup(provider, gatewayRef, nil); err == nil || !strings.Contains(err.Error(), "Keychain access is required") {
		t.Fatalf("nil Keychain lookup error=%v", err)
	}
}
