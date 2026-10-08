package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeCommandLogsBeforeParsingAndLoading(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "private-path-marker", "missing.json")
	malformed := filepath.Join(root, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`{"private-content-marker":`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, stage, code string
		args              []string
	}{
		{"unknown flag", "arguments", "invalid_arguments", []string{"--private-argument-marker\ninjected"}},
		{"missing value", "arguments", "invalid_arguments", []string{"--config"}},
		{"empty path", "arguments", "invalid_arguments", []string{"--config="}},
		{"positional value", "arguments", "invalid_arguments", []string{"private-argument-marker"}},
		{"missing config", "config", "config_load_failed", []string{"--config", missing}},
		{"malformed config", "config", "config_load_failed", []string{"--config", malformed}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout, err := os.CreateTemp(root, "stdout-*")
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			stderr, err := os.CreateTemp(root, "stderr-*")
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			err = run(append([]string{"serve"}, test.args...), stdout, stderr)
			var logged *runtimeLoggedError
			if !errors.As(err, &logged) {
				t.Fatalf("expected a logged failure, got %v", err)
			}
			data, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err := json.Unmarshal(data, &event); err != nil {
				t.Fatalf("expected exactly one JSON event: %v: %s", err, data)
			}
			if event["event"] != "gateway_start" || event["outcome"] != "error" || event["startup_stage"] != test.stage || event["error_code"] != test.code {
				t.Fatalf("unexpected failure classification: %#v", event)
			}
			human, err := os.ReadFile(stdout.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, marker := range []string{root, "private-path-marker", "private-content-marker", "private-argument-marker", "injected"} {
				if strings.Contains(string(data)+string(human), marker) {
					t.Fatalf("private startup input reached output: %q", marker)
				}
			}
			if !strings.Contains(string(human), "docs/runtime-logging.md") {
				t.Fatalf("missing safe recovery guidance: %s", human)
			}
		})
	}
}

func TestServeHelpSucceedsOnStdoutWithoutReadingConfig(t *testing.T) {
	for _, option := range []string{"-h", "--help"} {
		t.Run(option, func(t *testing.T) {
			root := t.TempDir()
			stdout, err := os.CreateTemp(root, "stdout-*")
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			stderr, err := os.CreateTemp(root, "stderr-*")
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			if err := run([]string{"serve", "--config", filepath.Join(root, "private-missing-config"), option}, stdout, stderr); err != nil {
				t.Fatalf("help must not read config: %v", err)
			}
			human, _ := os.ReadFile(stdout.Name())
			machine, _ := os.ReadFile(stderr.Name())
			if string(human) != serveUsage || len(machine) != 0 || strings.Contains(string(human), root) {
				t.Fatalf("unexpected help stdout=%q stderr=%q", human, machine)
			}
		})
	}
}
