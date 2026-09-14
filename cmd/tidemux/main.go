// TideMux is a loopback-only, local OpenAI-compatible gateway.
package main

import (
	"context"
	"encoding/json"
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
	"github.com/hs3180/tidemux/internal/ledger"
)

const (
	version = "0.1.1"
	usage   = "usage: tidemux <serve|doctor|ledger> --config <path>\n       tidemux reconcile <report|import> --config <path> [options]\n       tidemux configure --preset deepseek [--protocol anthropic]\n       tidemux <claude|kilo|hermes> [--config path] -- [client arguments]\n       tidemux version\n"
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
	if command == "reconcile" {
		return reconcile(args[1:], stdout, stderr)
	}
	if command == "version" {
		if len(args) != 1 {
			return errors.New(usage)
		}
		fmt.Fprintln(stdout, version)
		return nil
	}
	if command != "serve" && command != "doctor" && command != "ledger" {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config containing Keychain references")
	var diagnostics bool
	if command == "ledger" {
		flags.BoolVar(&diagnostics, "diagnostics", false, "show local rejections separately from upstream attempts")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New(usage)
	}
	config, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if command == "ledger" {
		store, err := ledger.Open(config.LedgerPath)
		if err != nil {
			return errors.New("cannot open ledger")
		}
		defer store.Close()
		if diagnostics {
			rows, err := store.RecentDiagnostics(context.Background(), 100)
			if err != nil {
				return errors.New("cannot read local diagnostics")
			}
			return json.NewEncoder(stdout).Encode(rows)
		}
		rows, err := store.Recent(context.Background(), 100)
		if err != nil {
			return errors.New("cannot read ledger")
		}
		return json.NewEncoder(stdout).Encode(rows)
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
	if host, _, err := net.SplitHostPort(listener.Addr().String()); err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("gateway did not bind a loopback listener")
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
