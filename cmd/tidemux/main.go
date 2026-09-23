// TideMux is a local OpenAI- and Anthropic-compatible gateway; it is
// loopback-only unless external listening is explicitly enabled in the
// configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
)

const (
	version = "0.1.1"
	usage   = `usage: tidemux <command> [options]

commands:
  serve       start the local gateway
  doctor      check the configuration, Keychain and local state directory
  configure   create or replace a provider configuration
  billing     inspect local usage and cost records (--details for requests)
  budget      set the rolling budget limits
  report      generate, view or deliver usage reports
              use --diagnostics [--json] for recent local rejections
  claude      launch Claude Code through the gateway
  kilo        launch Kilo CLI through the gateway
  hermes      launch Hermes Agent through the gateway
  version     print the TideMux version

configuration examples:
  tidemux configure --preset deepseek-flash [options]
  tidemux configure --base-url URL --model ID [pricing options]
  tidemux configure --openai-base-url URL --anthropic-base-url URL --model ID
  tidemux configure --openai-base-url URL --openai-model ID --anthropic-base-url URL --anthropic-model ID

common examples:
  tidemux configure --preset deepseek-flash
  tidemux configure --help
  tidemux serve --config /path/to/config.json

The provider protocol is detected automatically from the configured API root.
Both OpenAI Chat Completions and Anthropic Messages client routes are available.
`
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "tidemux:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	if command == "claude" || command == "kilo" || command == "hermes" || command == "kilo-ide" {
		return launch(args, stdout, stderr)
	}
	if command == "configure" {
		return configure(args[1:], stdout, stderr)
	}
	if command == "billing" {
		return billing(args[1:], stdout, stderr)
	}
	if command == "budget" {
		return budgetCommand(args[1:], os.Stdin, stdout, stderr)
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
	if command != "serve" && command != "doctor" {
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
	case "serve":
		return serve(resolved, stdout)
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

func serve(config gateway.Config, stdout *os.File) error {
	listener, server, closeGateway, err := gateway.Open(config, nil)
	if err != nil {
		return err
	}
	defer closeGateway()
	host, _, err := net.SplitHostPort(listener.Addr().String())
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		return errors.New("gateway did not bind an IP listener")
	}
	if !ip.IsLoopback() {
		fmt.Fprintf(stdout, "WARNING: external gateway access is enabled on %s; protect the network and gateway token.\n", listener.Addr())
	}
	fmt.Fprintf(stdout, "TideMux listening on http://%s\n", listener.Addr())
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case signal := <-interrupt:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			server.Close()
			return fmt.Errorf("shutdown after %s: %w", signal, err)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
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
