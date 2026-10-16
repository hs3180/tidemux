package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func runtimeTestConfig(dir string) gateway.Config {
	return gateway.Config{
		ListenAddr: "127.0.0.1:0", Protocol: "openai", BaseURL: "http://127.0.0.1:1/v1",
		Model: "test-model", UpstreamID: "test-provider", APIKey: "provider-secret",
		ModelCapabilities: gateway.ModelCapabilities{ContextTokens: 65536, MaxOutputTokens: 8192},
		AccessToken:       "local-secret", MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"),
		UpstreamKeychain:    gateway.KeychainReference{Service: "test.provider", Account: "default"},
		AccessTokenKeychain: gateway.KeychainReference{Service: "test.gateway", Account: "default"},
	}
}

func TestServeSeparatesHumanStdoutAndJSONLifecycleLogs(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()
	stderr := &bytes.Buffer{}
	config := runtimeTestConfig(t.TempDir())
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveWithSignals(config, stdoutWriter, stderr, signals)
		_ = stdoutWriter.Close()
	}()
	line, err := bufio.NewReader(stdoutReader).ReadString('\n')
	if err != nil {
		t.Fatalf("read human startup line: %v", err)
	}
	if !strings.HasPrefix(line, "TideMux listening on http://127.0.0.1:") || strings.HasPrefix(line, "{") {
		t.Fatalf("unexpected CLI stdout: %q", line)
	}
	signals <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var events []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(stderr.Bytes()))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("stderr was not JSON Lines: %v: %s", err, scanner.Text())
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0]["event"] != "gateway_start" || events[1]["event"] != "gateway_shutdown" || events[1]["outcome"] != "success" {
		t.Fatalf("unexpected lifecycle events: %#v", events)
	}
	if strings.Contains(stderr.String(), "provider-secret") || strings.Contains(stderr.String(), "local-secret") {
		t.Fatalf("lifecycle logs exposed credentials: %s", stderr.String())
	}
}

func TestServeUnexpectedServerErrorIsStructuredAndRedacted(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := serveWithSignalsUsing(runtimeTestConfig(t.TempDir()), &stdout, &stderr, make(chan os.Signal), func(*http.Server, net.Listener) error {
		return errors.New("private-listener-error-marker")
	})
	var logged *runtimeLoggedError
	if !errors.As(err, &logged) {
		t.Fatalf("serve error=%v; want a logged runtime error", err)
	}
	var events []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(stderr.Bytes()))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("stderr was not JSON Lines: %v: %s", err, scanner.Text())
		}
		events = append(events, event)
	}
	if len(events) != 2 || events[0]["event"] != "gateway_start" || events[1]["event"] != "unexpected_server_error" {
		t.Fatalf("unexpected server failure events: %#v", events)
	}
	if strings.Contains(stderr.String(), "private-listener-error-marker") || strings.Contains(stdout.String(), "private-listener-error-marker") {
		t.Fatalf("raw server error escaped into output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "TideMux listening on http://127.0.0.1:") {
		t.Fatalf("unexpected CLI stdout: %q", stdout.String())
	}
}

func TestServeStartupErrorIsStructuredAndHumanReadable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	config := runtimeTestConfig(t.TempDir())
	config.MaxInFlight = -1
	err := serveWithSignalsUsing(config, &stdout, &stderr, make(chan os.Signal), func(*http.Server, net.Listener) error {
		t.Fatal("server should not start when the configuration is invalid")
		return nil
	})
	var logged *runtimeLoggedError
	if !errors.As(err, &logged) {
		t.Fatalf("serve error=%v; want a logged runtime error", err)
	}
	if !strings.Contains(stdout.String(), "TideMux could not start:") || strings.Contains(stdout.String(), "provider-secret") {
		t.Fatalf("startup failure is not a safe human-readable message: %q", stdout.String())
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &event); err != nil {
		t.Fatalf("stderr was not one JSON lifecycle event: %v: %s", err, stderr.String())
	}
	if event["event"] != "gateway_start" || event["outcome"] != "error" || event["error_code"] != "config_load_failed" || event["startup_stage"] != "config" {
		t.Fatalf("unexpected startup failure event: %#v", event)
	}
	if strings.Contains(stderr.String(), "max_in_flight") || strings.Contains(stderr.String(), "provider-secret") {
		t.Fatalf("startup error details leaked to runtime logs: %s", stderr.String())
	}
}
