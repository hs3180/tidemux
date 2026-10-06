package gateway

import (
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/hs3180/tidemux/internal/ledger"
)

func TestOpenClassifiesStartupStageWithoutChangingErrorText(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, test := range []struct {
		name, stage, code string
		modify            func(*Config)
	}{
		{"invalid configuration", "config", "config_load_failed", func(c *Config) { c.MaxInFlight = 0 }},
		{"unresolved credentials", "credentials", "credential_resolution_failed", func(c *Config) { c.AccessToken = "" }},
		{"missing provider", "providers", "provider_initialization_failed", func(c *Config) {
			c.Protocol, c.APIVersion, c.BaseURL, c.Model, c.UpstreamID, c.APIKey = "", "", "", "", "", ""
			c.UpstreamKeychain = KeychainReference{}
		}},
		{"ledger directory missing", "ledger", "ledger_initialization_failed", func(c *Config) { c.LedgerPath = filepath.Join(c.LedgerPath, "private-path", "ledger.db") }},
		{"listener occupied", "listener", "listener_bind_failed", func(c *Config) { c.ListenAddr = listener.Addr().String() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(filepath.Join(t.TempDir(), "ledger.db"), "http://127.0.0.1:1/v1")
			config.ListenAddr = "127.0.0.1:0"
			test.modify(&config)
			_, _, closeGateway, err := Open(config, nil)
			if closeGateway != nil {
				_ = closeGateway()
			}
			if err == nil {
				t.Fatal("expected initialization to fail")
			}
			stage, code := StartupFailure(err)
			if stage != test.stage || code != test.code {
				t.Fatalf("unexpected classification %s/%s: %v", stage, code, err)
			}
			var failure *startupError
			if !errors.As(err, &failure) || failure.Error() != errors.Unwrap(failure).Error() {
				t.Fatalf("original non-serve error behavior changed: %v", err)
			}
			if test.stage == "listener" {
				// Bind failure must release the ledger owner acquired before net.Listen.
				l, err := ledger.OpenForGateway(config.LedgerPath)
				if err != nil {
					t.Fatalf("failed startup retained ledger ownership: %v", err)
				}
				_ = l.Close()
			}
		})
	}
}

func TestStartupFailureNeverUsesUnknownErrorTextAsClassification(t *testing.T) {
	stage, code := StartupFailure(errors.New("private-path/secret=arbitrary-input"))
	if stage != "gateway" || code != "gateway_initialization_failed" {
		t.Fatalf("unexpected unknown-error fallback: %s/%s", stage, code)
	}
}
