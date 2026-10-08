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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func runtimeTestConfig(dir string) gateway.Config {
	return gateway.Config{
		ListenAddr: "127.0.0.1:0", Protocol: "openai", BaseURL: "http://127.0.0.1:1/v1",
		Model: "test-model", UpstreamID: "test-provider", APIKey: "provider-secret",
		AccessToken: "local-secret", MaxInFlight: 1, LedgerPath: filepath.Join(dir, "ledger.db"),
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
	config.MaxInFlight = 0
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

func TestDoctorSmokeFromCleanDirectory(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the MVP Keychain implementation is macOS-only")
	}
	temp := t.TempDir()
	binary := filepath.Join(temp, "tidemux")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	configPath := filepath.Join(temp, "config.json")
	config := map[string]any{
		"protocol": "openai", "base_url": "https://example.com/v1", "model": "m", "upstream_id": "test",
		"listen_addr":           "127.0.0.1:8787",
		"upstream_keychain":     map[string]string{"service": "test.deepseek", "account": "default"},
		"access_token_keychain": map[string]string{"service": "test.gateway", "account": "default"},
		"max_in_flight":         1, "ledger_path": filepath.Join(temp, "ledger.db"),
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(temp, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	security := filepath.Join(fakeBin, "security")
	if err := os.WriteFile(security, []byte("#!/bin/sh\ncase \"$*\" in *test.gateway*) printf 'local-secret\\n';; *) printf 'provider-secret\\n';; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "doctor", "--config", configPath)
	command.Dir = temp
	command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "doctor: configuration, Keychain references, and ledger directory are ready") {
		t.Fatalf("unexpected doctor output: %s", output)
	}
}
