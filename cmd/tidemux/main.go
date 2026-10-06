// TideMux is a local OpenAI- and Anthropic-compatible gateway; it is
// loopback-only unless external listening is explicitly enabled in the
// configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/observability"
)

const (
	version = "0.3.1"
	usage   = `usage: tidemux <command> [options]

commands:
  serve       start the local gateway
  doctor      check the configuration, Keychain and local state directory
  provider    add, inspect, edit and reset budgets for upstream providers
  auto-chain  show, replace or clear the instance provider/model chain
  routing     configure shared-model selection and billing failover
  gateway     configure settings or check the running gateway
  billing     inspect local usage and cost records (--details for requests)
  usage       configure opt-in ccusage logs, inspect status or export committed data
  report      generate, schedule, deliver and retry reports; configure webhooks
              inspect delivery attempts with report deliveries --id N
              use --diagnostics [--json] for recent local rejections
  claude      launch Claude Code through the gateway
  kilo        launch Kilo CLI through the gateway
  hermes      launch Hermes Agent through the gateway
  version     print the TideMux version

common examples:
  tidemux provider add https://api.deepseek.com
  tidemux provider list
  tidemux report webhook --provider lark
  tidemux report schedule --time 09:00 --channel webhook
  tidemux gateway configure --listen loopback
  tidemux gateway check
  tidemux claude --model deepseek-flash
  tidemux serve --config /path/to/config.json

Each provider uses one API protocol and endpoint, with one or more API keys
stored in Keychain. Provider protocol is detected from bounded authenticated
/models schema probes unless explicitly forced.
Set each client's model to an upstream model ID when one configured provider
scope matches, or use PROVIDER/MODEL to choose explicitly when scopes overlap.
The client protocol does not choose the provider. TideMux strips PROVIDER/
from explicitly qualified IDs before forwarding upstream. Configure an ordered
instance auto-chain for model:auto. Existing auto sessions stay pinned; safe
failures advance the preference only for new sessions. Shared-model routing and
billing-exhaustion failover are disabled by default.
Random shared-model routing binds stable X-TideMux-Session-ID (or Anthropic
metadata.user_id) sessions in memory for 24 hours of idle time.
`
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var logged *runtimeLoggedError
		if !errors.As(err, &logged) {
			fmt.Fprintln(os.Stderr, "tidemux:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	if command == "serve" {
		return serveCommand(args[1:], stdout, stderr)
	}
	if command == "claude" || command == "kilo" || command == "hermes" || command == "kilo-ide" {
		return launch(args, stdout, stderr)
	}
	if command == "provider" {
		return providerCommand(args[1:], stdout, stderr)
	}
	if command == "auto-chain" {
		return autoChainCommand(args[1:], stdout, stderr)
	}
	if command == "routing" {
		return routingCommand(args[1:], stdout, stderr)
	}
	if command == "gateway" {
		return gatewayCommand(args[1:], stdout, stderr)
	}
	if command == "billing" {
		return billing(args[1:], stdout, stderr)
	}
	if command == "usage" {
		return usageCommand(args[1:], stdout, stderr)
	}
	if command == "report" {
		return report(args[1:], stdout, stderr)
	}
	if command == "version" {
		if len(args) != 1 {
			return errors.New(usage)
		}
		fmt.Fprintln(stdout, version)
		return nil
	}
	if command != "doctor" {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config containing Keychain references")
	var diagnostics, diagnosticJSON bool
	if command == "doctor" {
		flags.BoolVar(&diagnostics, "diagnostics", false, "show local rejections separately from upstream attempts")
		flags.BoolVar(&diagnosticJSON, "json", false, "output local diagnostics as JSON (requires --diagnostics)")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New(usage)
	}
	if diagnosticJSON && !diagnostics {
		return errors.New("doctor --json requires --diagnostics")
	}
	config, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if command == "doctor" && diagnostics {
		return doctorDiagnostics(config.LedgerPath, diagnosticJSON, stdout)
	}
	if command == "doctor" && len(config.Providers) == 0 && config.BaseURL == "" {
		return errors.New("no upstream provider is configured; run `tidemux provider add`")
	}
	resolved, err := config.ResolveCredentials(context.Background(), gateway.MacOSKeychain{})
	if err != nil {
		return err
	}
	switch command {
	case "doctor":
		if err := checkLedgerParent(resolved.LedgerPath); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "doctor: configuration, Keychain references, and ledger directory are ready")
		return nil
	default:
		return errors.New(usage)
	}
}

func checkLedgerParent(ledgerPath string) error {
	directory := filepath.Dir(ledgerPath)
	if _, err := os.Stat(directory); err != nil {
		return fmt.Errorf("ledger directory is not accessible: %w", err)
	}
	file, err := os.CreateTemp(directory, ".tidemux-doctor-*")
	if err != nil {
		return fmt.Errorf("ledger directory is not writable: %w", err)
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("clean up ledger directory check: %w", err)
	}
	return nil
}

type runtimeLoggedError struct {
	err error
}

func (e *runtimeLoggedError) Error() string { return e.err.Error() }
func (e *runtimeLoggedError) Unwrap() error { return e.err }

func serveConfigFile(config gateway.Config, path string, stdout, stderr *os.File) error {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	return serveWithSignalsUsingConfig(config, stdout, stderr, interrupt, func(server *http.Server, listener net.Listener) error { return server.Serve(listener) }, path)
}

func serve(config gateway.Config, stdout, stderr *os.File) error {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	return serveWithSignals(config, stdout, stderr, interrupt)
}

func serveWithSignals(config gateway.Config, stdout, stderr io.Writer, interrupt <-chan os.Signal) error {
	return serveWithSignalsUsing(config, stdout, stderr, interrupt, func(server *http.Server, listener net.Listener) error {
		return server.Serve(listener)
	})
}

func serveWithSignalsUsing(config gateway.Config, stdout, stderr io.Writer, interrupt <-chan os.Signal, serveServer func(*http.Server, net.Listener) error) error {
	return serveWithSignalsUsingConfig(config, stdout, stderr, interrupt, serveServer, "")
}

func serveWithSignalsUsingConfig(config gateway.Config, stdout, stderr io.Writer, interrupt <-chan os.Signal, serveServer func(*http.Server, net.Listener) error, path string) error {
	logger := observability.JSONLogger(stderr)
	listener, server, closeGateway, err := gateway.OpenWithLogger(config, nil, logger)
	if err != nil {
		stage, code := gateway.StartupFailure(err)
		return logStartupFailure(logger, stdout, stage, code, err)
	}
	if path != "" {
		stopReload := gateway.WatchConfig(server.Handler, path, config, gateway.MacOSKeychain{}, nil)
		previousClose := closeGateway
		closeGateway = func() error { stopReload(); return previousClose() }
	}
	closedGateway := false
	closeGatewayOnce := func() error {
		if closedGateway {
			return nil
		}
		closedGateway = true
		return closeGateway()
	}
	defer func() {
		if err := closeGatewayOnce(); err != nil {
			logger.Error("gateway cleanup failed",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "gateway_shutdown"),
				slog.String("outcome", "error"),
				slog.String("error_code", "cleanup_failed"),
			)
		}
	}()
	host, _, err := net.SplitHostPort(listener.Addr().String())
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		return logStartupFailure(logger, stdout, "listener", "listener_bind_failed", errors.New("gateway did not bind an IP listener"))
	}
	if !ip.IsLoopback() {
		fmt.Fprintf(stdout, "WARNING: external gateway access is enabled on %s; protect the network and gateway token.\n", listener.Addr())
	}
	logger.Info("gateway started",
		slog.Int("schema_version", observability.SchemaVersion),
		slog.String("event", "gateway_start"),
		slog.String("listen_address", listener.Addr().String()),
	)
	fmt.Fprintf(stdout, "TideMux listening on http://%s\n", listener.Addr())
	serveErr := make(chan error, 1)
	go func() { serveErr <- serveServer(server, listener) }()
	shutdown := func() error {
		gateway.BeginShutdown(server)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
	select {
	case receivedSignal := <-interrupt:
		if err := shutdown(); err != nil {
			_ = closeGatewayOnce()
			logger.Error("gateway shutdown failed",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "gateway_shutdown"),
				slog.String("signal", receivedSignal.String()),
				slog.String("outcome", "error"),
				slog.String("error_code", "shutdown_failed"),
			)
			return &runtimeLoggedError{err: fmt.Errorf("shutdown after %s: %w", receivedSignal, err)}
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = closeGatewayOnce()
			logger.Error("gateway server stopped unexpectedly",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "unexpected_server_error"),
				slog.String("error_class", "http_serve"),
			)
			return &runtimeLoggedError{err: err}
		}
		if err := closeGatewayOnce(); err != nil {
			logger.Error("gateway cleanup failed",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "gateway_shutdown"),
				slog.String("signal", receivedSignal.String()),
				slog.String("outcome", "error"),
				slog.String("error_code", "cleanup_failed"),
			)
			return &runtimeLoggedError{err: errors.New("gateway cleanup failed")}
		}
		logger.Info("gateway stopped",
			slog.Int("schema_version", observability.SchemaVersion),
			slog.String("event", "gateway_shutdown"),
			slog.String("signal", receivedSignal.String()),
			slog.String("outcome", "success"),
		)
		return nil
	case err := <-serveErr:
		if shutdownErr := shutdown(); shutdownErr != nil {
			logger.Error("gateway shutdown failed",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "gateway_shutdown"),
				slog.String("outcome", "error"),
				slog.String("error_code", "shutdown_failed"),
			)
			return &runtimeLoggedError{err: shutdownErr}
		}
		if errors.Is(err, http.ErrServerClosed) {
			if closeErr := closeGatewayOnce(); closeErr != nil {
				logger.Error("gateway cleanup failed",
					slog.Int("schema_version", observability.SchemaVersion),
					slog.String("event", "gateway_shutdown"),
					slog.String("outcome", "error"),
					slog.String("error_code", "cleanup_failed"),
				)
				return &runtimeLoggedError{err: errors.New("gateway cleanup failed")}
			}
			logger.Info("gateway stopped",
				slog.Int("schema_version", observability.SchemaVersion),
				slog.String("event", "gateway_shutdown"),
				slog.String("outcome", "success"),
			)
			return nil
		}
		_ = closeGatewayOnce()
		logger.Error("gateway server stopped unexpectedly",
			slog.Int("schema_version", observability.SchemaVersion),
			slog.String("event", "unexpected_server_error"),
			slog.String("error_class", "http_serve"),
		)
		return &runtimeLoggedError{err: err}
	}
}

func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
