package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/observability"
)

const serveUsage = `usage: tidemux serve [--config PATH]

Start the local gateway. Runtime events are JSON Lines on stderr; human output
is on stdout. Startup failures exit with status 1. Help exits with status 0.

  -config PATH  JSON configuration containing Keychain references
  -h, --help    show this help
`

// Establish the structured event path before anything reads command arguments,
// configuration or credentials. flag's raw diagnostics can contain private
// arguments; discard them rather than attempting to redact arbitrary text.
func serveCommand(args []string, stdout, stderr *os.File) error {
	logger, err := serveRuntimeLogger(stdout, stderr)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() { fmt.Fprint(stdout, serveUsage) }
	path := flags.String("config", defaultConfigPath(), "path to JSON configuration")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return logStartupFailure(logger, stdout, "arguments", "invalid_arguments", err)
	}
	if *path == "" || flags.NArg() != 0 {
		return logStartupFailure(logger, stdout, "arguments", "invalid_arguments", errors.New("invalid serve arguments"))
	}
	config, err := gateway.LoadConfig(*path)
	if err != nil {
		return logStartupFailure(logger, stdout, "config", "config_load_failed", err)
	}
	resolved, err := config.ResolveCredentials(context.Background(), gateway.MacOSKeychain{})
	if err != nil {
		return logStartupFailure(logger, stdout, "credentials", "credential_resolution_failed", err)
	}
	return serveConfigFile(resolved, *path, stdout, stderr)
}

func serveRuntimeLogger(stdout, stderr io.Writer) (*slog.Logger, error) {
	logger, err := observability.NewJSONLogger(stderr, version)
	if err != nil {
		fmt.Fprintln(stdout, "TideMux could not start: logging (runtime_identity_failed). See docs/runtime-logging.md for recovery.")
		return nil, &runtimeLoggedError{err: err}
	}
	return logger, nil
}

// Only callers' fixed code/stage pairs enter the event or the human message.
// Keep the original error in the returned chain for tests/internal diagnosis,
// while runtimeLoggedError prevents main's plain-text fallback from printing it.
func logStartupFailure(logger *slog.Logger, stdout io.Writer, stage, code string, err error) error {
	logger.Error("gateway failed to start",
		slog.Int("schema_version", observability.SchemaVersion),
		slog.String("event", "gateway_start"),
		slog.String("outcome", "error"),
		slog.String("startup_stage", stage),
		slog.String("error_code", code),
	)
	fmt.Fprintf(stdout, "TideMux could not start: %s (%s). See docs/runtime-logging.md for recovery.\n", stage, code)
	return &runtimeLoggedError{err: err}
}
